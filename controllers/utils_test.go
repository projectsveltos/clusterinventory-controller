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

package controller_test

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
	clientauthenticationv1 "k8s.io/client-go/pkg/apis/clientauthentication/v1"
	clientcmdv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/yaml"

	controller "github.com/projectsveltos/clusterinventory-controller/controllers"
	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
)

const (
	// secretReaderNameField and secretReaderKeyField are the JSON field names for secretReaderConfig.
	secretReaderNameField = "name"
	secretReaderKeyField  = "key"
	// srcSecretKey is the key name used in source Secrets in tests.
	srcSecretKey = "kubeconfig"
	labelKeyEnv  = "env"
	labelEnvProd = "prod"

	srcSecretRefName      = "my-secret"
	externalAnnotKey      = "other.io/annotation"
	externalAnnotValue    = "keep-me"
	displayNameAddedLater = "added-later"
)

var _ = Describe("Utils", func() {
	var namespace string

	BeforeEach(func() {
		namespace = randomString()
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(testEnv.Create(context.TODO(), ns)).To(Succeed())
		Expect(waitForObject(context.TODO(), testEnv.Client, ns)).To(Succeed())
	})

	AfterEach(func() {
		ns := &corev1.Namespace{}
		Expect(testEnv.Get(context.TODO(), types.NamespacedName{Name: namespace}, ns)).To(Succeed())
		Expect(testEnv.Delete(context.TODO(), ns)).To(Succeed())
	})

	Context("kubeconfigSecretName", func() {
		It("appends sveltos-kubeconfig suffix", func() {
			name := controller.KubeconfigSecretName("my-cluster")
			Expect(name).To(Equal("my-cluster-sveltos-kubeconfig"))
		})
	})

	Context("sourceSecretNamespacedName", func() {
		It("returns namespace/name using the explicit namespace in the extension", func() {
			extRaw, _ := json.Marshal(map[string]string{secretReaderNameField: srcSecretRefName, secretReaderKeyField: srcSecretKey, "namespace": "other-ns"})
			cp := buildClusterProfile("test", namespace, controller.KubeconfigSecretReaderProvider, extRaw)
			Expect(controller.SourceSecretNamespacedName(cp)).To(Equal("other-ns/" + srcSecretRefName))
		})

		It("defaults namespace to ClusterProfile namespace when not set in the extension", func() {
			extRaw, _ := json.Marshal(map[string]string{secretReaderNameField: srcSecretRefName, secretReaderKeyField: srcSecretKey})
			cp := buildClusterProfile("test", namespace, controller.KubeconfigSecretReaderProvider, extRaw)
			Expect(controller.SourceSecretNamespacedName(cp)).To(Equal(namespace + "/" + srcSecretRefName))
		})

		It("returns empty string when no kubeconfig-secretreader provider is present", func() {
			cp := buildClusterProfile("test", namespace, "other-provider", nil)
			Expect(controller.SourceSecretNamespacedName(cp)).To(BeEmpty())
		})

		It("returns empty string when no access provider is set at all", func() {
			cp := buildClusterProfile("test", namespace, "", nil)
			Expect(controller.SourceSecretNamespacedName(cp)).To(BeEmpty())
		})
	})

	Context("findAccessProvider", func() {
		It("finds provider in AccessProviders", func() {
			cp := buildClusterProfile("test", namespace, "my-provider", nil)
			provider := controller.FindAccessProvider(cp, "my-provider")
			Expect(provider).NotTo(BeNil())
			Expect(provider.Name).To(Equal("my-provider"))
		})

		It("falls back to CredentialProviders when not in AccessProviders", func() {
			cp := &clusterinventoryv1alpha1.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: namespace},
				Status: clusterinventoryv1alpha1.ClusterProfileStatus{
					CredentialProviders: []clusterinventoryv1alpha1.AccessProvider{
						{Name: "legacy-provider"},
					},
				},
			}
			provider := controller.FindAccessProvider(cp, "legacy-provider")
			Expect(provider).NotTo(BeNil())
		})

		It("returns nil when provider is not found", func() {
			cp := buildClusterProfile("test", namespace, "other", nil)
			Expect(controller.FindAccessProvider(cp, "not-there")).To(BeNil())
		})
	})

	Context("getKubeconfigFromSecretReader", func() {
		It("returns kubeconfig bytes from the referenced Secret", func() {
			kubeconfig := []byte("fake-kubeconfig-data")
			srcSecret := buildSourceSecret("src-secret", namespace, srcSecretKey, kubeconfig)
			Expect(testEnv.Create(context.TODO(), srcSecret)).To(Succeed())
			Expect(waitForObject(context.TODO(), testEnv.Client, srcSecret)).To(Succeed())

			extRaw, err := json.Marshal(map[string]string{secretReaderNameField: "src-secret", secretReaderKeyField: srcSecretKey})
			Expect(err).To(BeNil())
			provider := &clusterinventoryv1alpha1.AccessProvider{
				Name: controller.KubeconfigSecretReaderProvider,
				Cluster: clientcmdv1.Cluster{
					Extensions: []clientcmdv1.NamedExtension{
						{Name: controller.ExecExtensionKey, Extension: runtime.RawExtension{Raw: extRaw}},
					},
				},
			}

			result, err := controller.GetKubeconfigFromSecretReader(context.TODO(), testEnv.Client, namespace, provider)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(kubeconfig))
		})

		It("returns error when exec extension is missing", func() {
			provider := &clusterinventoryv1alpha1.AccessProvider{
				Name:    controller.KubeconfigSecretReaderProvider,
				Cluster: clientcmdv1.Cluster{},
			}
			_, err := controller.GetKubeconfigFromSecretReader(context.TODO(), testEnv.Client, namespace, provider)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(controller.ExecExtensionKey))
		})

		It("returns error when source Secret does not exist", func() {
			extRaw, _ := json.Marshal(map[string]string{secretReaderNameField: "nonexistent", secretReaderKeyField: srcSecretKey})
			provider := &clusterinventoryv1alpha1.AccessProvider{
				Name: controller.KubeconfigSecretReaderProvider,
				Cluster: clientcmdv1.Cluster{
					Extensions: []clientcmdv1.NamedExtension{
						{Name: controller.ExecExtensionKey, Extension: runtime.RawExtension{Raw: extRaw}},
					},
				},
			}
			_, err := controller.GetKubeconfigFromSecretReader(context.TODO(), testEnv.Client, namespace, provider)
			Expect(err).To(HaveOccurred())
		})
	})

	Context("getKubeconfig", func() {
		It("returns error when no supported provider is present", func() {
			cp := buildClusterProfile("test", namespace, "unsupported-provider", nil)
			_, _, err := controller.GetKubeconfig(context.TODO(), testEnv.Client, nil, cp, logger)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no supported access provider"))
		})

		It("routes to secretreader when provider matches", func() {
			kubeconfig := []byte("my-kubeconfig")
			srcSecret := buildSourceSecret("src", namespace, srcSecretKey, kubeconfig)
			Expect(testEnv.Create(context.TODO(), srcSecret)).To(Succeed())
			Expect(waitForObject(context.TODO(), testEnv.Client, srcSecret)).To(Succeed())

			extRaw, _ := json.Marshal(map[string]string{secretReaderNameField: "src", secretReaderKeyField: srcSecretKey})
			cp := buildClusterProfile("test2", namespace, controller.KubeconfigSecretReaderProvider, extRaw)

			result, _, err := controller.GetKubeconfig(context.TODO(), testEnv.Client, nil, cp, logger)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(kubeconfig))
		})
	})

	Context("reconcileKubeconfigSecret", func() {
		It("creates a new Secret when one does not exist", func() {
			cpName := randomString()
			cp := buildClusterProfile(cpName, namespace, "", nil)
			kubeconfigData := []byte("kubeconfig-data")

			Expect(controller.ReconcileKubeconfigSecret(context.TODO(), testEnv.Client, cp, kubeconfigData, logger)).To(Succeed())

			secret := &corev1.Secret{}
			Eventually(func() error {
				return testEnv.Get(context.TODO(),
					types.NamespacedName{Namespace: namespace, Name: controller.KubeconfigSecretName(cpName)},
					secret)
			}, timeout, pollingInterval).Should(Succeed())
			Expect(secret.Data[controller.KubeconfigKey]).To(Equal(kubeconfigData))
			Expect(secret.Labels[controller.ManagedByLabel]).To(Equal(controller.ManagedByValue))
		})

		It("updates an existing Secret when kubeconfig changes", func() {
			cpName := randomString()
			cp := buildClusterProfile(cpName, namespace, "", nil)

			Expect(controller.ReconcileKubeconfigSecret(context.TODO(), testEnv.Client, cp, []byte("v1"), logger)).To(Succeed())
			// Wait for the cache to reflect the created Secret before calling again,
			// so the second call's internal Get finds it and proceeds to Update.
			Expect(waitForObject(context.TODO(), testEnv.Client, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: controller.KubeconfigSecretName(cpName), Namespace: namespace},
			})).To(Succeed())
			Expect(controller.ReconcileKubeconfigSecret(context.TODO(), testEnv.Client, cp, []byte("v2"), logger)).To(Succeed())

			// Wait for the Update event to propagate to the cache.
			secret := &corev1.Secret{}
			Eventually(func() []byte {
				if err := testEnv.Get(context.TODO(),
					types.NamespacedName{Namespace: namespace, Name: controller.KubeconfigSecretName(cpName)},
					secret); err != nil {
					return nil
				}
				return secret.Data[controller.KubeconfigKey]
			}, timeout, pollingInterval).Should(Equal([]byte("v2")))
		})

		It("is idempotent when kubeconfig has not changed", func() {
			cpName := randomString()
			cp := buildClusterProfile(cpName, namespace, "", nil)
			data := []byte("unchanged")

			Expect(controller.ReconcileKubeconfigSecret(context.TODO(), testEnv.Client, cp, data, logger)).To(Succeed())
			Expect(controller.ReconcileKubeconfigSecret(context.TODO(), testEnv.Client, cp, data, logger)).To(Succeed())
		})
	})

	Context("reconcileSveltosCluster", func() {
		It("creates a SveltosCluster pointing to the managed Secret", func() {
			cpName := randomString()
			cp := buildClusterProfile(cpName, namespace, "", nil)

			Expect(controller.ReconcileSveltosCluster(context.TODO(), testEnv.Client, cp, logger)).To(Succeed())

			// The SveltosCluster CRD was just installed; wait for the cache to sync.
			sc := &libsveltosv1beta1.SveltosCluster{}
			Eventually(func() error {
				return testEnv.Get(context.TODO(),
					types.NamespacedName{Namespace: namespace, Name: cpName}, sc)
			}, timeout, pollingInterval).Should(Succeed())
			Expect(sc.Spec.KubeconfigName).To(Equal(controller.KubeconfigSecretName(cpName)))
			Expect(sc.Spec.KubeconfigKeyName).To(Equal(controller.KubeconfigKey))
			Expect(sc.Labels[controller.ManagedByLabel]).To(Equal(controller.ManagedByValue))
		})

		It("copies ClusterProfile labels onto the SveltosCluster at creation", func() {
			cpName := randomString()
			cp := buildClusterProfile(cpName, namespace, "", nil)
			cp.Labels = map[string]string{labelKeyEnv: labelEnvProd, "region": "us-east-1"}

			Expect(controller.ReconcileSveltosCluster(context.TODO(), testEnv.Client, cp, logger)).To(Succeed())

			sc := &libsveltosv1beta1.SveltosCluster{}
			Eventually(func() error {
				return testEnv.Get(context.TODO(),
					types.NamespacedName{Namespace: namespace, Name: cpName}, sc)
			}, timeout, pollingInterval).Should(Succeed())
			Expect(sc.Labels[labelKeyEnv]).To(Equal(labelEnvProd))
			Expect(sc.Labels["region"]).To(Equal("us-east-1"))
			Expect(sc.Labels[controller.ManagedByLabel]).To(Equal(controller.ManagedByValue))
		})

		It("updates SveltosCluster labels when ClusterProfile labels change", func() {
			cpName := randomString()
			cp := buildClusterProfile(cpName, namespace, "", nil)
			cp.Labels = map[string]string{labelKeyEnv: "staging"}

			Expect(controller.ReconcileSveltosCluster(context.TODO(), testEnv.Client, cp, logger)).To(Succeed())
			Expect(waitForObject(context.TODO(), testEnv.Client, &libsveltosv1beta1.SveltosCluster{
				ObjectMeta: metav1.ObjectMeta{Name: cpName, Namespace: namespace},
			})).To(Succeed())

			cp.Labels = map[string]string{labelKeyEnv: labelEnvProd}
			Expect(controller.ReconcileSveltosCluster(context.TODO(), testEnv.Client, cp, logger)).To(Succeed())

			sc := &libsveltosv1beta1.SveltosCluster{}
			Eventually(func() string {
				if err := testEnv.Get(context.TODO(),
					types.NamespacedName{Namespace: namespace, Name: cpName}, sc); err != nil {
					return ""
				}
				return sc.Labels[labelKeyEnv]
			}, timeout, pollingInterval).Should(Equal(labelEnvProd))
		})

		It("is idempotent on repeated calls", func() {
			cpName := randomString()
			cp := buildClusterProfile(cpName, namespace, "", nil)

			Expect(controller.ReconcileSveltosCluster(context.TODO(), testEnv.Client, cp, logger)).To(Succeed())
			Expect(controller.ReconcileSveltosCluster(context.TODO(), testEnv.Client, cp, logger)).To(Succeed())
		})

		It("sets the display-name annotation when Spec.DisplayName is non-empty", func() {
			cpName := randomString()
			cp := buildClusterProfile(cpName, namespace, "", nil)
			cp.Spec.DisplayName = "My Test Cluster"

			Expect(controller.ReconcileSveltosCluster(context.TODO(), testEnv.Client, cp, logger)).To(Succeed())

			sc := &libsveltosv1beta1.SveltosCluster{}
			Eventually(func() error {
				return testEnv.Get(context.TODO(),
					types.NamespacedName{Namespace: namespace, Name: cpName}, sc)
			}, timeout, pollingInterval).Should(Succeed())
			Expect(sc.Annotations[controller.DisplayNameAnnotation]).To(Equal("My Test Cluster"))
		})

		It("updates the display-name annotation when DisplayName changes", func() {
			cpName := randomString()
			cp := buildClusterProfile(cpName, namespace, "", nil)
			cp.Spec.DisplayName = "v1"

			Expect(controller.ReconcileSveltosCluster(context.TODO(), testEnv.Client, cp, logger)).To(Succeed())
			Expect(waitForObject(context.TODO(), testEnv.Client, &libsveltosv1beta1.SveltosCluster{
				ObjectMeta: metav1.ObjectMeta{Name: cpName, Namespace: namespace},
			})).To(Succeed())

			cp.Spec.DisplayName = "v2"
			Expect(controller.ReconcileSveltosCluster(context.TODO(), testEnv.Client, cp, logger)).To(Succeed())

			sc := &libsveltosv1beta1.SveltosCluster{}
			Eventually(func() string {
				if err := testEnv.Get(context.TODO(),
					types.NamespacedName{Namespace: namespace, Name: cpName}, sc); err != nil {
					return ""
				}
				return sc.Annotations[controller.DisplayNameAnnotation]
			}, timeout, pollingInterval).Should(Equal("v2"))
		})

		It("removes the display-name annotation when DisplayName is cleared", func() {
			cpName := randomString()
			cp := buildClusterProfile(cpName, namespace, "", nil)
			cp.Spec.DisplayName = "to-be-cleared"

			Expect(controller.ReconcileSveltosCluster(context.TODO(), testEnv.Client, cp, logger)).To(Succeed())
			Expect(waitForObject(context.TODO(), testEnv.Client, &libsveltosv1beta1.SveltosCluster{
				ObjectMeta: metav1.ObjectMeta{Name: cpName, Namespace: namespace},
			})).To(Succeed())

			cp.Spec.DisplayName = ""
			Expect(controller.ReconcileSveltosCluster(context.TODO(), testEnv.Client, cp, logger)).To(Succeed())

			sc := &libsveltosv1beta1.SveltosCluster{}
			Eventually(func() bool {
				if err := testEnv.Get(context.TODO(),
					types.NamespacedName{Namespace: namespace, Name: cpName}, sc); err != nil {
					return false
				}
				_, exists := sc.Annotations[controller.DisplayNameAnnotation]
				return !exists
			}, timeout, pollingInterval).Should(BeTrue())
		})

	})

	// applyDisplayName is a pure function; test it without the API server.
	Context("applyDisplayName", func() {
		It("sets the display-name annotation on a nil map", func() {
			result := controller.ApplyDisplayName(nil, "my-cluster")
			Expect(result[controller.DisplayNameAnnotation]).To(Equal("my-cluster"))
		})

		It("preserves existing annotations when adding display-name", func() {
			existing := map[string]string{externalAnnotKey: externalAnnotValue}
			result := controller.ApplyDisplayName(existing, displayNameAddedLater)
			Expect(result[externalAnnotKey]).To(Equal(externalAnnotValue))
			Expect(result[controller.DisplayNameAnnotation]).To(Equal(displayNameAddedLater))
		})

		It("removes only the display-name annotation when display name is cleared", func() {
			existing := map[string]string{
				externalAnnotKey:                 externalAnnotValue,
				controller.DisplayNameAnnotation: "old-name",
			}
			result := controller.ApplyDisplayName(existing, "")
			Expect(result[externalAnnotKey]).To(Equal(externalAnnotValue))
			_, exists := result[controller.DisplayNameAnnotation]
			Expect(exists).To(BeFalse())
		})

		It("returns nil when the result would be empty", func() {
			Expect(controller.ApplyDisplayName(nil, "")).To(BeNil())
		})

		It("does not mutate the input map", func() {
			existing := map[string]string{externalAnnotKey: externalAnnotValue}
			_ = controller.ApplyDisplayName(existing, displayNameAddedLater)
			Expect(existing).To(HaveLen(1))
			Expect(existing).NotTo(HaveKey(controller.DisplayNameAnnotation))
		})
	})

	Context("deleteSveltosCluster", func() {
		It("deletes the SveltosCluster if it exists", func() {
			cpName := randomString()
			cp := buildClusterProfile(cpName, namespace, "", nil)

			Expect(controller.ReconcileSveltosCluster(context.TODO(), testEnv.Client, cp, logger)).To(Succeed())
			// Wait for the cache to reflect the created SveltosCluster so that
			// DeleteSveltosCluster's internal Get can find it.
			Expect(waitForObject(context.TODO(), testEnv.Client, &libsveltosv1beta1.SveltosCluster{
				ObjectMeta: metav1.ObjectMeta{Name: cpName, Namespace: namespace},
			})).To(Succeed())

			Expect(controller.DeleteSveltosCluster(context.TODO(), testEnv.Client, cp, logger)).To(Succeed())

			sc := &libsveltosv1beta1.SveltosCluster{}
			Eventually(func() bool {
				err := testEnv.Get(context.TODO(), types.NamespacedName{Namespace: namespace, Name: cpName}, sc)
				return apierrors.IsNotFound(err)
			}, timeout, pollingInterval).Should(BeTrue())
		})

		It("is a no-op when SveltosCluster does not exist", func() {
			cp := buildClusterProfile(randomString(), namespace, "", nil)
			Expect(controller.DeleteSveltosCluster(context.TODO(), testEnv.Client, cp, logger)).To(Succeed())
		})
	})

	Context("deleteKubeconfigSecret", func() {
		It("deletes the kubeconfig Secret if it exists", func() {
			cpName := randomString()
			cp := buildClusterProfile(cpName, namespace, "", nil)

			Expect(controller.ReconcileKubeconfigSecret(context.TODO(), testEnv.Client, cp, []byte("data"), logger)).To(Succeed())
			// Wait for the cache to reflect the created Secret so that
			// DeleteKubeconfigSecret's internal Get can find it.
			Expect(waitForObject(context.TODO(), testEnv.Client, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: controller.KubeconfigSecretName(cpName), Namespace: namespace},
			})).To(Succeed())

			Expect(controller.DeleteKubeconfigSecret(context.TODO(), testEnv.Client, cp, logger)).To(Succeed())

			secret := &corev1.Secret{}
			Eventually(func() bool {
				err := testEnv.Get(context.TODO(),
					types.NamespacedName{Namespace: namespace, Name: controller.KubeconfigSecretName(cpName)}, secret)
				return apierrors.IsNotFound(err)
			}, timeout, pollingInterval).Should(BeTrue())
		})

		It("is a no-op when Secret does not exist", func() {
			cp := buildClusterProfile(randomString(), namespace, "", nil)
			Expect(controller.DeleteKubeconfigSecret(context.TODO(), testEnv.Client, cp, logger)).To(Succeed())
		})
	})

	Context("BuildKubeconfigFromExecStatus", func() {
		const (
			testServer  = "https://cluster.example.com:6443"
			testCertPEM = "-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n"
			testKeyPEM  = "-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----\n" //nolint:gosec // fake PEM, not a real key
		)
		testCA := []byte("fake-ca-data")

		It("produces a token-only kubeconfig when only Token is set", func() {
			status := &clientauthenticationv1.ExecCredentialStatus{Token: "my-token"}
			data, err := controller.BuildKubeconfigFromExecStatus(testServer, testCA, status)
			Expect(err).To(BeNil())

			var kc clientcmdv1.Config
			Expect(yaml.Unmarshal(data, &kc)).To(Succeed())
			Expect(kc.Clusters).To(HaveLen(1))
			Expect(kc.Clusters[0].Cluster.Server).To(Equal(testServer))
			Expect(kc.Clusters[0].Cluster.CertificateAuthorityData).To(Equal(testCA))
			Expect(kc.AuthInfos).To(HaveLen(1))
			Expect(kc.AuthInfos[0].AuthInfo.Token).To(Equal("my-token"))
			Expect(kc.AuthInfos[0].AuthInfo.ClientCertificateData).To(BeEmpty())
			Expect(kc.AuthInfos[0].AuthInfo.ClientKeyData).To(BeEmpty())
		})

		It("produces a cert+key kubeconfig when only certificate data is set", func() {
			status := &clientauthenticationv1.ExecCredentialStatus{
				ClientCertificateData: testCertPEM,
				ClientKeyData:         testKeyPEM,
			}
			data, err := controller.BuildKubeconfigFromExecStatus(testServer, testCA, status)
			Expect(err).To(BeNil())

			var kc clientcmdv1.Config
			Expect(yaml.Unmarshal(data, &kc)).To(Succeed())
			Expect(kc.AuthInfos).To(HaveLen(1))
			Expect(kc.AuthInfos[0].AuthInfo.Token).To(BeEmpty())
			Expect(kc.AuthInfos[0].AuthInfo.ClientCertificateData).To(Equal([]byte(testCertPEM)))
			Expect(kc.AuthInfos[0].AuthInfo.ClientKeyData).To(Equal([]byte(testKeyPEM)))
		})

		It("includes all credential fields when token and cert+key are both set", func() {
			status := &clientauthenticationv1.ExecCredentialStatus{
				Token:                 "combined-token",
				ClientCertificateData: testCertPEM,
				ClientKeyData:         testKeyPEM,
			}
			data, err := controller.BuildKubeconfigFromExecStatus(testServer, testCA, status)
			Expect(err).To(BeNil())

			var kc clientcmdv1.Config
			Expect(yaml.Unmarshal(data, &kc)).To(Succeed())
			Expect(kc.AuthInfos[0].AuthInfo.Token).To(Equal("combined-token"))
			Expect(kc.AuthInfos[0].AuthInfo.ClientCertificateData).To(Equal([]byte(testCertPEM)))
			Expect(kc.AuthInfos[0].AuthInfo.ClientKeyData).To(Equal([]byte(testKeyPEM)))
		})
	})
})

// buildClusterProfile builds a minimal ClusterProfile for use in tests.
func buildClusterProfile(name, namespace, providerName string,
	execExtRaw []byte) *clusterinventoryv1alpha1.ClusterProfile {

	cp := &clusterinventoryv1alpha1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: clusterinventoryv1alpha1.ClusterProfileSpec{
			ClusterManager: clusterinventoryv1alpha1.ClusterManager{Name: "test-manager"},
		},
	}
	if providerName != "" {
		cluster := clientcmdv1.Cluster{}
		if execExtRaw != nil {
			cluster.Extensions = []clientcmdv1.NamedExtension{
				{Name: controller.ExecExtensionKey, Extension: runtime.RawExtension{Raw: execExtRaw}},
			}
		}
		cp.Status.AccessProviders = []clusterinventoryv1alpha1.AccessProvider{
			{Name: providerName, Cluster: cluster},
		}
	}
	return cp
}

// buildSourceSecret builds a Secret that acts as the pre-existing kubeconfig source.
func buildSourceSecret(name, namespace, key string, data []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string][]byte{key: data},
	}
}

// logger is a no-op logger used in unit tests.
var logger = ctrl.Log.WithName("test")
