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
	"reflect"

	"github.com/go-logr/logr"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
)

// ClusterProfilePredicates returns a predicate that filters ClusterProfile events.
// Create and Delete events always trigger reconciliation.
// Update events are filtered to only trigger when the access provider list changes,
// since that is the only status field this controller acts on.
func ClusterProfilePredicates(logger logr.Logger) predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			newCP, ok := e.ObjectNew.(*clusterinventoryv1alpha1.ClusterProfile)
			if !ok {
				return true
			}
			oldCP, ok := e.ObjectOld.(*clusterinventoryv1alpha1.ClusterProfile)
			if !ok {
				return true
			}

			// React when the object is first marked for deletion so reconcileDelete
			// can run and remove the finalizer.
			if newCP.DeletionTimestamp != nil && oldCP.DeletionTimestamp == nil {
				return true
			}

			// React when access providers change.
			if !reflect.DeepEqual(newCP.Status.AccessProviders, oldCP.Status.AccessProviders) {
				logger.V(logs.LogVerbose).Info("ClusterProfile access providers changed, will reconcile")
				return true
			}
			if !reflect.DeepEqual(newCP.Status.CredentialProviders, oldCP.Status.CredentialProviders) {
				logger.V(logs.LogVerbose).Info("ClusterProfile credential providers changed, will reconcile")
				return true
			}

			// React when labels change (could affect owner references or metadata we copy).
			if !reflect.DeepEqual(newCP.Labels, oldCP.Labels) {
				return true
			}

			logger.V(logs.LogVerbose).Info("ClusterProfile update is not relevant, skipping reconcile")
			return false
		},
		CreateFunc:  func(e event.CreateEvent) bool { return true },
		DeleteFunc:  func(e event.DeleteEvent) bool { return true },
		GenericFunc: func(e event.GenericEvent) bool { return false },
	}
}
