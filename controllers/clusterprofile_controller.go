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
	"context"
	"fmt"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
)

const (
	// clusterProfileFinalizer is added to ClusterProfile instances so that the
	// controller can delete the SveltosCluster and kubeconfig Secret before
	// the ClusterProfile is removed from the API server.
	clusterProfileFinalizer = "clusterinventory.projectsveltos.io/finalizer"
)

// ClusterProfileReconciler reconciles a multicluster.x-k8s.io ClusterProfile.
type ClusterProfileReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	ConcurrentReconciles int
}

//+kubebuilder:rbac:groups=multicluster.x-k8s.io,resources=clusterprofiles,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=multicluster.x-k8s.io,resources=clusterprofiles/status,verbs=get
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=sveltosclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=sveltosclusters/status,verbs=get
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=debuggingconfigurations,verbs=get;list;watch
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

func (r *ClusterProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)
	logger.V(logs.LogInfo).Info("Reconciling ClusterProfile")

	cp := &clusterinventoryv1alpha1.ClusterProfile{}
	if err := r.Get(ctx, req.NamespacedName, cp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to fetch ClusterProfile")
		return ctrl.Result{}, err
	}

	if !cp.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.reconcileDelete(ctx, cp, logger)
	}

	return ctrl.Result{}, r.reconcileNormal(ctx, cp, logger)
}

func (r *ClusterProfileReconciler) reconcileNormal(ctx context.Context,
	cp *clusterinventoryv1alpha1.ClusterProfile, logger logr.Logger) error {

	logger.V(logs.LogDebug).Info("reconcileNormal")

	if !controllerutil.ContainsFinalizer(cp, clusterProfileFinalizer) {
		controllerutil.AddFinalizer(cp, clusterProfileFinalizer)
		if err := r.Update(ctx, cp); err != nil {
			logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to add finalizer: %v", err))
			return err
		}
	}

	kubeconfig, err := getKubeconfig(ctx, r.Client, cp, logger)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get kubeconfig: %v", err))
		return err
	}

	if err := reconcileKubeconfigSecret(ctx, r.Client, cp, kubeconfig, logger); err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to reconcile kubeconfig secret: %v", err))
		return err
	}

	if err := reconcileSveltosCluster(ctx, r.Client, cp, logger); err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to reconcile SveltosCluster: %v", err))
		return err
	}

	return nil
}

func (r *ClusterProfileReconciler) reconcileDelete(ctx context.Context,
	cp *clusterinventoryv1alpha1.ClusterProfile, logger logr.Logger) error {

	logger.V(logs.LogDebug).Info("reconcileDelete")

	if err := deleteSveltosCluster(ctx, r.Client, cp, logger); err != nil {
		return err
	}

	if err := deleteKubeconfigSecret(ctx, r.Client, cp, logger); err != nil {
		return err
	}

	controllerutil.RemoveFinalizer(cp, clusterProfileFinalizer)
	if err := r.Update(ctx, cp); err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to remove finalizer: %v", err))
		return err
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&clusterinventoryv1alpha1.ClusterProfile{}).
		WithEventFilter(ClusterProfilePredicates(mgr.GetLogger().WithValues("predicate", "clusterprofilepredicates"))).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.ConcurrentReconciles}).
		Complete(r)
}
