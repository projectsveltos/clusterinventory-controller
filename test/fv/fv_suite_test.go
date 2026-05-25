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
	"fmt"
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/TwiN/go-color"
	ginkgotypes "github.com/onsi/ginkgo/v2/types"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
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
)

const (
	timeout         = 2 * time.Minute
	pollingInterval = 5 * time.Second
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

	var err error
	k8sClient, err = client.New(restConfig, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())

	// Build a kubeconfig that the controller (running inside the cluster) can use
	// to connect back to the cluster via kubernetes.default.svc:443.
	testClusterKubeconfig, err = buildInClusterKubeconfig(context.TODO(), restConfig)
	Expect(err).NotTo(HaveOccurred())
})

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
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: saName, Namespace: saNamespace}},
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
			Name: "local",
			Cluster: clientcmdv1.Cluster{
				Server:                   "https://kubernetes.default.svc:443",
				CertificateAuthorityData: caData,
			},
		}},
		AuthInfos: []clientcmdv1.NamedAuthInfo{{
			Name:     "local",
			AuthInfo: clientcmdv1.AuthInfo{Token: tokenResp.Status.Token},
		}},
		Contexts: []clientcmdv1.NamedContext{{
			Name:    "local",
			Context: clientcmdv1.Context{Cluster: "local", AuthInfo: "local"},
		}},
		CurrentContext: "local",
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
