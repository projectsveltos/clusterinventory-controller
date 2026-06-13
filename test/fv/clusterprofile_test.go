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
	"bytes"
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
	"sigs.k8s.io/controller-runtime/pkg/client"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
)

const (
	execExtensionKey               = "client.authentication.k8s.io/exec"
	kubeconfigSecretReaderProvider = "kubeconfig-secretreader" //nolint:gosec // not a credential, just a provider name
)

var _ = Describe("ClusterProfile controller", Label("FV"), func() {
	var (
		namespace     string
		cpName        string
		srcSecretName string
	)

	BeforeEach(func() {
		namespace = randomString()
		cpName = randomString()
		srcSecretName = randomString()
		createNamespace(namespace)
	})

	AfterEach(func() {
		// Best-effort cleanup; test may have already deleted resources.
		cp := &clusterinventoryv1alpha1.ClusterProfile{}
		if err := k8sClient.Get(context.TODO(),
			types.NamespacedName{Namespace: namespace, Name: cpName}, cp); err == nil {
			// Remove finalizer so the API server can delete it immediately.
			cp.Finalizers = nil
			_ = k8sClient.Update(context.TODO(), cp)
			_ = k8sClient.Delete(context.TODO(), cp)
		}
		ns := &corev1.Namespace{}
		if err := k8sClient.Get(context.TODO(), types.NamespacedName{Name: namespace}, ns); err == nil {
			_ = k8sClient.Delete(context.TODO(), ns)
		}
	})

	It("creates SveltosCluster and kubeconfig Secret when a ClusterProfile is created", func() {
		kubeconfigData := testClusterKubeconfig

		Byf("Creating source kubeconfig Secret %s/%s", namespace, srcSecretName)
		srcSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: srcSecretName, Namespace: namespace},
			Data:       map[string][]byte{"kubeconfig": kubeconfigData},
		}
		Expect(k8sClient.Create(context.TODO(), srcSecret)).To(Succeed())

		Byf("Creating ClusterProfile %s/%s with kubeconfig-secretreader provider and initial labels", namespace, cpName)
		cp := buildFvClusterProfile(cpName, namespace)
		cp.Labels = map[string]string{keyEnv: "staging"}
		Expect(k8sClient.Create(context.TODO(), cp)).To(Succeed())

		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			currentCP := &clusterinventoryv1alpha1.ClusterProfile{}
			err := k8sClient.Get(context.TODO(), types.NamespacedName{
				Namespace: cp.Namespace, Name: cp.Name},
				currentCP)
			if err != nil {
				return err
			}
			currentCP.Status = buildFvAccessProviderStatus(srcSecretName, "kubeconfig", namespace)
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

		Byf("Verifying SveltosCluster has labels copied from ClusterProfile")
		Expect(sc.Labels[keyEnv]).To(Equal("staging"))

		Byf("Waiting for SveltosCluster %s/%s to be ready", namespace, cpName)
		Eventually(func() bool {
			sc := &libsveltosv1beta1.SveltosCluster{}
			err := k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: namespace, Name: cpName}, sc)
			if err != nil {
				return false
			}
			return sc.Status.Ready
		}, timeout, pollingInterval).Should(BeTrue())

		Byf("Waiting for managed kubeconfig Secret %s/%s to be created", namespace, expectedSecretName)
		Eventually(func() bool {
			secret := &corev1.Secret{}
			return k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: namespace, Name: expectedSecretName}, secret) == nil
		}, timeout, pollingInterval).Should(BeTrue())

		Byf("Verifying managed Secret contains the kubeconfig data")
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(context.TODO(),
			types.NamespacedName{Namespace: namespace, Name: expectedSecretName}, secret)).To(Succeed())
		Expect(secret.Data["kubeconfig"]).To(Equal(kubeconfigData))

		Byf("Updating source kubeconfig Secret and verifying managed Secret is updated")
		updatedKubeconfig := []byte("updated-fake-kubeconfig")
		Expect(updateSecretData(srcSecret, "kubeconfig", updatedKubeconfig)).To(Succeed())

		// Trigger reconciliation by updating labels on the ClusterProfile.
		// The controller predicate returns true when labels change.
		Expect(k8sClient.Get(context.TODO(),
			types.NamespacedName{Namespace: namespace, Name: cpName}, cp)).To(Succeed())
		cp.Labels[keyEnv] = keyEnvProd
		Expect(k8sClient.Update(context.TODO(), cp)).To(Succeed())

		Eventually(func() bool {
			s := &corev1.Secret{}
			if err := k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: namespace, Name: expectedSecretName}, s); err != nil {
				return false
			}
			return bytes.Equal(s.Data["kubeconfig"], updatedKubeconfig)
		}, timeout, pollingInterval).Should(BeTrue())

		Byf("Verifying SveltosCluster labels are updated after ClusterProfile label change")
		Eventually(func() string {
			current := &libsveltosv1beta1.SveltosCluster{}
			if err := k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: namespace, Name: cpName}, current); err != nil {
				return ""
			}
			return current.Labels[keyEnv]
		}, timeout, pollingInterval).Should(Equal(keyEnvProd))

		Byf("Deleting ClusterProfile and verifying SveltosCluster and managed Secret are removed")
		Expect(k8sClient.Get(context.TODO(),
			types.NamespacedName{Namespace: namespace, Name: cpName}, cp)).To(Succeed())
		Expect(k8sClient.Delete(context.TODO(), cp)).To(Succeed())

		Eventually(func() bool {
			sc := &libsveltosv1beta1.SveltosCluster{}
			err := k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: namespace, Name: cpName}, sc)
			return apierrors.IsNotFound(err)
		}, timeout, pollingInterval).Should(BeTrue())

		Eventually(func() bool {
			s := &corev1.Secret{}
			err := k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: namespace, Name: expectedSecretName}, s)
			return apierrors.IsNotFound(err)
		}, timeout, pollingInterval).Should(BeTrue())
	})
})

func buildFvClusterProfile(name, namespace string) *clusterinventoryv1alpha1.ClusterProfile {
	return &clusterinventoryv1alpha1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: clusterinventoryv1alpha1.ClusterProfileSpec{
			ClusterManager: clusterinventoryv1alpha1.ClusterManager{Name: "fv-manager"},
		},
	}
}

func buildFvAccessProviderStatus(secretName, secretKey, namespace string) clusterinventoryv1alpha1.ClusterProfileStatus {
	extPayload := map[string]string{
		keyName:     secretName,
		"key":       secretKey,
		"namespace": namespace,
	}
	extRaw, _ := json.Marshal(extPayload)

	return clusterinventoryv1alpha1.ClusterProfileStatus{
		AccessProviders: []clusterinventoryv1alpha1.AccessProvider{
			{
				Name: kubeconfigSecretReaderProvider,
				Cluster: clientcmdv1.Cluster{
					Server: "https://fake-server:6443",
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

func updateSecretData(secret *corev1.Secret, key string, data []byte) error {
	return retryOnConflict(func() error {
		current := &corev1.Secret{}
		if err := k8sClient.Get(context.TODO(),
			client.ObjectKeyFromObject(secret), current); err != nil {
			return err
		}
		current.Data[key] = data
		return k8sClient.Update(context.TODO(), current)
	})
}

func retryOnConflict(fn func() error) error {
	const maxRetries = 5
	for i := 0; i < maxRetries; i++ {
		err := fn()
		if err == nil || !apierrors.IsConflict(err) {
			return err
		}
	}
	return fn()
}
