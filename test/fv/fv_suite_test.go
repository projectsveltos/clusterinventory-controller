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

package fv_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/TwiN/go-color"
	ginkgotypes "github.com/onsi/ginkgo/v2/types"
	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/util"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
)

var (
	k8sClient             client.Client
	scheme                *runtime.Scheme
	testClusterKubeconfig []byte

	// workloadK8sClient is non-nil only when the workload cluster is available
	// (i.e. create-cluster-fv was used). Exec-plugin FV tests skip when nil.
	workloadK8sClient     client.Client
	workloadClusterCAData []byte
	// workloadClusterServer is the API server URL reachable from inside the
	// management cluster pods (Docker network hostname).
	workloadClusterServer string
)

const (
	timeout         = 2 * time.Minute
	pollingInterval = 5 * time.Second

	localContextName = "local"

	controllerDeploymentName      = "clusterinventory-controller"
	controllerDeploymentNamespace = "projectsveltos"
	controllerContainerName       = "manager"

	// execPluginBinaryPath is the path where 'make create-cluster-fv' places the
	// exec-plugin binary on the kind node (docker cp) and where it is mounted
	// into the controller pod as a hostPath volume.
	execPluginBinaryPath        = "/usr/local/bin/test-exec-plugin"
	execPluginProviderConfigMap = "test-exec-plugin-provider"
	execPluginTokenSecret       = "test-exec-plugin-token"
	execPluginProviderPath      = "/etc/test-exec-plugin/provider.json"
	execPluginTokenPath         = "/var/run/secrets/test-exec-plugin/token" //nolint:gosec // not a real credential path
	execPluginProviderName      = "fv-exec-plugin"

	workloadClusterSAName      = "clusterinventory-fv-workload"
	workloadClusterSANamespace = "kube-system"

	execPluginBinaryVolume   = "exec-plugin-binary"
	execPluginProviderVolume = "exec-plugin-provider"
	execPluginTokenVolume    = "exec-plugin-token" //nolint:gosec // not a credential, just a volume name

	providerJSONKey        = "provider.json"
	tokenKey               = "token"
	rbacKindClusterRole    = "ClusterRole"
	rbacKindServiceAccount = "ServiceAccount"
	rbacAPIGroup           = "rbac.authorization.k8s.io"
	clusterAdminRole       = "cluster-admin"
	keyName                = "name"
	keyEnv                 = "env"
	keyEnvProd             = "prod"
)

func TestFv(t *testing.T) {
	RegisterFailHandler(Fail)

	suiteConfig, reporterConfig := GinkgoConfiguration()
	reporterConfig.FullTrace = true
	reporterConfig.JSONReport = "out.json"
	report := func(report ginkgotypes.Report) {
		for i := range report.SpecReports {
			specReport := report.SpecReports[i]
			if specReport.State.String() == "skipped" {
				GinkgoWriter.Printf(color.Colorize(color.Blue, fmt.Sprintf("[Skipped]: %s\n", specReport.FullText())))
			}
		}
		for i := range report.SpecReports {
			specReport := report.SpecReports[i]
			if specReport.Failed() {
				GinkgoWriter.Printf(color.Colorize(color.Red, fmt.Sprintf("[Failed]: %s\n", specReport.FullText())))
			}
		}
	}
	ReportAfterSuite("report", report)

	RunSpecs(t, "FV Suite", suiteConfig, reporterConfig)
}

var _ = BeforeSuite(func() {
	restConfig := ctrl.GetConfigOrDie()
	restConfig.QPS = 100
	restConfig.Burst = 100

	scheme = runtime.NewScheme()

	ctrl.SetLogger(klog.Background())

	Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	Expect(libsveltosv1beta1.AddToScheme(scheme)).To(Succeed())
	Expect(clusterinventoryv1alpha1.AddToScheme(scheme)).To(Succeed())
	Expect(appsv1.AddToScheme(scheme)).To(Succeed())

	var err error
	k8sClient, err = client.New(restConfig, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())

	// Build a kubeconfig that the controller (running inside the cluster) can use
	// to connect back to the cluster via kubernetes.default.svc:443.
	testClusterKubeconfig, err = buildInClusterKubeconfig(context.TODO(), restConfig)
	Expect(err).NotTo(HaveOccurred())

	// Set up exec-plugin test infrastructure if the workload cluster kubeconfig exists.
	// It is written by the create-cluster-fv Makefile target.
	if _, statErr := os.Stat("workload_kubeconfig"); statErr == nil {
		workloadRestConfig, loadErr := clientcmd.BuildConfigFromFlags("", "workload_kubeconfig")
		Expect(loadErr).NotTo(HaveOccurred())
		workloadRestConfig.QPS = 100
		workloadRestConfig.Burst = 100

		workloadK8sClient, err = client.New(workloadRestConfig, client.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred())

		workloadClusterCAData = workloadRestConfig.CAData
		if len(workloadClusterCAData) == 0 && workloadRestConfig.CAFile != "" {
			workloadClusterCAData, err = os.ReadFile(workloadRestConfig.CAFile)
			Expect(err).NotTo(HaveOccurred())
		}

		// The workload cluster's API server is reachable from inside the management
		// cluster pods via the Docker network using the container hostname.
		workloadClusterServer = fmt.Sprintf("https://%s-control-plane:6443", "fv-workload")

		Expect(setupExecPluginInfrastructure(context.TODO(), restConfig, workloadRestConfig)).To(Succeed())
	}
})

var _ = AfterSuite(func() {
	if workloadK8sClient == nil {
		return
	}
	teardownExecPluginInfrastructure(context.TODO())
})

// setupExecPluginInfrastructure creates all Kubernetes objects needed for exec-plugin FV tests
// and patches the controller Deployment to make the exec binary and provider file available.
// The binary itself is already on the kind node at execPluginBinaryPath, placed there by
// 'make create-cluster-fv' via docker cp.
func setupExecPluginInfrastructure(ctx context.Context, mgmtCfg, workloadCfg *rest.Config) error {
	// 1. Create a service account in the workload cluster and get a token.
	token, err := buildWorkloadClusterToken(ctx, workloadCfg)
	if err != nil {
		return fmt.Errorf("building workload cluster token: %w", err)
	}

	// 2. Provider JSON that points the controller at our test exec binary.
	providerJSON, err := buildProviderJSON()
	if err != nil {
		return fmt.Errorf("building provider JSON: %w", err)
	}

	// 3. Create objects in the management cluster.
	if err := createOrUpdateConfigMap(ctx, execPluginProviderConfigMap, controllerDeploymentNamespace,
		map[string]string{providerJSONKey: string(providerJSON)}, nil); err != nil {
		return fmt.Errorf("creating provider ConfigMap: %w", err)
	}
	if err := createOrUpdateSecret(ctx, execPluginTokenSecret, controllerDeploymentNamespace,
		map[string][]byte{tokenKey: []byte(token)}); err != nil {
		return fmt.Errorf("creating token Secret: %w", err)
	}

	// 4. Patch the controller Deployment to mount the binary (hostPath), provider file, and token.
	if err := patchControllerDeployment(ctx); err != nil {
		return fmt.Errorf("patching controller Deployment: %w", err)
	}

	// 5. Wait for the Deployment rollout to complete.
	mgmtClientset, err := kubernetes.NewForConfig(mgmtCfg)
	if err != nil {
		return fmt.Errorf("building management clientset: %w", err)
	}
	return waitForDeploymentRollout(ctx, mgmtClientset)
}

// teardownExecPluginInfrastructure removes the objects created by setupExecPluginInfrastructure
// and restores the controller Deployment.
func teardownExecPluginInfrastructure(ctx context.Context) {
	_ = k8sClient.Delete(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: execPluginProviderConfigMap, Namespace: controllerDeploymentNamespace},
	})
	_ = k8sClient.Delete(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: execPluginTokenSecret, Namespace: controllerDeploymentNamespace},
	})
	restoreControllerDeployment(ctx)
}

func buildWorkloadClusterToken(ctx context.Context, cfg *rest.Config) (string, error) {
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", fmt.Errorf("building workload clientset: %w", err)
	}

	_, err = clientset.CoreV1().ServiceAccounts(workloadClusterSANamespace).Create(ctx,
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: workloadClusterSAName, Namespace: workloadClusterSANamespace}},
		metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("creating workload SA: %w", err)
	}

	_, err = clientset.RbacV1().ClusterRoleBindings().Create(ctx,
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: workloadClusterSAName + "-admin"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacAPIGroup, Kind: rbacKindClusterRole, Name: clusterAdminRole},
			Subjects:   []rbacv1.Subject{{Kind: rbacKindServiceAccount, Name: workloadClusterSAName, Namespace: workloadClusterSANamespace}},
		}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("creating workload ClusterRoleBinding: %w", err)
	}

	expiry := int64(7200)
	tokenResp, err := clientset.CoreV1().ServiceAccounts(workloadClusterSANamespace).CreateToken(ctx,
		workloadClusterSAName,
		&authv1.TokenRequest{Spec: authv1.TokenRequestSpec{ExpirationSeconds: &expiry}},
		metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("creating workload token: %w", err)
	}
	return tokenResp.Status.Token, nil
}

func buildProviderJSON() ([]byte, error) {
	provider := map[string]interface{}{
		"providers": []map[string]interface{}{
			{
				keyName: execPluginProviderName,
				"execConfig": map[string]interface{}{
					"command":            execPluginBinaryPath,
					"apiVersion":         "client.authentication.k8s.io/v1",
					"args":               []string{},
					keyEnv:               []interface{}{},
					"provideClusterInfo": false,
				},
			},
		},
	}
	return json.Marshal(provider)
}

func createOrUpdateConfigMap(ctx context.Context, name, namespace string,
	data map[string]string, binaryData map[string][]byte) error {

	cm := &corev1.ConfigMap{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, cm)
	if apierrors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Data:       data,
			BinaryData: binaryData,
		}
		return k8sClient.Create(ctx, cm)
	}
	if err != nil {
		return err
	}
	cm.Data = data
	cm.BinaryData = binaryData
	return k8sClient.Update(ctx, cm)
}

func createOrUpdateSecret(ctx context.Context, name, namespace string, data map[string][]byte) error {
	secret := &corev1.Secret{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, secret)
	if apierrors.IsNotFound(err) {
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Data:       data,
		}
		return k8sClient.Create(ctx, secret)
	}
	if err != nil {
		return err
	}
	secret.Data = data
	return k8sClient.Update(ctx, secret)
}

func patchControllerDeployment(ctx context.Context) error {
	deploy := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx,
		types.NamespacedName{Name: controllerDeploymentName, Namespace: controllerDeploymentNamespace},
		deploy); err != nil {
		return err
	}

	hostPathFile := corev1.HostPathFile
	// Add volumes.
	deploy.Spec.Template.Spec.Volumes = append(deploy.Spec.Template.Spec.Volumes,
		corev1.Volume{
			// Binary is placed on the kind node by 'make create-cluster-fv' via docker cp.
			Name: execPluginBinaryVolume,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: execPluginBinaryPath,
					Type: &hostPathFile,
				},
			},
		},
		corev1.Volume{
			Name: execPluginProviderVolume,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: execPluginProviderConfigMap},
				},
			},
		},
		corev1.Volume{
			Name: execPluginTokenVolume,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: execPluginTokenSecret,
				},
			},
		},
	)

	// Add volume mounts and provider file arg to the manager container.
	for i := range deploy.Spec.Template.Spec.Containers {
		c := &deploy.Spec.Template.Spec.Containers[i]
		if c.Name != controllerContainerName {
			continue
		}
		c.VolumeMounts = append(c.VolumeMounts,
			corev1.VolumeMount{
				Name:      execPluginBinaryVolume,
				MountPath: execPluginBinaryPath,
			},
			corev1.VolumeMount{
				Name:      execPluginProviderVolume,
				MountPath: execPluginProviderPath,
				SubPath:   providerJSONKey,
			},
			corev1.VolumeMount{
				Name:      execPluginTokenVolume,
				MountPath: execPluginTokenPath,
				SubPath:   tokenKey,
			},
		)
		c.Args = append(c.Args, "--clusterprofile-provider-file="+execPluginProviderPath)
	}

	return k8sClient.Update(ctx, deploy)
}

func restoreControllerDeployment(ctx context.Context) {
	deploy := &appsv1.Deployment{}
	if err := k8sClient.Get(ctx,
		types.NamespacedName{Name: controllerDeploymentName, Namespace: controllerDeploymentNamespace},
		deploy); err != nil {
		return
	}

	// Remove our volumes.
	testVolumeNames := map[string]bool{
		execPluginBinaryVolume:   true,
		execPluginProviderVolume: true,
		execPluginTokenVolume:    true,
	}
	filtered := deploy.Spec.Template.Spec.Volumes[:0]
	for i := range deploy.Spec.Template.Spec.Volumes {
		v := &deploy.Spec.Template.Spec.Volumes[i]
		if !testVolumeNames[v.Name] {
			filtered = append(filtered, *v)
		}
	}
	deploy.Spec.Template.Spec.Volumes = filtered

	for i := range deploy.Spec.Template.Spec.Containers {
		c := &deploy.Spec.Template.Spec.Containers[i]
		if c.Name != controllerContainerName {
			continue
		}
		mounts := c.VolumeMounts[:0]
		for _, m := range c.VolumeMounts {
			if !testVolumeNames[m.Name] {
				mounts = append(mounts, m)
			}
		}
		c.VolumeMounts = mounts

		args := c.Args[:0]
		for _, a := range c.Args {
			if a != "--clusterprofile-provider-file="+execPluginProviderPath {
				args = append(args, a)
			}
		}
		c.Args = args
	}

	_ = k8sClient.Update(ctx, deploy)
}

func waitForDeploymentRollout(ctx context.Context, clientset *kubernetes.Clientset) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		deploy, err := clientset.AppsV1().Deployments(controllerDeploymentNamespace).
			Get(ctx, controllerDeploymentName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if deploy.Status.UpdatedReplicas == *deploy.Spec.Replicas &&
			deploy.Status.ReadyReplicas == *deploy.Spec.Replicas &&
			deploy.Status.ObservedGeneration >= deploy.Generation {

			return nil
		}
		time.Sleep(pollingInterval)
	}
	return fmt.Errorf("timed out waiting for deployment %s/%s rollout",
		controllerDeploymentNamespace, controllerDeploymentName)
}

// buildInClusterKubeconfig creates a ServiceAccount with cluster-admin rights and
// returns a kubeconfig that uses https://kubernetes.default.svc:443 as the server.
// This kubeconfig is valid from inside the cluster (e.g., from a controller Pod).
func buildInClusterKubeconfig(ctx context.Context, cfg *rest.Config) ([]byte, error) {
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building kubernetes clientset: %w", err)
	}

	const (
		saName      = "clusterinventory-fv"
		saNamespace = "kube-system"
	)

	_, err = clientset.CoreV1().ServiceAccounts(saNamespace).Create(ctx,
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: saNamespace}},
		metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("creating serviceaccount: %w", err)
	}

	_, err = clientset.RbacV1().ClusterRoleBindings().Create(ctx,
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "clusterinventory-fv-admin"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacAPIGroup, Kind: rbacKindClusterRole, Name: clusterAdminRole},
			Subjects:   []rbacv1.Subject{{Kind: rbacKindServiceAccount, Name: saName, Namespace: saNamespace}},
		}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("creating clusterrolebinding: %w", err)
	}

	expiry := int64(7200)
	tokenResp, err := clientset.CoreV1().ServiceAccounts(saNamespace).CreateToken(ctx, saName,
		&authv1.TokenRequest{Spec: authv1.TokenRequestSpec{ExpirationSeconds: &expiry}},
		metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating token: %w", err)
	}

	caData := cfg.CAData
	if len(caData) == 0 && cfg.CAFile != "" {
		caData, err = os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading CA file: %w", err)
		}
	}

	kc := clientcmdv1.Config{
		APIVersion: "v1",
		Kind:       "Config",
		Clusters: []clientcmdv1.NamedCluster{{
			Name: localContextName,
			Cluster: clientcmdv1.Cluster{
				Server:                   "https://kubernetes.default.svc:443",
				CertificateAuthorityData: caData,
			},
		}},
		AuthInfos: []clientcmdv1.NamedAuthInfo{{
			Name:     localContextName,
			AuthInfo: clientcmdv1.AuthInfo{Token: tokenResp.Status.Token},
		}},
		Contexts: []clientcmdv1.NamedContext{{
			Name:    localContextName,
			Context: clientcmdv1.Context{Cluster: localContextName, AuthInfo: localContextName},
		}},
		CurrentContext: localContextName,
	}

	return yaml.Marshal(kc)
}

// Byf is a simple wrapper around By.
func Byf(format string, a ...interface{}) {
	By(fmt.Sprintf(format, a...)) // ignore_by_check
}

func randomString() string {
	const length = 10
	return util.RandomString(length)
}

// createNamespace creates a test namespace and returns a cleanup function.
func createNamespace(name string) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	Expect(k8sClient.Create(context.TODO(), ns)).To(Succeed())
}
