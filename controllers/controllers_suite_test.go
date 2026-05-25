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
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/projectsveltos/clusterinventory-controller/internal/test/helpers"
	libsveltoscrd "github.com/projectsveltos/libsveltos/lib/crd"
	"github.com/projectsveltos/libsveltos/lib/k8s_utils"
)

const (
	timeout         = 1 * time.Minute
	pollingInterval = 5 * time.Second
)

var (
	testEnv *helpers.TestEnvironment
	cancel  context.CancelFunc
	ctx     context.Context
)

func TestController(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	By("bootstrapping test environment")

	ctrl.SetLogger(klog.Background())

	ctx, cancel = context.WithCancel(context.TODO())

	scheme, err := helpers.InitTestScheme()
	Expect(err).To(BeNil())

	testEnvConfig := helpers.NewTestEnvironmentConfiguration(scheme)
	testEnv, err = testEnvConfig.Build(scheme)
	Expect(err).To(BeNil())

	go func() {
		By("Starting the manager")
		err = testEnv.StartManager(ctx)
		if err != nil {
			panic(fmt.Sprintf("Failed to start the envtest manager: %v", err))
		}
	}()

	// Install SveltosCluster CRD.
	sveltosCRD, err := k8s_utils.GetUnstructured(libsveltoscrd.GetSveltosClusterCRDYAML())
	Expect(err).To(BeNil())
	Expect(testEnv.Create(context.TODO(), sveltosCRD)).To(Succeed())
	Expect(waitForObject(context.TODO(), testEnv.Client, sveltosCRD)).To(Succeed())

	if synced := testEnv.GetCache().WaitForCacheSync(ctx); !synced {
		time.Sleep(time.Second)
	}
})

var _ = AfterSuite(func() {
	cancel()
	By("tearing down the test environment")
	Expect(testEnv.Stop()).To(Succeed())
})

func randomString() string {
	const length = 10
	return util.RandomString(length)
}

var cacheSyncBackoff = wait.Backoff{
	Duration: 100 * time.Millisecond,
	Factor:   1.5,
	Steps:    8,
	Jitter:   0.4,
}

func waitForObject(ctx context.Context, c client.Client, obj client.Object) error {
	objCopy := obj.DeepCopyObject().(client.Object)
	key := client.ObjectKeyFromObject(obj)
	return wait.ExponentialBackoff(cacheSyncBackoff, func() (bool, error) {
		if err := c.Get(ctx, key, objCopy); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
}

// Byf wraps By with fmt.Sprintf formatting.
func Byf(format string, a ...interface{}) {
	By(fmt.Sprintf(format, a...))
}

// Unused in unit tests but keeps the import live for the test helper.
var _ = corev1.Pod{}
var _ = unstructured.Unstructured{}
