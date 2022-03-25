//go:build e2e
// +build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"

	"github.com/openshift/cluster-dns-operator/pkg/operator/controller"

	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
)

// lookForStringInPodExec looks for expectedString in the output of command
// executed in the specified pod container every 2 seconds until the timeout
// is reached or the string is found. Returns an error if the string was not found.
func lookForStringInPodExec(ns, pod, container string, command []string, expectedString string, timeout time.Duration) error {
	cmdPath, err := exec.LookPath("oc")
	if err != nil {
		return err
	}
	args := []string{"exec", pod, "-c", container, fmt.Sprintf("--namespace=%v", ns), "--"}
	args = append(args, command...)
	if err := lookForString(cmdPath, args, expectedString, timeout); err != nil {
		return err
	}
	return nil
}

// lookForStringInPodLog looks for the given string in the log of the
// specified pod container every 2 seconds until the timeout is reached
// or the string is found. Returns an error if the string was not found.
func lookForStringInPodLog(ns, pod, container, expectedString string, timeout time.Duration) error {
	cmdPath, err := exec.LookPath("oc")
	if err != nil {
		return err
	}
	args := []string{"logs", pod, "-c", container, fmt.Sprintf("--namespace=%v", ns)}
	if err := lookForString(cmdPath, args, expectedString, timeout); err != nil {
		return err
	}
	return nil
}

// lookForStringInPodLog looks for the given string in the log of the
// specified pod container every 2 seconds until the timeout is reached
// or the string is found. Returns an error if the string was not found.
func lookForSubStringsInPodLog(ns, pod, container string, timeout time.Duration, expectedStrings ...string) error {
	cmdPath, err := exec.LookPath("oc")
	if err != nil {
		return err
	}
	args := []string{"logs", pod, "-c", container, fmt.Sprintf("--namespace=%v", ns)}
	if bool, err := lookForSubStrings(cmdPath, args, timeout, expectedStrings); err != nil && !bool {
		return err
	}
	return nil
}

// lookForString looks for the given string using cmd and args every
// 2 seconds until the timeout is reached or the string is found.
// Returns an error if the string was not found.
func lookForString(cmd string, args []string, expectedString string, timeout time.Duration) error {
	err := wait.PollImmediate(2*time.Second, timeout, func() (bool, error) {
		result, err := runCmd(cmd, args)
		//fmt.Printf("\n result %v", result)
		if err != nil {
			return false, nil
		}
		if !strings.Contains(result, expectedString) {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("failed to find %q", expectedString)
	}
	return nil
}
func lookForSubStrings(cmd string, args []string, timeout time.Duration, expectedStrings []string) (bool, error) {
	err := wait.PollImmediate(2*time.Second, timeout, func() (bool, error) {
		result, err := runCmd(cmd, args)
		if err != nil {
			return false, nil
		}
		slicedResult := strings.Split(result, "\"")
		slicedResultToString := strings.Join(slicedResult, " ")
		if bool, err := checkSubStrings(slicedResultToString, expectedStrings); err != nil && !bool {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return false, fmt.Errorf("failed to find %q", expectedStrings)
	}
	return true, nil
}

func checkSubStrings(str string, subs []string) (bool, error) {
	isCompleteMatch := true

	for _, sub := range subs {
		if strings.Contains(str, sub) {
		} else {
			isCompleteMatch = false
		}
	}

	if !isCompleteMatch {
		return false, fmt.Errorf("failed to find a match %q", strings.Join(subs, ""))
	}

	return isCompleteMatch, nil
}

// runCmd runs command cmd with arguments args and returns the output
// of the command or an error.
func runCmd(cmd string, args []string) (string, error) {
	execCmd := exec.Command(cmd, args...)
	result, err := execCmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to run command %q with args %q: %v", cmd, args, err)
	}
	return string(result), nil
}

// upstreamContainer returns a Container definition configured for
// the test upstream resolver.
func upstreamContainer(container, image string) corev1.Container {
	dnsPorts := []corev1.ContainerPort{
		{
			Name:          "dns",
			ContainerPort: int32(5353),
			Protocol:      corev1.Protocol("UDP"),
		},
		{
			Name:          "dns-tcp",
			ContainerPort: int32(5353),
			Protocol:      corev1.Protocol("TCP"),
		},
	}
	healthPort := intstr.IntOrString{
		IntVal: int32(8080),
	}
	getAction := &corev1.HTTPGetAction{
		Path:   "/health",
		Port:   healthPort,
		Scheme: "HTTP",
	}
	healthProbe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: getAction,
		},
		InitialDelaySeconds: int32(10),
		TimeoutSeconds:      int32(10),
	}
	configVolume := corev1.VolumeMount{
		Name:      "config-volume",
		ReadOnly:  true,
		MountPath: "/etc/coredns",
	}
	return corev1.Container{
		Name:           container,
		Image:          image,
		Command:        []string{"coredns"},
		Args:           []string{"-conf", "/etc/coredns/Corefile"},
		Ports:          dnsPorts,
		VolumeMounts:   []corev1.VolumeMount{configVolume},
		LivenessProbe:  healthProbe,
		ReadinessProbe: healthProbe,
	}
}

// upstreamPod returns a Pod definition configured for the test
// upstream resolver.
func upstreamPod(name, ns, image, cfgMap string) *corev1.Pod {
	coreContainer := upstreamContainer(name, image)
	volMode := int32(420)
	volSrc := &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{
			Name: cfgMap,
		},
		Items: []corev1.KeyToPath{
			{
				Key:  "Corefile",
				Path: "Corefile",
			},
		},
		DefaultMode: &volMode,
	}
	cfgVol := corev1.Volume{
		Name: "config-volume",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: volSrc,
		},
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"test": "upstream"},
		},
		Spec: corev1.PodSpec{
			Volumes:            []corev1.Volume{cfgVol},
			Containers:         []corev1.Container{coreContainer},
			ServiceAccountName: "dns",
		},
	}
}

func upstreamTLSPod(name, ns, image string, configMap *corev1.ConfigMap) *corev1.Pod {
	coreContainer := upstreamContainer(name, image)
	volumeName := configMap.Name

	items := []corev1.KeyToPath{}
	for k, _ := range configMap.Data {
		items = append(items, corev1.KeyToPath{Key: k, Path: k})
	}
	volume := corev1.Volume{
		Name: "config-volume",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: volumeName,
				},
				Items: items,
			},
		},
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"test": "upstream-tls"},
		},
		Spec: corev1.PodSpec{
			Volumes:            []corev1.Volume{volume},
			Containers:         []corev1.Container{coreContainer},
			ServiceAccountName: "dns",
		},
	}
}

//func upstreamVolume(configMap *corev1.ConfigMap) (*corev1.Volume, *corev1.VolumeMount) {
func upstreamVolume(configMap *corev1.ConfigMap) *corev1.Volume {
	volumeName := configMap.Name

	items := []corev1.KeyToPath{}
	for k, _ := range configMap.Data {
		items = append(items, corev1.KeyToPath{Key: k, Path: k})
	}
	volume := corev1.Volume{
		Name: volumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: volumeName,
				},
				Items: items,
			},
		},
	}
	//volumeMountPath := fmt.Sprintf("/tmp/%s", volumeName)
	//volumeMount := corev1.VolumeMount{
	//	Name:      volumeName,
	//	MountPath: volumeMountPath,
	//	ReadOnly:  true,
	//}
	//return &volume, &volumeMount
	return &volume
}

// upstreamService returns a Service definition configured for the
// test upstream resolver.
func upstreamService(name, ns string) *corev1.Service {
	svcPorts := []corev1.ServicePort{
		{
			Name:       "dns",
			Protocol:   "UDP",
			Port:       53,
			TargetPort: intstr.IntOrString{IntVal: 5353},
		},
		{
			Name:       "dns-tcp",
			Protocol:   "TCP",
			Port:       53,
			TargetPort: intstr.IntOrString{IntVal: 5353},
		},
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: corev1.ServiceSpec{
			Ports:    svcPorts,
			Selector: map[string]string{"test": "upstream"},
		},
	}
}

// buildConfigMap returns a ConfigMap definition using name
// for the ConfigMap name, ns as the ConfigMap namespace, k
// as the ConfigMap data key and v as the ConfigMap data value.
func buildConfigMap(name, ns, k, v string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Data: map[string]string{k: v},
	}
}

// buildPod returns a Pod definition using name as the Pod's name, ns as
// the Pod's namespace, image as the Pod container's image and cmd as the
// Pod container's command.
func buildPod(name, ns, image string, cmd []string) *corev1.Pod {
	container := buildContainer(name, image, cmd)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{container},
		},
	}
}

// buildContainer returns a Container definition using name as the
// Container's name, image as the Container's image and cmd as
// Container's command.
func buildContainer(name, image string, cmd []string) corev1.Container {
	return corev1.Container{
		Name:    name,
		Image:   image,
		Command: cmd,
	}
}

func waitForClusterOperatorConditions(t *testing.T, cl client.Client, timeout time.Duration, conditions ...configv1.ClusterOperatorStatusCondition) error {
	return wait.PollImmediate(1*time.Second, timeout, func() (bool, error) {
		co := &configv1.ClusterOperator{}
		coName := controller.DNSClusterOperatorName()
		if err := cl.Get(context.TODO(), coName, co); err != nil {
			t.Logf("failed to get DNS cluster operator %s: %v", coName.Name, err)
			return false, nil
		}

		expected := clusterOperatorConditionMap(conditions...)
		current := clusterOperatorConditionMap(co.Status.Conditions...)
		return conditionsMatchExpected(expected, current), nil
	})
}

func waitForDNSConditions(t *testing.T, cl client.Client, timeout time.Duration, name types.NamespacedName, conditions ...operatorv1.OperatorCondition) error {
	return wait.PollImmediate(1*time.Second, timeout, func() (bool, error) {
		dns := &operatorv1.DNS{}
		if err := cl.Get(context.TODO(), name, dns); err != nil {
			t.Logf("failed to get DNS operator %s: %v", name.Name, err)
			return false, nil
		}
		expected := operatorConditionMap(conditions...)
		current := operatorConditionMap(dns.Status.Conditions...)
		return conditionsMatchExpected(expected, current), nil
	})
}

func clusterOperatorConditionMap(conditions ...configv1.ClusterOperatorStatusCondition) map[string]string {
	conds := map[string]string{}
	for _, cond := range conditions {
		conds[string(cond.Type)] = string(cond.Status)
	}
	return conds
}

func operatorConditionMap(conditions ...operatorv1.OperatorCondition) map[string]string {
	conds := map[string]string{}
	for _, cond := range conditions {
		conds[cond.Type] = string(cond.Status)
	}
	return conds
}

func conditionsMatchExpected(expected, actual map[string]string) bool {
	filtered := map[string]string{}
	for k := range actual {
		if _, comparable := expected[k]; comparable {
			filtered[k] = actual[k]
		}
	}
	return reflect.DeepEqual(expected, filtered)
}

// createCertPair creates a PEM encoded CA cert, serving cert, and private key for the serving cert.
// The use case for this is testing DNS-over-TLS where we need to install a CA in cluster DNS and a serving cert + key
// in the upstream resolver.
func createCertPair() (pemEncodedCA string, pemEncodedCert string, pemEncodedCertPrivateKey string) {
	// Fields common to both serving certs and CAs
	certCommon := &x509.Certificate{
		SerialNumber: big.NewInt(2019),
		Subject: pkix.Name{
			Organization:  []string{"Company, INC."},
			Country:       []string{"US"},
			Province:      []string{""},
			Locality:      []string{"San Francisco"},
			StreetAddress: []string{"Golden Gate Bridge"},
			PostalCode:    []string{"94016"},
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().AddDate(0, 0, 7),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:    x509.KeyUsageDigitalSignature,
	}

	// Make adjustments to the CA cert. These indicate that it's a CA cert and can sign other certs.
	caCert := certCommon
	caCert.IsCA = true
	caCert.BasicConstraintsValid = true
	caCert.KeyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign

	// Make adjustments to the serving cert. These indicate what IP address it should be used for.
	cert := certCommon
	cert.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	cert.SubjectKeyId = []byte{1, 2, 3, 4, 6}

	caKey, _ := rsa.GenerateKey(rand.Reader, 4096)
	//if err != nil {
	//	return err
	//}

	certKey, _ := rsa.GenerateKey(rand.Reader, 4096)
	//if err != nil {
	//	errs = append(errs, err)
	//}

	rawCA, _ := x509.CreateCertificate(rand.Reader, caCert, caCert, &caKey.PublicKey, caKey)
	//if err != nil {
	//	errs = append(errs, err)
	//}

	rawCert, _ := x509.CreateCertificate(rand.Reader, cert, caCert, &certKey.PublicKey, caKey)
	//if err != nil {
	//	errs = append(errs, err)
	//}

	pemCA := new(bytes.Buffer)
	pem.Encode(pemCA, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: rawCA,
	})

	pemCert := new(bytes.Buffer)
	pem.Encode(pemCert, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: rawCert,
	})

	pemCertPrivateKey := new(bytes.Buffer)
	pem.Encode(pemCertPrivateKey, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(certKey),
	})

	//servingCert, err := tls.X509KeyPair(pemCert.Bytes(), pemCertPrivateKey.Bytes())
	//if err != nil {
	//	errs = append(errs, err)
	//}

	return pemCA.String(), pemCert.String(), pemCertPrivateKey.String() //, errs
}
