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
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
)

const (
	// clusterProfileFinalizer is added to ClusterProfile instances so that the
	// controller can delete the SveltosCluster and kubeconfig Secret before
	// the ClusterProfile is removed from the API server.
	clusterProfileFinalizer = "clusterinventory.projectsveltos.io/finalizer"

	// tokenRefreshRatio controls how early we requeue before the token expires.
	// Requeuing at 80% of the remaining lifetime gives a comfortable safety margin.
	tokenRefreshRatio = 0.8
)

// ClusterProfileReconciler reconciles a multicluster.x-k8s.io ClusterProfile.
type ClusterProfileReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	ConcurrentReconciles int
	AccessConfig         *access.Config
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

	return r.reconcileNormal(ctx, cp, logger)
}

func (r *ClusterProfileReconciler) reconcileNormal(ctx context.Context,
	cp *clusterinventoryv1alpha1.ClusterProfile, logger logr.Logger) (ctrl.Result, error) {

	logger.V(logs.LogDebug).Info("reconcileNormal")

	if !controllerutil.ContainsFinalizer(cp, clusterProfileFinalizer) {
		controllerutil.AddFinalizer(cp, clusterProfileFinalizer)
		if err := r.Update(ctx, cp); err != nil {
			logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to add finalizer: %v", err))
			return ctrl.Result{}, err
		}
	}

	kubeconfig, expiry, err := getKubeconfig(ctx, r.Client, r.AccessConfig, cp, logger)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get kubeconfig: %v", err))
		return ctrl.Result{}, err
	}

	if err := reconcileKubeconfigSecret(ctx, r.Client, cp, kubeconfig, logger); err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to reconcile kubeconfig secret: %v", err))
		return ctrl.Result{}, err
	}

	if err := reconcileSveltosCluster(ctx, r.Client, cp, logger); err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to reconcile SveltosCluster: %v", err))
		return ctrl.Result{}, err
	}

	if expiry != nil {
		remaining := time.Until(*expiry)
		const minRequeue = time.Minute
		requeueAfter := time.Duration(float64(remaining) * tokenRefreshRatio)
		if requeueAfter < minRequeue {
			requeueAfter = minRequeue
		}
		logger.V(logs.LogDebug).Info("scheduling token refresh",
			"expiry", expiry, "requeueAfter", requeueAfter)
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	return ctrl.Result{}, nil
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
	// Index ClusterProfiles by the source Secret they reference via kubeconfig-secretreader,
	// so the Secret watch can enqueue the right ClusterProfile when a source Secret changes.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&clusterinventoryv1alpha1.ClusterProfile{},
		sourceSecretIndex,
		func(obj client.Object) []string {
			cp, ok := obj.(*clusterinventoryv1alpha1.ClusterProfile)
			if !ok {
				return nil
			}
			ref := sourceSecretNamespacedName(cp)
			if ref == "" {
				return nil
			}
			return []string{ref}
		}); err != nil {
		return fmt.Errorf("indexing ClusterProfile source secrets: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&clusterinventoryv1alpha1.ClusterProfile{}).
		WithEventFilter(ClusterProfilePredicates(mgr.GetLogger().WithValues("predicate", "clusterprofilepredicates"))).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.sourceSecretToClusterProfiles),
			builder.WithPredicates(SourceSecretPredicates()),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.ConcurrentReconciles}).
		Complete(r)
}

// sourceSecretToClusterProfiles maps a changed source Secret to the ClusterProfiles
// that reference it via the kubeconfig-secretreader access provider.
func (r *ClusterProfileReconciler) sourceSecretToClusterProfiles(
	ctx context.Context, obj client.Object) []reconcile.Request {

	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}

	ref := secret.Namespace + "/" + secret.Name
	cpList := &clusterinventoryv1alpha1.ClusterProfileList{}
	if err := r.List(ctx, cpList, client.MatchingFields{sourceSecretIndex: ref}); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(cpList.Items))
	for i := range cpList.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: cpList.Items[i].Namespace,
				Name:      cpList.Items[i].Name,
			},
		})
	}
	return requests
}
