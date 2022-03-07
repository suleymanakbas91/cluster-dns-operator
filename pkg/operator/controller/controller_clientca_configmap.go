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

// ensureClientCAConfigMap syncs client CA configmaps for a DNS
// between the openshift-config and openshift-dns namespaces if the user has
// configured a client CA configmap.  Returns a Boolean indicating whether the
// configmap exists, the configmap if it does exist, and an error value.
func (r *reconciler) ensureClientCAConfigMap(dns *operatorv1.DNS) (bool, *corev1.ConfigMap, error) {
	sourceName := types.NamespacedName{
		Namespace: GlobalUserSpecifiedConfigNamespace,
		Name:      dns.Spec.UpstreamResolvers.CABundle.Name,
	}
	haveSource, source, err := r.currentClientCAConfigMap(sourceName)
	if err != nil {
		return false, nil, err
	}

	destName := ClientCABundleConfigMapName(dns)
	have, current, err := r.currentClientCAConfigMap(destName)
	if err != nil {
		return false, nil, err
	}

	want, desired, err := desiredClientCAConfigMap(dns, haveSource, source, destName)
	if err != nil {
		return have, current, err
	}

	switch {
	case !want && !have:
		return false, nil, nil
	case !want && have:
		if err := r.client.Delete(context.TODO(), current); err != nil {
			if !errors.IsNotFound(err) {
				return true, current, fmt.Errorf("failed to delete configmap: %w", err)
			}
		} else {
			logrus.Infof("deleted configmap %s/%s", current.Namespace, current.Name)
		}
		return false, nil, nil
	case want && !have:
		if err := r.client.Create(context.TODO(), desired); err != nil {
			return false, nil, fmt.Errorf("failed to create configmap: %w", err)
		}
		logrus.Infof("created configmap %s/%s", desired.Namespace, desired.Name)
		return r.currentClientCAConfigMap(destName)
	case want && have:
		if updated, err := r.updateClientCAConfigMap(current, desired); err != nil {
			return true, current, fmt.Errorf("failed to update configmap: %w", err)
		} else if updated {
			logrus.Infof("updated configmap %s/%s", desired.Namespace, desired.Name)
			return r.currentClientCAConfigMap(destName)
		}
	}

	return have, current, nil
}

// desiredClientCAConfigMap returns the desired client CA configmap.  Returns a
// Boolean indicating whether a configmap is desired, as well as the configmap
// if one is desired.
func desiredClientCAConfigMap(dns *operatorv1.DNS, haveSource bool, sourceConfigmap *corev1.ConfigMap, name types.NamespacedName) (bool, *corev1.ConfigMap, error) {
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

// currentClientCAConfigMap returns the current configmap.  Returns a Boolean
// indicating whether the configmap existed, the configmap if it did exist, and
// an error value.
func (r *reconciler) currentClientCAConfigMap(name types.NamespacedName) (bool, *corev1.ConfigMap, error) {
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

// updateClientCAConfigMap updates a configmap.  Returns a Boolean indicating
// whether the configmap was updated, and an error value.
func (r *reconciler) updateClientCAConfigMap(current, desired *corev1.ConfigMap) (bool, error) {
	if clientCAConfigmapsEqual(current, desired) {
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

// clientCAConfigmapsEqual compares two client CA configmaps.  Returns true if
// the configmaps should be considered equal for the purpose of determining
// whether an update is necessary, false otherwise.
func clientCAConfigmapsEqual(a, b *corev1.ConfigMap) bool {
	return reflect.DeepEqual(a.Data, b.Data)
}
