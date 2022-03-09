package controller

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"text/template"

	"github.com/openshift/cluster-dns-operator/pkg/manifests"

	"github.com/sirupsen/logrus"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	operatorv1 "github.com/openshift/api/operator/v1"

	corev1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const resolvConf = "/etc/resolv.conf"
const defaultDNSPort = 53

var errInvalidNetworkUpstream = fmt.Errorf("The address field is mandatory for upstream of type Network, but was not provided")
var errTransportTLSConfiguredWithoutServerName = fmt.Errorf("The ServerName field is mandatory when configuring tls as the DNS Transport")
var corefileTemplate = template.Must(template.New("Corefile").Funcs(template.FuncMap{
	"CoreDNSForwardingPolicy": coreDNSPolicy, "UpstreamResolver": coreDNSResolver,
}).Parse(`{{range .Servers -}}
# {{.Name}}
{{range .Zones}}{{.}}:5353 {{end}}{
    {{with .ForwardPlugin -}}
    prometheus 127.0.0.1:9153
    forward .{{range .Upstreams}} {{.}}{{end}} {
        {{- if ne .ServerName "" }}
        tls_servername {{.ServerName}}
		{{- if ne .CABundle.Name "" -}}
        tls /etc/pki/{{.ServerName}}/caBundle.crt
		{{- else}}
        tls
		{{- end}}
        {{- end}}
        policy {{ CoreDNSForwardingPolicy .Policy }}
    }
    {{- end}}
    errors
    log . {
        {{$.LogLevel}}
    }
    bufsize 512
    cache 900 {
        denial 9984 30
    }
}
{{end -}}
.:5353 {
    bufsize 512
    errors
    log . {
        {{.LogLevel}}
    }
    health {
        lameduck 20s
    }
    ready
    kubernetes {{.ClusterDomain}} in-addr.arpa ip6.arpa {
        pods insecure
        fallthrough in-addr.arpa ip6.arpa
    }
    prometheus 127.0.0.1:9153
	{{- with .UpstreamResolvers }}
    forward .{{range .Upstreams}} {{UpstreamResolver . $.UpstreamResolvers.Transport}}{{end}} {
        {{- if ne .ServerName "" }}
        tls_servername {{.ServerName}}
        {{- if ne .CABundle.Name "" }}
        tls /etc/pki/{{.ServerName}}/caBundle.crt
        {{- else}}
        tls
        {{- end}}
        {{- end}}
        policy {{ CoreDNSForwardingPolicy .Policy }}
    }
    {{- end}}
    cache 900 {
        denial 9984 30
    }
    reload
}
`))

// ensureDNSConfigMap ensures that a configmap exists for a given DNS.
func (r *reconciler) ensureDNSConfigMap(dns *operatorv1.DNS, clusterDomain string) (bool, *corev1.ConfigMap, error) {
	haveCM, current, err := r.currentDNSConfigMap(dns)
	if err != nil {
		return false, nil, fmt.Errorf("failed to get configmap: %v", err)
	}
	desired, err := desiredDNSConfigMap(dns, clusterDomain)
	if err != nil {
		return haveCM, current, fmt.Errorf("failed to build configmap: %v", err)
	}

	switch {
	case !haveCM:
		if err := r.client.Create(context.TODO(), desired); err != nil {
			return false, nil, fmt.Errorf("failed to create configmap: %v", err)
		}
		logrus.Infof("created configmap: %s", desired.Name)
		return r.currentDNSConfigMap(dns)
	case haveCM:
		if updated, err := r.updateDNSConfigMap(current, desired); err != nil {
			return true, current, err
		} else if updated {
			return r.currentDNSConfigMap(dns)
		}
	}
	return true, current, nil
}

func (r *reconciler) currentDNSConfigMap(dns *operatorv1.DNS) (bool, *corev1.ConfigMap, error) {
	current := &corev1.ConfigMap{}
	err := r.client.Get(context.TODO(), DNSConfigMapName(dns), current)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil, nil
		}
		return false, nil, err
	}
	return true, current, nil
}

func desiredDNSConfigMap(dns *operatorv1.DNS, clusterDomain string) (*corev1.ConfigMap, error) {
	if len(clusterDomain) == 0 {
		clusterDomain = "cluster.local"
	}

	// Ensure that Transport: tls cannot be configured without a ServerName
	for _, server := range dns.Spec.Servers {
		t := server.ForwardPlugin.Transport
		sn := server.ForwardPlugin.ServerName

		// For security purposes, ServerName MUST be set when Transport is tls
		if t == operatorv1.TLSTransport && sn == "" {
			return nil, errTransportTLSConfiguredWithoutServerName
		}

		// When Transport is "" or cleartext and a ServerName is set the Corefile will ignore any other TLS settings
		if (t == "" || t == operatorv1.CleartextTransport) && sn != "" {
			logrus.Warningf("ServerName is set in %s but Transport is not set to tls. ServerName will be ignored", server.Name)
		}
	}

	if dns.Spec.UpstreamResolvers.Transport == operatorv1.TLSTransport && dns.Spec.UpstreamResolvers.ServerName == "" {
		return nil, errTransportTLSConfiguredWithoutServerName
	}

	upstreamResolvers := operatorv1.UpstreamResolvers{
		Upstreams: []operatorv1.Upstream{
			{
				Type: operatorv1.SystemResolveConfType,
			},
		},
		Policy:    operatorv1.SequentialForwardingPolicy,
		Transport: operatorv1.CleartextTransport,
	}

	if len(dns.Spec.UpstreamResolvers.Upstreams) > 0 {
		//Upstreams are defined, we can remove the default one
		upstreamResolvers.Upstreams = []operatorv1.Upstream{}

		for _, upstream := range dns.Spec.UpstreamResolvers.Upstreams {
			if upstream.Type == operatorv1.NetworkResolverType {
				if upstream.Address == "" {
					return nil, errInvalidNetworkUpstream
				}
			}
			upstreamCopy := *upstream.DeepCopy()
			//appending only if there are no duplicates
			if !contains(upstreamResolvers.Upstreams, upstream) {
				upstreamResolvers.Upstreams = append(upstreamResolvers.Upstreams, upstreamCopy)
			}
		}

	}

	if dns.Spec.UpstreamResolvers.Policy != "" {
		upstreamResolvers.Policy = dns.Spec.UpstreamResolvers.Policy
	}

	if dns.Spec.UpstreamResolvers.Transport == operatorv1.TLSTransport {
		if dns.Spec.UpstreamResolvers.ServerName != "" { //TODO: print a warning about the ignored TLS configuration
			upstreamResolvers.Transport = operatorv1.TLSTransport
			upstreamResolvers.ServerName = dns.Spec.UpstreamResolvers.ServerName
			if dns.Spec.UpstreamResolvers.CABundle.Name != "" {
				upstreamResolvers.CABundle = dns.Spec.UpstreamResolvers.CABundle
			}
		}
	}

	corefileParameters := struct {
		ClusterDomain     string
		Servers           interface{}
		UpstreamResolvers operatorv1.UpstreamResolvers
		PolicyStr         func(policy operatorv1.ForwardingPolicy) string
		LogLevel          string
	}{
		ClusterDomain:     clusterDomain,
		Servers:           dns.Spec.Servers,
		UpstreamResolvers: upstreamResolvers,
		PolicyStr:         coreDNSPolicy,
		LogLevel:          coreDNSLogLevel(dns),
	}
	corefile := new(bytes.Buffer)
	if err := corefileTemplate.Execute(corefile, corefileParameters); err != nil {
		return nil, err
	}

	name := DNSConfigMapName(dns)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name.Name,
			Namespace: name.Namespace,
			Labels: map[string]string{
				manifests.OwningDNSLabel: DNSDaemonSetLabel(dns),
			},
		},
		Data: map[string]string{
			"Corefile": corefile.String(),
		},
	}
	cm.SetOwnerReferences([]metav1.OwnerReference{dnsOwnerRef(dns)})

	return cm, nil
}

func (r *reconciler) updateDNSConfigMap(current, desired *corev1.ConfigMap) (bool, error) {
	changed, updated := corefileChanged(current, desired)
	if !changed {
		return false, nil
	}

	// Diff before updating because the client may mutate the object.
	diff := cmp.Diff(current, updated, cmpopts.EquateEmpty())
	if err := r.client.Update(context.TODO(), updated); err != nil {
		return false, fmt.Errorf("failed to update configmap: %v", err)
	}
	logrus.Infof("updated configmap %s/%s: %v", updated.Namespace, updated.Name, diff)
	return true, nil
}

func corefileChanged(current, expected *corev1.ConfigMap) (bool, *corev1.ConfigMap) {
	if cmp.Equal(current.Data, expected.Data, cmpopts.EquateEmpty()) {
		return false, current
	}
	updated := current.DeepCopy()
	updated.Data = expected.Data
	return true, updated
}

func coreDNSResolver(upstream operatorv1.Upstream, transport operatorv1.DNSTransport) (string, error) {
	if upstream.Type == operatorv1.NetworkResolverType {
		if upstream.Address == "" {
			return "", errInvalidNetworkUpstream
		}
		var address string
		if upstream.Port > 0 {
			address = net.JoinHostPort(strings.ToUpper(upstream.Address), fmt.Sprintf("%d", upstream.Port))
		} else {
			address = strings.ToUpper(upstream.Address)
		}
		if transport == operatorv1.TLSTransport {
			address = fmt.Sprintf("tls://%s", address)
		}
		return address, nil
	}
	return resolvConf, nil
}

func coreDNSPolicy(policy operatorv1.ForwardingPolicy) string {
	switch policy {
	case operatorv1.RandomForwardingPolicy:
		return "random"
	case operatorv1.RoundRobinForwardingPolicy:
		return "round_robin"
	case operatorv1.SequentialForwardingPolicy:
		return "sequential"
	}
	return "random"
}

func coreDNSLogLevel(dns *operatorv1.DNS) string {
	switch dns.Spec.LogLevel {
	case operatorv1.DNSLogLevelNormal:
		return "class error"
	case operatorv1.DNSLogLevelDebug:
		return "class denial error"
	case operatorv1.DNSLogLevelTrace:
		return "class all"
	}
	return "class error"
}

func contains(upstreams []operatorv1.Upstream, upstream operatorv1.Upstream) bool {
	for _, anUpstream := range upstreams {
		if cmp.Equal(upstream, anUpstream, cmp.Comparer(cmpPort), cmp.Comparer(cmpAddress), cmp.Comparer(cmpUpstreamType)) {
			return true
		}
	}
	return false
}

func cmpUpstreamType(a, b operatorv1.UpstreamType) bool {
	if a == "" {
		a = operatorv1.SystemResolveConfType
	}
	if b == "" {
		b = operatorv1.SystemResolveConfType
	}
	return a == b
}

func cmpPort(a, b uint32) bool {
	aVal := uint32(defaultDNSPort)
	if a != 0 {
		aVal = a
	}
	bVal := uint32(defaultDNSPort)
	if b != 0 {
		bVal = b
	}
	return aVal == bVal
}

func cmpAddress(a, b string) bool {
	return strings.EqualFold(a, b)
}
