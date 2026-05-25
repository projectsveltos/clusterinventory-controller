/*
Copyright 2026. projectsveltos.io. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
)

const (
	// kubeconfigSecretReaderProviderName is the only access provider supported by this
	// controller today. It points to a pre-existing Secret that holds a full kubeconfig.
	//
	// To add exec-plugin-based providers (e.g. GKE Fleet, EKS), add a branch in
	// getKubeconfig and a corresponding getKubeconfigFrom<Provider> helper.
	// No other code needs to change.
	kubeconfigSecretReaderProviderName = "kubeconfig-secretreader" //nolint:gosec // not a credential, just a provider name

	// execExtensionKey is the kubeconfig Cluster extension key that carries
	// cluster-specific config for the exec credential plugin (KEP-541).
	execExtensionKey = "client.authentication.k8s.io/exec"

	// kubeconfigKey is the key within the managed Secret that holds the kubeconfig.
	kubeconfigKey = "kubeconfig"

	// managedByLabel is added to every SveltosCluster and kubeconfig Secret
	// created by this controller so they can be identified for cleanup.
	managedByLabel = "clusterinventory.projectsveltos.io/managed-by"
	managedByValue = "clusterinventory-controller"
)

// secretReaderConfig is the JSON payload embedded in the ClusterProfile's
// "client.authentication.k8s.io/exec" Cluster extension when the provider
// is "kubeconfig-secretreader".
type secretReaderConfig struct {
	Name      string `json:"name"`
	Key       string `json:"key"`
	Namespace string `json:"namespace,omitempty"`
}

// InitScheme registers all types required by the controller into the given scheme.
func InitScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := apiextensionsv1.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := libsveltosv1beta1.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := clusterinventoryv1alpha1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// kubeconfigSecretName returns the name of the kubeconfig Secret to create for a ClusterProfile.
func kubeconfigSecretName(cpName string) string {
	return cpName + "-sveltos-kubeconfig"
}

// getKubeconfig returns the raw kubeconfig bytes for the cluster described by cp.
//
// Currently only the "kubeconfig-secretreader" access provider is supported:
// the provider's exec extension must reference a pre-existing Secret that
// contains a complete kubeconfig.
//
// To support exec-plugin-based providers (e.g. GKE Fleet, EKS), add additional
// branches here and implement a corresponding helper (e.g. getKubeconfigFromGKE).
// Only this function and the new helper need to change.
func getKubeconfig(ctx context.Context, c client.Client,
	cp *clusterinventoryv1alpha1.ClusterProfile, logger logr.Logger) ([]byte, error) {

	if provider := findAccessProvider(cp, kubeconfigSecretReaderProviderName); provider != nil {
		logger.V(logs.LogDebug).Info("using kubeconfig-secretreader access provider")
		return getKubeconfigFromSecretReader(ctx, c, cp.Namespace, provider)
	}

	// TODO: add more provider branches here when exec-plugin support is implemented.

	return nil, fmt.Errorf("no supported access provider in ClusterProfile %s/%s"+
		" (only %q is supported currently)",
		cp.Namespace, cp.Name, kubeconfigSecretReaderProviderName)
}

// findAccessProvider returns the first AccessProvider matching name.
// AccessProviders is checked before the deprecated CredentialProviders field.
func findAccessProvider(cp *clusterinventoryv1alpha1.ClusterProfile,
	name string) *clusterinventoryv1alpha1.AccessProvider {

	for i := range cp.Status.AccessProviders {
		if cp.Status.AccessProviders[i].Name == name {
			return &cp.Status.AccessProviders[i]
		}
	}
	for i := range cp.Status.CredentialProviders {
		if cp.Status.CredentialProviders[i].Name == name {
			return &cp.Status.CredentialProviders[i]
		}
	}
	return nil
}

// getKubeconfigFromSecretReader handles the "kubeconfig-secretreader" provider.
// It reads the Secret reference from the provider's exec extension and returns
// the kubeconfig bytes stored in that Secret.
func getKubeconfigFromSecretReader(ctx context.Context, c client.Client,
	cpNamespace string, provider *clusterinventoryv1alpha1.AccessProvider) ([]byte, error) {

	var cfg secretReaderConfig
	found := false
	for _, ext := range provider.Cluster.Extensions {
		if ext.Name == execExtensionKey {
			if err := json.Unmarshal(ext.Extension.Raw, &cfg); err != nil {
				return nil, fmt.Errorf("failed to unmarshal exec extension: %w", err)
			}
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("extension %q not found in kubeconfig-secretreader provider", execExtensionKey)
	}
	if cfg.Name == "" {
		return nil, fmt.Errorf("missing name in exec extension config")
	}
	if cfg.Key == "" {
		return nil, fmt.Errorf("missing key in exec extension config")
	}

	ns := cfg.Namespace
	if ns == "" {
		ns = cpNamespace
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: cfg.Name}, secret); err != nil {
		return nil, fmt.Errorf("failed to get source kubeconfig secret %s/%s: %w", ns, cfg.Name, err)
	}

	data, ok := secret.Data[cfg.Key]
	if !ok || len(data) == 0 {
		return nil, fmt.Errorf("secret %s/%s missing key %q", ns, cfg.Name, cfg.Key)
	}
	return data, nil
}

// reconcileKubeconfigSecret creates or updates the managed kubeconfig Secret for cp.
func reconcileKubeconfigSecret(ctx context.Context, c client.Client,
	cp *clusterinventoryv1alpha1.ClusterProfile, kubeconfigData []byte, logger logr.Logger) error {

	secretName := kubeconfigSecretName(cp.Name)
	logger = logger.WithValues("secret", fmt.Sprintf("%s/%s", cp.Namespace, secretName))

	existing := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Namespace: cp.Namespace, Name: secretName}, existing)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get kubeconfig secret: %w", err)
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: cp.Namespace,
				Labels:    map[string]string{managedByLabel: managedByValue},
			},
			Data: map[string][]byte{kubeconfigKey: kubeconfigData},
		}
		logger.V(logs.LogDebug).Info("creating kubeconfig secret")
		return c.Create(ctx, secret)
	}

	if bytes.Equal(existing.Data[kubeconfigKey], kubeconfigData) {
		return nil
	}

	existing.Data = map[string][]byte{kubeconfigKey: kubeconfigData}
	logger.V(logs.LogDebug).Info("updating kubeconfig secret")
	return c.Update(ctx, existing)
}

// reconcileSveltosCluster creates or updates the SveltosCluster for cp.
func reconcileSveltosCluster(ctx context.Context, c client.Client,
	cp *clusterinventoryv1alpha1.ClusterProfile, logger logr.Logger) error {

	secretName := kubeconfigSecretName(cp.Name)
	logger = logger.WithValues("sveltoscluster", fmt.Sprintf("%s/%s", cp.Namespace, cp.Name))

	existing := &libsveltosv1beta1.SveltosCluster{}
	err := c.Get(ctx, types.NamespacedName{Namespace: cp.Namespace, Name: cp.Name}, existing)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get SveltosCluster: %w", err)
		}
		sc := &libsveltosv1beta1.SveltosCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cp.Name,
				Namespace: cp.Namespace,
				Labels:    map[string]string{managedByLabel: managedByValue},
			},
			Spec: libsveltosv1beta1.SveltosClusterSpec{
				KubeconfigName:    secretName,
				KubeconfigKeyName: kubeconfigKey,
			},
		}
		logger.V(logs.LogDebug).Info("creating SveltosCluster")
		return c.Create(ctx, sc)
	}

	if existing.Spec.KubeconfigName == secretName && existing.Spec.KubeconfigKeyName == kubeconfigKey {
		return nil
	}

	existing.Spec.KubeconfigName = secretName
	existing.Spec.KubeconfigKeyName = kubeconfigKey
	logger.V(logs.LogDebug).Info("updating SveltosCluster")
	return c.Update(ctx, existing)
}

// deleteSveltosCluster deletes the SveltosCluster associated with cp.
func deleteSveltosCluster(ctx context.Context, c client.Client,
	cp *clusterinventoryv1alpha1.ClusterProfile, logger logr.Logger) error {

	sc := &libsveltosv1beta1.SveltosCluster{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: cp.Namespace, Name: cp.Name}, sc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get SveltosCluster %s/%s: %w", cp.Namespace, cp.Name, err)
	}

	logger.V(logs.LogDebug).Info(fmt.Sprintf("deleting SveltosCluster %s/%s", cp.Namespace, cp.Name))
	return c.Delete(ctx, sc)
}

// deleteKubeconfigSecret deletes the managed kubeconfig Secret for cp.
func deleteKubeconfigSecret(ctx context.Context, c client.Client,
	cp *clusterinventoryv1alpha1.ClusterProfile, logger logr.Logger) error {

	secretName := kubeconfigSecretName(cp.Name)
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: cp.Namespace, Name: secretName}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get kubeconfig secret %s/%s: %w", cp.Namespace, secretName, err)
	}

	logger.V(logs.LogDebug).Info(fmt.Sprintf("deleting kubeconfig secret %s/%s", cp.Namespace, secretName))
	return c.Delete(ctx, secret)
}
