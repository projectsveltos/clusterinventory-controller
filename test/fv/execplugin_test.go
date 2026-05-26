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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientcmdv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	"k8s.io/client-go/util/retry"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/yaml"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
)

var _ = Describe("ClusterProfile exec-plugin provider", Label("FV-EXECPLUGIN"), func() {
	var (
		namespace string
		cpName    string
	)

	BeforeEach(func() {
		if workloadK8sClient == nil {
			Skip("workload cluster not available; run with 'make create-cluster-fv'")
		}
		namespace = randomString()
		cpName = randomString()
		createNamespace(namespace)
	})

	AfterEach(func() {
		cp := &clusterinventoryv1alpha1.ClusterProfile{}
		if err := k8sClient.Get(context.TODO(),
			types.NamespacedName{Namespace: namespace, Name: cpName}, cp); err == nil {
			cp.Finalizers = nil
			_ = k8sClient.Update(context.TODO(), cp)
			_ = k8sClient.Delete(context.TODO(), cp)
		}
		ns := &corev1.Namespace{}
		if err := k8sClient.Get(context.TODO(), types.NamespacedName{Name: namespace}, ns); err == nil {
			_ = k8sClient.Delete(context.TODO(), ns)
		}
	})

	It("creates SveltosCluster and kubeconfig Secret using exec-plugin provider, and SveltosCluster becomes Ready", func() {
		Byf("Creating ClusterProfile %s/%s with exec-plugin access provider", namespace, cpName)
		cp := &clusterinventoryv1alpha1.ClusterProfile{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cpName,
				Namespace: namespace,
			},
			Spec: clusterinventoryv1alpha1.ClusterProfileSpec{
				ClusterManager: clusterinventoryv1alpha1.ClusterManager{Name: "fv-exec-plugin-manager"},
			},
		}
		Expect(k8sClient.Create(context.TODO(), cp)).To(Succeed())

		Byf("Setting ClusterProfile status with exec-plugin AccessProvider pointing at workload cluster")
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			currentCP := &clusterinventoryv1alpha1.ClusterProfile{}
			err := k8sClient.Get(context.TODO(), types.NamespacedName{
				Namespace: cp.Namespace, Name: cp.Name},
				currentCP)
			if err != nil {
				return err
			}
			currentCP.Status = buildExecPluginAccessProviderStatus()
			return k8sClient.Status().Update(context.TODO(), currentCP)
		})
		Expect(err).To(BeNil())

		expectedSecretName := cpName + "-sveltos-kubeconfig"

		Byf("Waiting for SveltosCluster %s/%s to be created", namespace, cpName)
		Eventually(func() bool {
			sc := &libsveltosv1beta1.SveltosCluster{}
			return k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: namespace, Name: cpName}, sc) == nil
		}, timeout, pollingInterval).Should(BeTrue())

		Byf("Verifying SveltosCluster points to the managed kubeconfig Secret")
		sc := &libsveltosv1beta1.SveltosCluster{}
		Expect(k8sClient.Get(context.TODO(),
			types.NamespacedName{Namespace: namespace, Name: cpName}, sc)).To(Succeed())
		Expect(sc.Spec.KubeconfigName).To(Equal(expectedSecretName))
		Expect(sc.Spec.KubeconfigKeyName).To(Equal("kubeconfig"))

		Byf("Waiting for kubeconfig Secret %s/%s to be created", namespace, expectedSecretName)
		Eventually(func() bool {
			secret := &corev1.Secret{}
			return k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: namespace, Name: expectedSecretName}, secret) == nil
		}, timeout, pollingInterval).Should(BeTrue())

		Byf("Verifying kubeconfig Secret contains a token-based kubeconfig for the workload cluster")
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(context.TODO(),
			types.NamespacedName{Namespace: namespace, Name: expectedSecretName}, secret)).To(Succeed())
		Expect(secret.Data["kubeconfig"]).NotTo(BeEmpty())

		// The generated kubeconfig must point at the workload cluster server, not the management cluster.
		var kc clientcmdv1.Config
		Expect(yaml.Unmarshal(secret.Data["kubeconfig"], &kc)).To(Succeed())
		Expect(kc.Clusters).NotTo(BeEmpty())
		Expect(kc.Clusters[0].Cluster.Server).To(Equal(workloadClusterServer))

		Byf("Waiting for SveltosCluster %s/%s to become Ready (reachable workload cluster)", namespace, cpName)
		Eventually(func() bool {
			sc := &libsveltosv1beta1.SveltosCluster{}
			err := k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: namespace, Name: cpName}, sc)
			if err != nil {
				return false
			}
			return sc.Status.Ready
		}, timeout, pollingInterval).Should(BeTrue())

		Byf("Deleting ClusterProfile and verifying SveltosCluster and Secret are removed")
		Expect(k8sClient.Get(context.TODO(),
			types.NamespacedName{Namespace: namespace, Name: cpName}, cp)).To(Succeed())
		Expect(k8sClient.Delete(context.TODO(), cp)).To(Succeed())

		Eventually(func() bool {
			sc := &libsveltosv1beta1.SveltosCluster{}
			return apierrors.IsNotFound(k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: namespace, Name: cpName}, sc))
		}, timeout, pollingInterval).Should(BeTrue())

		Eventually(func() bool {
			s := &corev1.Secret{}
			return apierrors.IsNotFound(k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: namespace, Name: expectedSecretName}, s))
		}, timeout, pollingInterval).Should(BeTrue())
	})
})

func buildExecPluginAccessProviderStatus() clusterinventoryv1alpha1.ClusterProfileStatus {
	// The exec-plugin extension payload carries cluster-specific config;
	// for the FV test binary it is not read, so we pass a minimal object.
	extRaw, _ := json.Marshal(map[string]string{"provider": execPluginProviderName})

	return clusterinventoryv1alpha1.ClusterProfileStatus{
		AccessProviders: []clusterinventoryv1alpha1.AccessProvider{
			{
				Name: execPluginProviderName,
				Cluster: clientcmdv1.Cluster{
					Server:                   workloadClusterServer,
					CertificateAuthorityData: workloadClusterCAData,
					Extensions: []clientcmdv1.NamedExtension{
						{
							Name:      execExtensionKey,
							Extension: runtime.RawExtension{Raw: extRaw},
						},
					},
				},
			},
		},
	}
}
