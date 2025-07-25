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

package podcliquescalinggroup

import (
	"context"
	"fmt"

	grovecorev1alpha1 "github.com/NVIDIA/grove/operator/api/core/v1alpha1"
	ctrlcommon "github.com/NVIDIA/grove/operator/internal/controller/common"
	ctrlutils "github.com/NVIDIA/grove/operator/internal/controller/utils"
	"github.com/NVIDIA/grove/operator/internal/utils"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (r *Reconciler) triggerDeletionFlow(ctx context.Context, logger logr.Logger, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) ctrlcommon.ReconcileStepResult {
	deleteStepFns := []ctrlcommon.ReconcileStepFn[grovecorev1alpha1.PodCliqueScalingGroup]{
		r.deletePodCliqueScalingGroupResources,
		r.verifyNoResourcesAwaitsCleanup,
		r.removeFinalizer,
	}
	for _, fn := range deleteStepFns {
		if stepResult := fn(ctx, logger, pcsg); ctrlcommon.ShortCircuitReconcileFlow(stepResult) {
			return stepResult
		}
	}
	logger.Info("PodCliqueScalingGroup deleted successfully")
	return ctrlcommon.DoNotRequeue()
}

func (r *Reconciler) deletePodCliqueScalingGroupResources(ctx context.Context, logger logr.Logger, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) ctrlcommon.ReconcileStepResult {
	operators := r.operatorRegistry.GetAllOperators()
	deleteTasks := make([]utils.Task, 0, len(operators))
	for kind, operator := range operators {
		deleteTasks = append(deleteTasks, utils.Task{
			Name: fmt.Sprintf("delete-%s", kind),
			Fn: func(ctx context.Context) error {
				return operator.Delete(ctx, logger, pcsg.ObjectMeta)
			},
		})
	}
	logger.Info("Triggering delete of PodCliqueScalingGroup resources")
	if runResult := utils.RunConcurrently(ctx, logger, deleteTasks); runResult.HasErrors() {
		deletionErr := runResult.GetAggregatedError()
		logger.Error(deletionErr, "Error deleting managed resources", "summary", runResult.GetSummary())
		return ctrlcommon.ReconcileWithErrors("error deleting managed resources", deletionErr)
	}
	return ctrlcommon.ContinueReconcile()
}

func (r *Reconciler) verifyNoResourcesAwaitsCleanup(ctx context.Context, logger logr.Logger, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) ctrlcommon.ReconcileStepResult {
	return ctrlutils.VerifyNoResourceAwaitsCleanup(ctx, logger, r.operatorRegistry, pcsg.ObjectMeta)
}

func (r *Reconciler) removeFinalizer(ctx context.Context, logger logr.Logger, pcsg *grovecorev1alpha1.PodCliqueScalingGroup) ctrlcommon.ReconcileStepResult {
	if !controllerutil.ContainsFinalizer(pcsg, grovecorev1alpha1.FinalizerPodCliqueScalingGroup) {
		logger.Info("Finalizer not found", "PodCliqueScalingGroup", pcsg)
		return ctrlcommon.DoNotRequeue()
	}
	logger.Info("Removing finalizer", "PodCliqueScalingGroup", pcsg, "finalizerName", grovecorev1alpha1.FinalizerPodCliqueScalingGroup)
	if err := ctrlutils.RemoveAndPatchFinalizer(ctx, r.client, pcsg, grovecorev1alpha1.FinalizerPodCliqueScalingGroup); err != nil {
		return ctrlcommon.ReconcileWithErrors("error removing finalizer", fmt.Errorf("failed to remove finalizer: %s from PodCliqueScalingGroup: %v: %w", grovecorev1alpha1.FinalizerPodCliqueScalingGroup, client.ObjectKeyFromObject(pcsg), err))
	}
	return ctrlcommon.ContinueReconcile()
}
