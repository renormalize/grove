// /*
// Copyright 2025 The Grove Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// */

package podclique

import (
	"context"
	"fmt"
	"strconv"

	apicommon "github.com/NVIDIA/grove/operator/api/common"
	"github.com/NVIDIA/grove/operator/api/common/constants"
	grovecorev1alpha1 "github.com/NVIDIA/grove/operator/api/core/v1alpha1"
	"github.com/NVIDIA/grove/operator/internal/component"
	componentutils "github.com/NVIDIA/grove/operator/internal/component/utils"
	ctrlcommon "github.com/NVIDIA/grove/operator/internal/controller/common"
	ctrlutils "github.com/NVIDIA/grove/operator/internal/controller/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (r *Reconciler) reconcileSpec(ctx context.Context, logger logr.Logger, pclq *grovecorev1alpha1.PodClique) ctrlcommon.ReconcileStepResult {
	rLog := logger.WithValues("operation", "spec-reconcile")
	reconcileStepFns := []ctrlcommon.ReconcileStepFn[grovecorev1alpha1.PodClique]{
		r.ensureFinalizer,
		r.recordReconcileStart,
		r.processRollingUpdate,
		r.syncPCLQResources,
		r.recordReconcileSuccess,
		r.updateObservedGeneration,
	}

	for _, fn := range reconcileStepFns {
		if stepResult := fn(ctx, rLog, pclq); ctrlcommon.ShortCircuitReconcileFlow(stepResult) {
			return r.recordIncompleteReconcile(ctx, logger, pclq, &stepResult)
		}
	}
	logger.Info("Finished spec reconciliation flow", "PodClique", client.ObjectKeyFromObject(pclq))
	return ctrlcommon.ContinueReconcile()
}

func (r *Reconciler) ensureFinalizer(ctx context.Context, logger logr.Logger, pclq *grovecorev1alpha1.PodClique) ctrlcommon.ReconcileStepResult {
	if !controllerutil.ContainsFinalizer(pclq, constants.FinalizerPodClique) {
		logger.Info("Adding finalizer", "PodClique", client.ObjectKeyFromObject(pclq), "finalizerName", constants.FinalizerPodClique)
		if err := ctrlutils.AddAndPatchFinalizer(ctx, r.client, pclq, constants.FinalizerPodClique); err != nil {
			return ctrlcommon.ReconcileWithErrors("error adding finalizer", err)
		}
	}
	return ctrlcommon.ContinueReconcile()
}

func (r *Reconciler) recordReconcileStart(ctx context.Context, logger logr.Logger, pclq *grovecorev1alpha1.PodClique) ctrlcommon.ReconcileStepResult {
	if err := r.reconcileStatusRecorder.RecordStart(ctx, pclq, grovecorev1alpha1.LastOperationTypeReconcile); err != nil {
		logger.Error(err, "failed to record reconcile start operation")
		return ctrlcommon.ReconcileWithErrors("error recoding reconcile start", err)
	}
	return ctrlcommon.ContinueReconcile()
}

func (r *Reconciler) processRollingUpdate(ctx context.Context, logger logr.Logger, pclq *grovecorev1alpha1.PodClique) ctrlcommon.ReconcileStepResult {
	pclqObjectKey := client.ObjectKeyFromObject(pclq)
	pgs, err := componentutils.GetPodGangSet(ctx, r.client, pclq.ObjectMeta)
	if err != nil {
		return ctrlcommon.ReconcileWithErrors(fmt.Sprintf("could not get owner PodGangSet for PodClique: %v", pclqObjectKey), err)
	}
	// updates of PCSG PodCliques are handled by the PCSG controller
	if !componentutils.IsStandalonePCLQ(pgs, pclq.Name) {
		return ctrlcommon.ContinueReconcile()
	}
	if pgs.Status.RollingUpdateProgress == nil || pgs.Status.RollingUpdateProgress.CurrentlyUpdating == nil {
		// No update has yet been triggered for the PodGangSet. Nothing to do here.
		return ctrlcommon.ContinueReconcile()
	}
	pgsReplicaInUpdating := pgs.Status.RollingUpdateProgress.CurrentlyUpdating.ReplicaIndex
	pgsReplicaIndexStr, ok := pclq.Labels[apicommon.LabelPodGangSetReplicaIndex]
	if !ok {
		logger.Info("PodGangSet is currently under rolling update. Cannot process pending updates for this PodClique as no PodGangSet index label is found")
		return ctrlcommon.ContinueReconcile()
	}
	if pgsReplicaIndexStr != strconv.Itoa(int(pgsReplicaInUpdating)) {
		logger.Info("PodGangSet is currently under rolling update. Skipping processing pending updates for this PodClique as it does not belong to the PodGangSet Index in update", "currentlyUpdatingPGSIndex", pgsReplicaInUpdating, "pgsIndexForpclq", pgsReplicaIndexStr)
		return ctrlcommon.ContinueReconcile()
	}

	if err := r.updateRollingUpdateProgress(ctx, pgs, pclq); err != nil {
		return ctrlcommon.ReconcileWithErrors("could not update rolling update progress", err)
	}

	return ctrlcommon.ContinueReconcile()
}

func (r *Reconciler) updateRollingUpdateProgress(ctx context.Context, pgs *grovecorev1alpha1.PodGangSet, pclq *grovecorev1alpha1.PodClique) error {
	if pgs.Status.CurrentGenerationHash != nil && *pgs.Status.CurrentGenerationHash != *pclq.Status.CurrentPodGangSetGenerationHash {
		// reset and start the rolling update
		patch := client.MergeFrom(pclq.DeepCopy())
		pclq.Status.RollingUpdateProgress = &grovecorev1alpha1.PodCliqueRollingUpdateProgress{
			UpdateStartedAt:          metav1.Now(),
			PodGangSetGenerationHash: *pgs.Status.CurrentGenerationHash,
		}
		if err := r.client.Status().Patch(ctx, pclq, patch); err != nil {
			return fmt.Errorf("failed to update PodClique %s status with rolling update progress", client.ObjectKeyFromObject(pclq))
		}
	}
	return nil
}
func (r *Reconciler) syncPCLQResources(ctx context.Context, logger logr.Logger, pclq *grovecorev1alpha1.PodClique) ctrlcommon.ReconcileStepResult {
	for _, kind := range getOrderedKindsForSync() {
		operator, err := r.operatorRegistry.GetOperator(kind)
		if err != nil {
			return ctrlcommon.ReconcileWithErrors(fmt.Sprintf("error getting operator for kind: %s", kind), err)
		}
		logger.Info("Syncing PodClique resources", "kind", kind)
		if err = operator.Sync(ctx, logger, pclq); err != nil {
			if shouldRequeue := ctrlutils.ShouldRequeueAfter(err); shouldRequeue {
				logger.Info("retrying sync due to component", "kind", kind, "syncRetryInterval", ctrlcommon.ComponentSyncRetryInterval, "message", err.Error())
				return ctrlcommon.ReconcileAfter(ctrlcommon.ComponentSyncRetryInterval, err.Error())
			}
			logger.Error(err, "failed to sync PodClique resources", "kind", kind)
			return ctrlcommon.ReconcileWithErrors("error syncing managed resources", fmt.Errorf("failed to sync %s: %w", kind, err))
		}
	}
	return ctrlcommon.ContinueReconcile()
}

func (r *Reconciler) recordReconcileSuccess(ctx context.Context, logger logr.Logger, pclq *grovecorev1alpha1.PodClique) ctrlcommon.ReconcileStepResult {
	if err := r.reconcileStatusRecorder.RecordCompletion(ctx, pclq, grovecorev1alpha1.LastOperationTypeReconcile, nil); err != nil {
		logger.Error(err, "failed to record reconcile success operation")
		return ctrlcommon.ReconcileWithErrors("error recording reconcile success", err)
	}
	return ctrlcommon.ContinueReconcile()
}

func (r *Reconciler) updateObservedGeneration(ctx context.Context, logger logr.Logger, pclq *grovecorev1alpha1.PodClique) ctrlcommon.ReconcileStepResult {
	original := pclq.DeepCopy()
	pclq.Status.ObservedGeneration = &pclq.Generation
	if err := r.client.Status().Patch(ctx, pclq, client.MergeFrom(original)); err != nil {
		logger.Error(err, "failed to patch status.ObservedGeneration")
		return ctrlcommon.ReconcileWithErrors("error updating observed generation", err)
	}
	logger.Info("patched status.ObservedGeneration", "ObservedGeneration", pclq.Generation)
	return ctrlcommon.ContinueReconcile()
}

func (r *Reconciler) recordIncompleteReconcile(ctx context.Context, logger logr.Logger, pclq *grovecorev1alpha1.PodClique, errResult *ctrlcommon.ReconcileStepResult) ctrlcommon.ReconcileStepResult {
	if err := r.reconcileStatusRecorder.RecordCompletion(ctx, pclq, grovecorev1alpha1.LastOperationTypeReconcile, errResult); err != nil {
		logger.Error(err, "failed to record incomplete reconcile operation")
		// combine all errors
		allErrs := append(errResult.GetErrors(), err)
		return ctrlcommon.ReconcileWithErrors("error recording incomplete reconciliation", allErrs...)
	}
	return *errResult
}

func getOrderedKindsForSync() []component.Kind {
	return []component.Kind{
		component.KindPod,
	}
}
