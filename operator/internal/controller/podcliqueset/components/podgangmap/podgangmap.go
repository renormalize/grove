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

package podgangmap

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	apicommon "github.com/ai-dynamo/grove/operator/api/common"
	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	"github.com/ai-dynamo/grove/operator/internal/controller/common/component"
	componentutils "github.com/ai-dynamo/grove/operator/internal/controller/common/component/utils"
	groveerr "github.com/ai-dynamo/grove/operator/internal/errors"
	k8sutils "github.com/ai-dynamo/grove/operator/internal/utils/kubernetes"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	errCodeListPodGangMaps   grovecorev1alpha1.ErrorCode = "ERR_LIST_PODGANGMAPS"
	errCodeSyncPodGangMap    grovecorev1alpha1.ErrorCode = "ERR_SYNC_PODGANGMAP"
	errCodeDeletePodGangMaps grovecorev1alpha1.ErrorCode = "ERR_DELETE_PODGANGMAPS"
	errCodeListPCSGs         grovecorev1alpha1.ErrorCode = "ERR_LIST_PCSGS_FOR_PODGANGMAP"
	errCodeListPCLQs         grovecorev1alpha1.ErrorCode = "ERR_LIST_PCLQS_FOR_PODGANGMAP"
)

type _resource struct {
	client client.Client
	scheme *runtime.Scheme
}

// New creates a new instance of the PodGangMap component operator.
func New(cl client.Client, scheme *runtime.Scheme) component.Operator[grovecorev1alpha1.PodCliqueSet] {
	return &_resource{
		client: cl,
		scheme: scheme,
	}
}

// GetExistingResourceNames returns the names of existing PodGangMap resources owned by the PodCliqueSet.
func (r _resource) GetExistingResourceNames(ctx context.Context, _ logr.Logger, pcsObjMeta metav1.ObjectMeta) ([]string, error) {
	objMetaList := &metav1.PartialObjectMetadataList{}
	objMetaList.SetGroupVersionKind(grovecorev1alpha1.SchemeGroupVersion.WithKind("PodGangMap"))
	if err := r.client.List(ctx,
		objMetaList,
		client.InNamespace(pcsObjMeta.Namespace),
		client.MatchingLabels(getSelectorLabels(pcsObjMeta.Name)),
	); err != nil {
		return nil, groveerr.WrapError(err,
			errCodeListPodGangMaps,
			component.OperationGetExistingResourceNames,
			fmt.Sprintf("Error listing PodGangMap for PodCliqueSet: %v", k8sutils.GetObjectKeyFromObjectMeta(pcsObjMeta)),
		)
	}
	return k8sutils.FilterMapOwnedResourceNames(pcsObjMeta, objMetaList.Items), nil
}

// Sync creates or updates one PodGangMap per PodCliqueSet replica and deletes any excess ones.
func (r _resource) Sync(ctx context.Context, logger logr.Logger, pcs *grovecorev1alpha1.PodCliqueSet) error {
	logger.Info("Syncing PodGangMap resources")

	existingPGMNames, err := r.GetExistingResourceNames(ctx, logger, pcs.ObjectMeta)
	if err != nil {
		return err
	}

	existingPCSGs, err := r.listPCSGsForPCS(ctx, pcs)
	if err != nil {
		return groveerr.WrapError(err,
			errCodeListPCSGs,
			component.OperationSync,
			fmt.Sprintf("Error listing PodCliqueScalingGroups for PodCliqueSet: %v", client.ObjectKeyFromObject(pcs)),
		)
	}

	existingStandalonePCLQs, err := componentutils.GetPodCliquesWithParentPCS(ctx, r.client, client.ObjectKeyFromObject(pcs))
	if err != nil {
		return groveerr.WrapError(err,
			errCodeListPCLQs,
			component.OperationSync,
			fmt.Sprintf("Error listing standalone PodCliques for PodCliqueSet: %v", client.ObjectKeyFromObject(pcs)),
		)
	}

	expectedPGMNames := make([]string, 0, pcs.Spec.Replicas)
	for replicaIndex := range pcs.Spec.Replicas {
		pgmName := apicommon.GeneratePodGangMapName(apicommon.ResourceNameReplica{Name: pcs.Name, Replica: int(replicaIndex)})
		expectedPGMNames = append(expectedPGMNames, pgmName)

		pcsgsForReplica := lo.Filter(existingPCSGs, func(pcsg grovecorev1alpha1.PodCliqueScalingGroup, _ int) bool {
			return pcsg.Labels[apicommon.LabelPodCliqueSetReplicaIndex] == strconv.Itoa(int(replicaIndex))
		})
		pclqsForReplica := lo.Filter(existingStandalonePCLQs, func(pclq grovecorev1alpha1.PodClique, _ int) bool {
			return pclq.Labels[apicommon.LabelPodCliqueSetReplicaIndex] == strconv.Itoa(int(replicaIndex))
		})

		entries, err := r.computeEntries(pcs, int(replicaIndex), pcsgsForReplica, pclqsForReplica)
		if err != nil {
			return groveerr.WrapError(err,
				errCodeSyncPodGangMap,
				component.OperationSync,
				fmt.Sprintf("Error computing entries for PodGangMap %s: %v", pgmName, client.ObjectKeyFromObject(pcs)),
			)
		}
		pgm := emptyPodGangMap(client.ObjectKey{Namespace: pcs.Namespace, Name: pgmName})
		if _, err := controllerutil.CreateOrPatch(ctx, r.client, pgm, func() error {
			return r.buildResource(pgm, pcs, int(replicaIndex), entries)
		}); err != nil {
			return groveerr.WrapError(err,
				errCodeSyncPodGangMap,
				component.OperationSync,
				fmt.Sprintf("Error creating or updating PodGangMap %s for PodCliqueSet: %v", pgmName, client.ObjectKeyFromObject(pcs)),
			)
		}
	}

	// Delete excess PodGangMaps (from scale-in).
	for _, excessPGMName := range lo.Filter(existingPGMNames, func(n string, _ int) bool { return !slices.Contains(expectedPGMNames, n) }) {
		pgm := emptyPodGangMap(client.ObjectKey{Namespace: pcs.Namespace, Name: excessPGMName})
		if err := r.client.Delete(ctx, pgm); err != nil {
			return groveerr.WrapError(err,
				errCodeSyncPodGangMap,
				component.OperationSync,
				fmt.Sprintf("Error deleting excess PodGangMap %s for PodCliqueSet: %v", excessPGMName, client.ObjectKeyFromObject(pcs)),
			)
		}
		logger.Info("Deleted excess PodGangMap", "name", excessPGMName)
	}

	logger.Info("Successfully synced PodGangMap resources")
	return nil
}

// Delete removes all PodGangMap resources owned by the PodCliqueSet.
func (r _resource) Delete(ctx context.Context, logger logr.Logger, pcsObjMeta metav1.ObjectMeta) error {
	logger.Info("Triggering deletion of PodGangMaps")
	if err := r.client.DeleteAllOf(ctx,
		&grovecorev1alpha1.PodGangMap{},
		client.InNamespace(pcsObjMeta.Namespace),
		client.MatchingLabels(getSelectorLabels(pcsObjMeta.Name)),
	); err != nil {
		return groveerr.WrapError(err,
			errCodeDeletePodGangMaps,
			component.OperationDelete,
			fmt.Sprintf("Error deleting PodGangMaps for PodCliqueSet: %v", k8sutils.GetObjectKeyFromObjectMeta(pcsObjMeta)),
		)
	}
	logger.Info("Deleted PodGangMaps")
	return nil
}

// computeEntries returns the desired PodGangEntries for one PodCliqueSet replica.
//
// For non-Coherent strategies (RollingRecreate, OnDelete, nil), entries are computed
// from pcs.Spec and live PCLQ/PCSG state: one BasePodGang entry covering all standalone
// PCLQs and the first MinAvailable replicas of each PCSG, plus one ScaledPodGang entry
// per PCSG replica beyond MinAvailable. pcs.Status.CurrentGenerationHash is always
// up-to-date at this point (set by initUpdateProgress before Sync runs), so the same
// code path handles both steady-state and update-in-progress cases.
//
// TODO: For the Coherent strategy, entry computation is not yet implemented in this commit.
func (r _resource) computeEntries(pcs *grovecorev1alpha1.PodCliqueSet, replicaIndex int, pcsgs []grovecorev1alpha1.PodCliqueScalingGroup, pclqs []grovecorev1alpha1.PodClique) ([]grovecorev1alpha1.PodGangEntry, error) {
	switch {
	case componentutils.IsCoherentUpdateInProgress(pcs):
		// TODO(coherent-updates): compute partial entries from InFlightPodGangs.
		return nil, nil
	case componentutils.IsCoherentStrategy(pcs):
		// TODO(coherent-updates): compute MPG-convention entries from MVU rules.
		return nil, nil
	default:
		// RollingRecreate, OnDelete, or nil strategy — all use the same BasePodGang/ScaledPodGang entry structure.
		entries, err := r.buildBaseAndScaledPodGangEntries(pcs, replicaIndex, pcsgs, pclqs)
		if err != nil {
			return nil, err
		}
		return entries, nil
	}
}

// buildBaseAndScaledPodGangEntries builds the BasePodGang entry and ScaledPodGang entries for one PodCliqueSet replica.
// One BasePodGang entry covers all standalone PodCliques and the first MinAvailable replicas of each PCSG.
// One ScaledPodGang entry is added per PCSG replica beyond MinAvailable.
func (r _resource) buildBaseAndScaledPodGangEntries(pcs *grovecorev1alpha1.PodCliqueSet, replicaIndex int, existingPCSGs []grovecorev1alpha1.PodCliqueScalingGroup, existingPCLQs []grovecorev1alpha1.PodClique) ([]grovecorev1alpha1.PodGangEntry, error) {
	if pcs.Status.CurrentGenerationHash == nil {
		return nil, fmt.Errorf("PodCliqueSet %s/%s has no CurrentGenerationHash set in status", pcs.Namespace, pcs.Name)
	}

	generationHash := *pcs.Status.CurrentGenerationHash
	bpgName := apicommon.GenerateBasePodGangName(apicommon.ResourceNameReplica{Name: pcs.Name, Replica: replicaIndex})

	// Build a lookup of live replica count by existing PCLQ FQN.
	existingReplicasByPCLQ := make(map[string]int32, len(existingPCLQs))
	for _, pclq := range existingPCLQs {
		existingReplicasByPCLQ[pclq.Name] = pclq.Spec.Replicas
	}

	// BasePodGang entry: standalone PCLQs at their live replica count (falling back to template if not yet created).
	standalonePCLQFQNSet := componentutils.GetStandalonePCLQFQNSet(pcs, replicaIndex)
	bpgPodCliques := make(map[string]int32, standalonePCLQFQNSet.Len())
	for _, cliqueTemplate := range pcs.Spec.Template.Cliques {
		pclqFQN := apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{Name: pcs.Name, Replica: replicaIndex}, cliqueTemplate.Name)
		if !standalonePCLQFQNSet.Has(pclqFQN) {
			continue
		}
		replicas, exists := existingReplicasByPCLQ[pclqFQN]
		if !exists {
			// PCLQ not yet created (first reconcile); fall back to template replicas.
			replicas = cliqueTemplate.Spec.Replicas
		}
		bpgPodCliques[pclqFQN] = replicas
	}

	// BasePodGang entry: each PCSG contributes MinAvailable replicas.
	bpgPCSGs := make(map[string]int32)
	for _, pcsg := range existingPCSGs {
		if pcsg.Spec.MinAvailable != nil {
			bpgPCSGs[pcsg.Name] = *pcsg.Spec.MinAvailable
		}
	}

	entries := []grovecorev1alpha1.PodGangEntry{
		{
			Name:                       bpgName,
			PodCliqueSetGenerationHash: generationHash,
			PodCliques:                 bpgPodCliques,
			PodCliqueScalingGroups:     bpgPCSGs,
		},
	}

	// ScaledPodGang entries: one per PCSG replica beyond MinAvailable.
	for _, pcsg := range existingPCSGs {
		if pcsg.Spec.MinAvailable == nil {
			continue
		}
		minAvailable := *pcsg.Spec.MinAvailable
		for scaledIndex := range pcsg.Spec.Replicas - minAvailable {
			spgName := apicommon.CreatePodGangNameFromPCSGFQN(pcsg.Name, int(scaledIndex))
			entries = append(entries, grovecorev1alpha1.PodGangEntry{
				Name:                       spgName,
				PodCliqueSetGenerationHash: generationHash,
				PodCliqueScalingGroups:     map[string]int32{pcsg.Name: 1},
			})
		}
	}

	return entries, nil
}

// buildResource configures the PodGangMap with the desired entries.
func (r _resource) buildResource(pgm *grovecorev1alpha1.PodGangMap, pcs *grovecorev1alpha1.PodCliqueSet, replicaIndex int, entries []grovecorev1alpha1.PodGangEntry) error {
	if err := controllerutil.SetControllerReference(pcs, pgm, r.scheme); err != nil {
		return groveerr.WrapError(err,
			errCodeSyncPodGangMap,
			component.OperationSync,
			fmt.Sprintf("Error setting controller reference on PodGangMap %s", pgm.Name),
		)
	}
	pgm.Labels = getLabels(pcs.Name, replicaIndex)
	pgm.Spec.PodCliqueSetReplicaIndex = int32(replicaIndex)
	pgm.Spec.Entries = entries
	return nil
}

// listPCSGsForPCS fetches all PodCliqueScalingGroups owned by the PodCliqueSet.
func (r _resource) listPCSGsForPCS(ctx context.Context, pcs *grovecorev1alpha1.PodCliqueSet) ([]grovecorev1alpha1.PodCliqueScalingGroup, error) {
	pcsgList := &grovecorev1alpha1.PodCliqueScalingGroupList{}
	if err := r.client.List(ctx,
		pcsgList,
		client.InNamespace(pcs.Namespace),
		client.MatchingLabels(apicommon.GetDefaultLabelsForPodCliqueSetManagedResources(pcs.Name)),
	); err != nil {
		return nil, err
	}
	return lo.Filter(pcsgList.Items, func(pcsg grovecorev1alpha1.PodCliqueScalingGroup, _ int) bool {
		return metav1.IsControlledBy(&pcsg, pcs)
	}), nil
}

// getSelectorLabels returns labels for selecting all PodGangMaps of a PodCliqueSet.
func getSelectorLabels(pcsName string) map[string]string {
	return lo.Assign(
		apicommon.GetDefaultLabelsForPodCliqueSetManagedResources(pcsName),
		map[string]string{
			apicommon.LabelComponentKey: apicommon.LabelComponentNamePodGangMap,
		},
	)
}

// getLabels returns labels for a PodGangMap resource.
func getLabels(pcsName string, replicaIndex int) map[string]string {
	return lo.Assign(
		getSelectorLabels(pcsName),
		map[string]string{
			apicommon.LabelAppNameKey:               fmt.Sprintf("%s-%d", pcsName, replicaIndex),
			apicommon.LabelPodCliqueSetReplicaIndex: strconv.Itoa(replicaIndex),
		},
	)
}

// emptyPodGangMap creates an empty PodGangMap with only metadata set.
func emptyPodGangMap(objKey client.ObjectKey) *grovecorev1alpha1.PodGangMap {
	return &grovecorev1alpha1.PodGangMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      objKey.Name,
			Namespace: objKey.Namespace,
		},
	}
}
