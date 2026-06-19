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

var (
	GetKubeconfig                 = getKubeconfig
	GetKubeconfigFromSecretReader = getKubeconfigFromSecretReader
	FindAccessProvider            = findAccessProvider
	SourceSecretNamespacedName    = sourceSecretNamespacedName
	ApplyDisplayName              = applyDisplayName
	KubeconfigSecretName          = kubeconfigSecretName
	ReconcileKubeconfigSecret     = reconcileKubeconfigSecret
	ReconcileSveltosCluster       = reconcileSveltosCluster
	DeleteSveltosCluster          = deleteSveltosCluster
	DeleteKubeconfigSecret        = deleteKubeconfigSecret
)

const (
	ExecExtensionKey               = execExtensionKey
	KubeconfigSecretReaderProvider = kubeconfigSecretReaderProviderName
	KubeconfigKey                  = kubeconfigKey
	ManagedByLabel                 = managedByLabel
	ManagedByValue                 = managedByValue
	DisplayNameAnnotation          = displayNameAnnotation
	SourceSecretIndex              = sourceSecretIndex
)
