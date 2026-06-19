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
	"os"
	"os/exec"
	"reflect"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clientauthenticationv1 "k8s.io/client-go/pkg/apis/clientauthentication/v1"
	clientcmdv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

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

	// displayNameAnnotation carries ClusterProfile.Spec.DisplayName on the SveltosCluster.
	displayNameAnnotation = "clusterinventory.projectsveltos.io/display-name"

	// sourceSecretIndex is the field-index key used to map a source Secret back to the
	// ClusterProfiles that reference it via the kubeconfig-secretreader access provider.
	sourceSecretIndex = ".status.sourceSecretRef"

	// tokenKubeconfigCluster, tokenKubeconfigUser, and tokenKubeconfigContext are
	// the fixed names used in the kubeconfig built by BuildKubeconfigFromExecStatus.
	tokenKubeconfigCluster = "cluster"
	tokenKubeconfigUser    = "user"
	tokenKubeconfigContext = "context"
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

// getKubeconfig returns the raw kubeconfig bytes for the cluster described by cp,
// along with an optional token expiry time (nil if the kubeconfig does not expire).
//
// The kubeconfig-secretreader provider is always supported: it reads a full
// kubeconfig from a pre-existing Secret.
//
// When accessCfg is non-nil, any provider listed in the config file that
// matches an AccessProvider in the ClusterProfile is also supported. The
// corresponding exec plugin is invoked directly in this process; the resulting
// bearer token is written into a plain kubeconfig so that sveltoscluster-manager
// needs no exec binary. The token expiry is returned so the caller can requeue
// before the token expires.
func getKubeconfig(ctx context.Context, c client.Client,
	accessCfg *access.Config,
	cp *clusterinventoryv1alpha1.ClusterProfile, logger logr.Logger) ([]byte, *time.Time, error) {

	if provider := findAccessProvider(cp, kubeconfigSecretReaderProviderName); provider != nil {
		logger.V(logs.LogDebug).Info("using kubeconfig-secretreader access provider")
		data, err := getKubeconfigFromSecretReader(ctx, c, cp.Namespace, provider)
		return data, nil, err
	}

	if accessCfg != nil {
		for _, p := range accessCfg.Providers {
			if findAccessProvider(cp, p.Name) != nil {
				logger.V(logs.LogDebug).Info("using exec-plugin access provider", "provider", p.Name)
				return getKubeconfigFromExecPlugin(ctx, accessCfg, cp, logger)
			}
		}
	}

	return nil, nil, fmt.Errorf("no supported access provider in ClusterProfile %s/%s"+
		" (kubeconfig-secretreader not present; set --clusterprofile-provider-file to enable exec-plugin providers)",
		cp.Namespace, cp.Name)
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

// sourceSecretNamespacedName returns the "namespace/name" of the source Secret
// referenced by the kubeconfig-secretreader access provider, or "" if the provider
// is absent or carries no valid Secret reference.
func sourceSecretNamespacedName(cp *clusterinventoryv1alpha1.ClusterProfile) string {
	provider := findAccessProvider(cp, kubeconfigSecretReaderProviderName)
	if provider == nil {
		return ""
	}
	for _, ext := range provider.Cluster.Extensions {
		if ext.Name != execExtensionKey {
			continue
		}
		var cfg secretReaderConfig
		if err := json.Unmarshal(ext.Extension.Raw, &cfg); err != nil || cfg.Name == "" {
			return ""
		}
		ns := cfg.Namespace
		if ns == "" {
			ns = cp.Namespace
		}
		return ns + "/" + cfg.Name
	}
	return ""
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
		if createErr := c.Create(ctx, secret); createErr != nil {
			if !apierrors.IsAlreadyExists(createErr) {
				return createErr
			}
			// Race: another reconcile created the secret between our Get and Create.
			if err = c.Get(ctx, types.NamespacedName{Namespace: cp.Namespace, Name: secretName}, existing); err != nil {
				return fmt.Errorf("failed to re-get kubeconfig secret: %w", err)
			}
		} else {
			return nil
		}
	}

	if bytes.Equal(existing.Data[kubeconfigKey], kubeconfigData) {
		return nil
	}

	existing.Data = map[string][]byte{kubeconfigKey: kubeconfigData}
	logger.V(logs.LogDebug).Info("updating kubeconfig secret")
	return c.Update(ctx, existing)
}

// sveltosClusterLabels returns the labels to set on the SveltosCluster.
// All labels from the ClusterProfile are copied so that Sveltos selectors can
// match the SveltosCluster using the same labels. The managed-by label is
// always added (and takes precedence) so the controller can identify its own objects.
func sveltosClusterLabels(cp *clusterinventoryv1alpha1.ClusterProfile) map[string]string {
	labels := make(map[string]string, len(cp.Labels)+1)
	for k, v := range cp.Labels {
		labels[k] = v
	}
	labels[managedByLabel] = managedByValue
	return labels
}

// applyDisplayName returns a copy of annotations with the display-name annotation
// set or removed according to displayName. Other existing annotations are preserved.
func applyDisplayName(existing map[string]string, displayName string) map[string]string {
	result := make(map[string]string, len(existing)+1)
	for k, v := range existing {
		result[k] = v
	}
	if displayName != "" {
		result[displayNameAnnotation] = displayName
	} else {
		delete(result, displayNameAnnotation)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// reconcileSveltosCluster creates or updates the SveltosCluster for cp.
func reconcileSveltosCluster(ctx context.Context, c client.Client,
	cp *clusterinventoryv1alpha1.ClusterProfile, logger logr.Logger) error {

	secretName := kubeconfigSecretName(cp.Name)
	logger = logger.WithValues("sveltoscluster", fmt.Sprintf("%s/%s", cp.Namespace, cp.Name))

	desiredLabels := sveltosClusterLabels(cp)

	existing := &libsveltosv1beta1.SveltosCluster{}
	err := c.Get(ctx, types.NamespacedName{Namespace: cp.Namespace, Name: cp.Name}, existing)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get SveltosCluster: %w", err)
		}
		sc := &libsveltosv1beta1.SveltosCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:        cp.Name,
				Namespace:   cp.Namespace,
				Labels:      desiredLabels,
				Annotations: applyDisplayName(nil, cp.Spec.DisplayName),
			},
			Spec: libsveltosv1beta1.SveltosClusterSpec{
				KubeconfigName:    secretName,
				KubeconfigKeyName: kubeconfigKey,
			},
		}
		logger.V(logs.LogDebug).Info("creating SveltosCluster")
		if createErr := c.Create(ctx, sc); createErr != nil {
			if !apierrors.IsAlreadyExists(createErr) {
				return createErr
			}
			// Race: another reconcile created the SveltosCluster between our Get and Create.
			if err = c.Get(ctx, types.NamespacedName{Namespace: cp.Namespace, Name: cp.Name}, existing); err != nil {
				return fmt.Errorf("failed to re-get SveltosCluster: %w", err)
			}
		} else {
			return nil
		}
	}

	desiredAnnotations := applyDisplayName(existing.Annotations, cp.Spec.DisplayName)

	if existing.Spec.KubeconfigName == secretName &&
		existing.Spec.KubeconfigKeyName == kubeconfigKey &&
		reflect.DeepEqual(existing.Labels, desiredLabels) &&
		reflect.DeepEqual(existing.Annotations, desiredAnnotations) {

		return nil
	}

	existing.Spec.KubeconfigName = secretName
	existing.Spec.KubeconfigKeyName = kubeconfigKey
	existing.Labels = desiredLabels
	existing.Annotations = desiredAnnotations
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

// getKubeconfigFromExecPlugin handles exec-plugin-based access providers.
// It uses pkg/access to build an exec config (merging any cluster-specific
// CLI args and env vars from the ClusterProfile extensions per KEP-5339),
// then invokes the binary directly to obtain a short-lived bearer token.
// The token is embedded in a plain kubeconfig so that sveltoscluster-manager
// needs no exec binary in its own pod.
func getKubeconfigFromExecPlugin(ctx context.Context,
	accessCfg *access.Config,
	cp *clusterinventoryv1alpha1.ClusterProfile, logger logr.Logger) ([]byte, *time.Time, error) {

	// BuildConfigFromCP handles provider lookup and KEP-5339 arg/env merging.
	restCfg, err := accessCfg.BuildConfigFromCP(cp)
	if err != nil {
		return nil, nil, fmt.Errorf("building rest.Config from ClusterProfile: %w", err)
	}
	if restCfg.ExecProvider == nil {
		return nil, nil, fmt.Errorf("access provider produced no exec configuration")
	}

	// Find the matching AccessProvider to get the cluster server address and CA.
	var ap *clusterinventoryv1alpha1.AccessProvider
	for _, p := range accessCfg.Providers {
		if found := findAccessProvider(cp, p.Name); found != nil {
			ap = found
			break
		}
	}
	if ap == nil {
		return nil, nil, fmt.Errorf("no matching access provider found in ClusterProfile %s/%s",
			cp.Namespace, cp.Name)
	}

	var envVars []string
	for _, e := range restCfg.ExecProvider.Env {
		envVars = append(envVars, e.Name+"="+e.Value)
	}

	logger.V(logs.LogDebug).Info("invoking exec plugin", "command", restCfg.ExecProvider.Command)
	status, err := invokeExecPlugin(ctx, restCfg.ExecProvider.Command,
		restCfg.ExecProvider.Args, envVars, ap)
	if err != nil {
		return nil, nil, err
	}

	var expiry *time.Time
	if status.ExpirationTimestamp != nil {
		t := status.ExpirationTimestamp.Time
		expiry = &t
	}

	kubeconfig, err := BuildKubeconfigFromExecStatus(ap.Cluster.Server, ap.Cluster.CertificateAuthorityData, status)
	if err != nil {
		return nil, nil, fmt.Errorf("building kubeconfig from exec status: %w", err)
	}

	return kubeconfig, expiry, nil
}

// invokeExecPlugin runs an exec credential plugin and returns its full
// ExecCredentialStatus. It sets KUBERNETES_EXEC_INFO so that plugins
// that request cluster info (ProvideClusterInfo: true) receive the correct
// server and CA data. Returns an error if the plugin emits no status, or if
// the status contains neither a token nor client certificate data.
func invokeExecPlugin(ctx context.Context,
	command string, args []string, envVars []string,
	ap *clusterinventoryv1alpha1.AccessProvider) (*clientauthenticationv1.ExecCredentialStatus, error) {

	// Build KUBERNETES_EXEC_INFO: an ExecCredential carrying the cluster
	// connection details so the plugin knows which cluster it is authenticating to.
	execInfo := &clientauthenticationv1.ExecCredential{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "client.authentication.k8s.io/v1",
			Kind:       "ExecCredential",
		},
		Spec: clientauthenticationv1.ExecCredentialSpec{
			Cluster: &clientauthenticationv1.Cluster{
				Server:                   ap.Cluster.Server,
				CertificateAuthorityData: ap.Cluster.CertificateAuthorityData,
			},
		},
	}
	// Pass through the exec extension data as cluster config if present.
	for _, ext := range ap.Cluster.Extensions {
		if ext.Name == execExtensionKey {
			execInfo.Spec.Cluster.Config = runtime.RawExtension{Raw: ext.Extension.Raw}
			break
		}
	}
	execInfoJSON, err := json.Marshal(execInfo)
	if err != nil {
		return nil, fmt.Errorf("marshaling KUBERNETES_EXEC_INFO: %w", err)
	}

	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = append(os.Environ(), "KUBERNETES_EXEC_INFO="+string(execInfoJSON))
	cmd.Env = append(cmd.Env, envVars...)

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("exec plugin %q: %w", command, err)
	}

	var result clientauthenticationv1.ExecCredential
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("parsing exec plugin output: %w", err)
	}
	if result.Status == nil {
		return nil, fmt.Errorf("exec plugin %q returned no status", command)
	}
	if result.Status.Token == "" && result.Status.ClientCertificateData == "" {
		return nil, fmt.Errorf("exec plugin %q returned neither a token nor client certificate data", command)
	}
	return result.Status, nil
}

// BuildKubeconfigFromExecStatus constructs a minimal kubeconfig from the
// ExecCredentialStatus returned by an exec credential plugin. All credential
// fields are mapped:
//   - status.Token → AuthInfo.Token
//   - status.ClientCertificateData → AuthInfo.ClientCertificateData (PEM bytes)
//   - status.ClientKeyData → AuthInfo.ClientKeyData (PEM bytes)
//
// Empty fields are omitted, so the result is correct for token-only,
// cert+key-only, or combined credentials. The resulting YAML can be stored in
// a Secret and used by sveltoscluster-manager without any exec binary.
func BuildKubeconfigFromExecStatus(server string, caData []byte,
	status *clientauthenticationv1.ExecCredentialStatus) ([]byte, error) {

	kc := clientcmdv1.Config{
		APIVersion: "v1",
		Kind:       "Config",
		Clusters: []clientcmdv1.NamedCluster{{
			Name: tokenKubeconfigCluster,
			Cluster: clientcmdv1.Cluster{
				Server:                   server,
				CertificateAuthorityData: caData,
			},
		}},
		AuthInfos: []clientcmdv1.NamedAuthInfo{{
			Name: tokenKubeconfigUser,
			AuthInfo: clientcmdv1.AuthInfo{
				Token:                 status.Token,
				ClientCertificateData: []byte(status.ClientCertificateData),
				ClientKeyData:         []byte(status.ClientKeyData),
			},
		}},
		Contexts: []clientcmdv1.NamedContext{{
			Name:    tokenKubeconfigContext,
			Context: clientcmdv1.Context{Cluster: tokenKubeconfigCluster, AuthInfo: tokenKubeconfigUser},
		}},
		CurrentContext: tokenKubeconfigContext,
	}
	return yaml.Marshal(kc)
}
