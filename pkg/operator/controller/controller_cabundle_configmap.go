package controller

import (
	"context"
	"fmt"
	"reflect"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/sirupsen/logrus"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ensureCABundleConfigMaps syncs CA bundle configmaps for a DNS
// between the openshift-config and openshift-dns namespaces if the user has
// configured a CA bundle configmap.  Returns a Boolean indicating whether the
// configmap exists, the configmap if it does exist, and an error value.
func (r *reconciler) ensureCABundleConfigMaps(dns *operatorv1.DNS) error {
	var configmapNames []string
	if dns.Spec.UpstreamResolvers.TransportConfig.TLS.CABundle.Name != "" {
		configmapNames = append(configmapNames, dns.Spec.UpstreamResolvers.TransportConfig.TLS.CABundle.Name)
	}
	for _, server := range dns.Spec.Servers {
		if server.ForwardPlugin.TransportConfig.TLS.CABundle.Name != "" {
			configmapNames = append(configmapNames, server.ForwardPlugin.TransportConfig.TLS.CABundle.Name)
		}
	}

	for _, name := range configmapNames {
		sourceName := types.NamespacedName{
			Namespace: GlobalUserSpecifiedConfigNamespace,
			Name:      name,
		}
		haveSource, source, err := r.currentCABundleConfigMap(sourceName)
		if err != nil {
			return err
		}

		destName := CABundleConfigMapName(source.Name)
		have, current, err := r.currentCABundleConfigMap(destName)
		if err != nil {
			return err
		}

		want, desired, err := desiredCABundleConfigMap(dns, haveSource, source, destName)
		if err != nil {
			return err
		}

		switch {
		case !want && !have:
			return nil
		case !want && have:
			if err := r.client.Delete(context.TODO(), current); err != nil {
				if !errors.IsNotFound(err) {
					return fmt.Errorf("failed to delete configmap: %w", err)
				}
			} else {
				logrus.Infof("deleted configmap %s/%s", current.Namespace, current.Name)
			}
			return nil
		case want && !have:
			if err := r.client.Create(context.TODO(), desired); err != nil {
				return fmt.Errorf("failed to create configmap: %w", err)
			}
			logrus.Infof("created configmap %s/%s", desired.Namespace, desired.Name)
			_, _, err = r.currentCABundleConfigMap(destName)
			return err
		case want && have:
			if updated, err := r.updateCABundleConfigMap(current, desired); err != nil {
				return fmt.Errorf("failed to update configmap: %w", err)
			} else if updated {
				logrus.Infof("updated configmap %s/%s", desired.Namespace, desired.Name)
				_, _, err = r.currentCABundleConfigMap(destName)
				return err
			}
		}
	}

	return nil
}

// desiredCABundleConfigMap returns the desired CA bundle configmap.  Returns a
// Boolean indicating whether a configmap is desired, as well as the configmap
// if one is desired.
func desiredCABundleConfigMap(dns *operatorv1.DNS, haveSource bool, sourceConfigmap *corev1.ConfigMap, name types.NamespacedName) (bool, *corev1.ConfigMap, error) {
	if !haveSource {
		return false, nil, nil
	}
	if dns.DeletionTimestamp != nil {
		return false, nil, nil
	}
	cm := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name.Name,
			Namespace: name.Namespace,
		},
		Data: sourceConfigmap.Data,
	}
	cm.SetOwnerReferences([]metav1.OwnerReference{dnsOwnerRef(dns)})

	return true, &cm, nil
}

// currentCABundleConfigMap returns the current configmap.  Returns a Boolean
// indicating whether the configmap existed, the configmap if it did exist, and
// an error value.
func (r *reconciler) currentCABundleConfigMap(name types.NamespacedName) (bool, *corev1.ConfigMap, error) {
	if len(name.Name) == 0 {
		return false, nil, nil
	}
	cm := &corev1.ConfigMap{}
	if err := r.client.Get(context.TODO(), name, cm); err != nil {
		if errors.IsNotFound(err) {
			return false, nil, nil
		}
		return false, nil, err
	}
	return true, cm, nil
}

// updateCABundleConfigMap updates a configmap.  Returns a Boolean indicating
// whether the configmap was updated, and an error value.
func (r *reconciler) updateCABundleConfigMap(current, desired *corev1.ConfigMap) (bool, error) {
	if caBundleConfigmapsEqual(current, desired) {
		return false, nil
	}
	updated := current.DeepCopy()
	updated.Data = desired.Data
	if err := r.client.Update(context.TODO(), updated); err != nil {
		return false, err
	}
	logrus.Infof("updated configmap %s/%s", updated.Namespace, updated.Name)
	return true, nil
}

// caBundleConfigmapsEqual compares two CA bundle configmaps.  Returns true if
// the configmaps should be considered equal for the purpose of determining
// whether an update is necessary, false otherwise.
func caBundleConfigmapsEqual(a, b *corev1.ConfigMap) bool {
	return reflect.DeepEqual(a.Data, b.Data)
}
