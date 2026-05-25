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

package helpers

import (
	"context"
	"path"
	goruntime "runtime"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apimachineryscheme "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	controller "github.com/projectsveltos/clusterinventory-controller/controllers"
	"github.com/projectsveltos/clusterinventory-controller/internal/test/helpers/external"
	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
)

var (
	root string
	_    = root // ensure root is used
)

func init() {
	//nolint:dogsled // test helper
	_, filename, _, _ := goruntime.Caller(0)
	root = path.Join(path.Dir(filename), "..", "..", "..")
}

// TestEnvironmentConfiguration holds the envtest configuration.
type TestEnvironmentConfiguration struct {
	env *envtest.Environment
}

// TestEnvironment wraps a controller-runtime manager backed by envtest.
type TestEnvironment struct {
	manager.Manager
	client.Client
	Config *rest.Config
	env    *envtest.Environment
}

// InitTestScheme builds and returns the scheme used by tests.
func InitTestScheme() (*apimachineryscheme.Scheme, error) {
	s := apimachineryscheme.NewScheme()
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

// NewTestEnvironmentConfiguration creates a configuration that starts envtest
// with the multicluster ClusterProfile CRD pre-installed.
func NewTestEnvironmentConfiguration(s *apimachineryscheme.Scheme) *TestEnvironmentConfiguration {
	return &TestEnvironmentConfiguration{
		env: &envtest.Environment{
			Scheme:                s,
			ErrorIfCRDPathMissing: false,
			CRDs: []*apiextensionsv1.CustomResourceDefinition{
				external.TestClusterProfileCRD.DeepCopy(),
			},
		},
	}
}

// Build starts the local API server and returns the TestEnvironment.
func (t *TestEnvironmentConfiguration) Build(s *apimachineryscheme.Scheme) (*TestEnvironment, error) {
	if _, err := t.env.Start(); err != nil {
		return nil, err
	}

	user, err := t.env.ControlPlane.AddUser(envtest.User{
		Name:   "cluster-admin",
		Groups: []string{"system:masters"},
	}, nil)
	if err != nil {
		klog.Fatalf("unable to add user: %v", err)
	}

	mgr, err := ctrl.NewManager(t.env.Config, manager.Options{
		Scheme: s,
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
	})
	if err != nil {
		return nil, err
	}

	if err := (&controller.ClusterProfileReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		ConcurrentReconciles: 5,
	}).SetupWithManager(mgr); err != nil {
		return nil, err
	}

	kubeconfig, err := user.KubeConfig()
	if err != nil {
		klog.Fatalf("unable to get kubeconfig: %v", err)
	}
	_ = kubeconfig

	return &TestEnvironment{
		Manager: mgr,
		Client:  mgr.GetClient(),
		Config:  t.env.Config,
		env:     t.env,
	}, nil
}

// StartManager starts the manager. Call this in a goroutine from BeforeSuite.
func (t *TestEnvironment) StartManager(ctx context.Context) error {
	return t.Start(ctx)
}

// Stop tears down the envtest environment.
func (t *TestEnvironment) Stop() error {
	return t.env.Stop()
}
