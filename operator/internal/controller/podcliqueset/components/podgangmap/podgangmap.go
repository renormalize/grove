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
	groveschedulerv1alpha1 "github.com/ai-dynamo/grove/scheduler/api/core/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

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

// Sync creates or updates PodGangMap resources only during coherent updates.
// PodGangMap is the single source of truth for PodGang recomputation during coherent updates.
// For all other cases (RollingRecreate, OnDelete, steady state), PodGangMaps should not exist —
// the PodGang component manages PodGangs directly from PCS spec and existing resources.
func (r _resource) Sync(ctx context.Context, logger logr.Logger, pcs *grovecorev1alpha1.PodCliqueSet) error {
	logger.Info("Syncing PodGangMap resources")

	existingPGMNames, err := r.GetExistingResourceNames(ctx, logger, pcs.ObjectMeta)
	if err != nil {
		return err
	}

	// If the coherent update is not in-progress then delete any leftover PodGangMap resources if they exist and exit early.
	// The life-span of PodGangMap resource is only during an in-process coherent update.
	if !componentutils.IsCoherentUpdateInProgress(pcs) {
		if len(existingPGMNames) == 0 {
			return nil
		}
		return r.Delete(ctx, logger, pcs.ObjectMeta)
	}

	// If InFlightPodGangs is populated, the orchestrator is still waiting for them to become
	// Available. The PodGangMap already has the correct entries from the previous reconcile —
	// skip recomputation entirely.
	if hasInFlightPodGangs(pcs) {
		return nil
	}

	// Coherent update in progress: create/update PodGangMaps.
	existingPCLQs, err := componentutils.GetPCLQsMatchingLabels(ctx, r.client, pcs.Namespace, apicommon.GetDefaultLabelsForPodCliqueSetManagedResources(pcs.Name))
	if err != nil {
		return groveerr.WrapError(err,
			errCodeListPCLQs,
			component.OperationSync,
			fmt.Sprintf("Error listing PodCliques for PodCliqueSet: %v", client.ObjectKeyFromObject(pcs)),
		)
	}

	expectedPGMNames := make([]string, 0, pcs.Spec.Replicas)
	for replicaIndex := range pcs.Spec.Replicas {
		pgmName := apicommon.GeneratePodGangMapName(apicommon.ResourceNameReplica{Name: pcs.Name, Replica: int(replicaIndex)})
		expectedPGMNames = append(expectedPGMNames, pgmName)

		pclqsForReplica := lo.Filter(existingPCLQs, func(pclq grovecorev1alpha1.PodClique, _ int) bool {
			return pclq.Labels[apicommon.LabelPodCliqueSetReplicaIndex] == strconv.Itoa(int(replicaIndex))
		})

		entries, err := r.computeCoherentUpdateEntries(ctx, pcs, int(replicaIndex), pclqsForReplica)
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

// computeCoherentUpdateEntries computes PodGangMap entries for a coherent update.
// On the first reconcile (PodGangMap doesn't exist yet): it initializes old entries from existing
// PodGang resources and computes the first iteration's new entries.
// On subsequent reconciles (PodGangMap exists): it reads existing entries, separates into old-hash
// and new-hash, and computes next iteration's entries.
// Returns the complete set of entries for the PodGangMap: updated old entries + all new entries.
func (r _resource) computeCoherentUpdateEntries(ctx context.Context, pcs *grovecorev1alpha1.PodCliqueSet, replicaIndex int, pclqs []grovecorev1alpha1.PodClique) ([]grovecorev1alpha1.PodGangEntry, error) {
	// Compute the MVU template from current PCS spec vs live PCLQ hashes.
	template, err := computeMVUTemplate(pcs, pclqs)
	if err != nil {
		return nil, err
	}

	newGenerationHash := *pcs.Status.CurrentGenerationHash
	// Get old-hash and previously-created new-hash entries.
	oldEntries, existingNewEntries, err := r.getOldAndNewEntries(ctx, pcs, replicaIndex, pclqs, newGenerationHash)
	if err != nil {
		return nil, err
	}

	// Build the entry builder closure for generating new PodGang names.
	nextPodGangIndex := getCreatedPodGangCount(pcs, replicaIndex)
	entryBuilder := func(standalonePCLQPods map[string]int32, pcsgReplicas map[string]int32) grovecorev1alpha1.PodGangEntry {
		name := apicommon.GeneratePodGangName(pcs.Name, int32(replicaIndex), newGenerationHash, nextPodGangIndex)
		nextPodGangIndex++
		return grovecorev1alpha1.PodGangEntry{
			Name:                       name,
			PodCliqueSetGenerationHash: newGenerationHash,
			PodCliques:                 standalonePCLQPods,
			PodCliqueScalingGroups:     pcsgReplicas,
		}
	}

	// Compute next iteration's state.
	state := computeNextPodGangMapState(*template, oldEntries, entryBuilder)

	// Combine: updated old entries + previously created new entries + this iteration's new entries.
	var allEntries []grovecorev1alpha1.PodGangEntry
	allEntries = append(allEntries, state.oldEntries...)
	allEntries = append(allEntries, existingNewEntries...)
	allEntries = append(allEntries, state.newEntries...)
	return allEntries, nil
}

// hasInFlightPodGangs returns true if the orchestrator has in-flight PodGangs awaiting availability.
func hasInFlightPodGangs(pcs *grovecorev1alpha1.PodCliqueSet) bool {
	return len(pcs.Status.UpdateProgress.CurrentlyUpdating) > 0 &&
		len(pcs.Status.UpdateProgress.CurrentlyUpdating[0].InFlightPodGangs) > 0
}

// getOldAndNewEntries retrieves old-hash and new-hash entries for the given replica.
// If the PodGangMap doesn't exist yet (first reconcile of this update), it initializes
// old entries from existing PodGang resources.
func (r _resource) getOldAndNewEntries(ctx context.Context, pcs *grovecorev1alpha1.PodCliqueSet, replicaIndex int, pclqs []grovecorev1alpha1.PodClique, newHash string) (oldEntries, newEntries []grovecorev1alpha1.PodGangEntry, err error) {
	pgmName := apicommon.GeneratePodGangMapName(apicommon.ResourceNameReplica{Name: pcs.Name, Replica: replicaIndex})
	pgm := &grovecorev1alpha1.PodGangMap{}
	err = r.client.Get(ctx, client.ObjectKey{Namespace: pcs.Namespace, Name: pgmName}, pgm)

	if err != nil {
		if apierrors.IsNotFound(err) {
			// PodGangMap doesn't exist yet — first reconcile of this update.
			oldEntries, err = r.buildOldEntriesFromExistingPodGangs(ctx, pcs, replicaIndex, pclqs)
			return
		}
		return
	}

	// PodGangMap exists — separate entries by generation hash.
	for _, entry := range pgm.Spec.Entries {
		if entry.PodCliqueSetGenerationHash == newHash {
			newEntries = append(newEntries, entry)
		} else {
			oldEntries = append(oldEntries, entry)
		}
	}
	return
}

// buildOldEntriesFromExistingPodGangs lists existing PodGang resources for this PCS replica
// and builds PodGangEntries from their PodGroup specs. At the start of a coherent update,
// all existing PodGangs are treated as old (not matching the new generation hash).
func (r _resource) buildOldEntriesFromExistingPodGangs(ctx context.Context, pcs *grovecorev1alpha1.PodCliqueSet, replicaIndex int, pclqs []grovecorev1alpha1.PodClique) ([]grovecorev1alpha1.PodGangEntry, error) {
	existingPodGangs, err := componentutils.GetExistingPodGangs(ctx, r.client, pcs.ObjectMeta, pcs.Namespace)
	if err != nil {
		return nil, err
	}

	podGangsForReplica := lo.Filter(existingPodGangs, func(pg groveschedulerv1alpha1.PodGang, _ int) bool {
		return pg.Labels[apicommon.LabelPodCliqueSetReplicaIndex] == strconv.Itoa(replicaIndex)
	})

	oldPCSHash := getOldPodCliqueSetGenerationHash(pclqs)

	entries := make([]grovecorev1alpha1.PodGangEntry, 0, len(podGangsForReplica))
	for _, pg := range podGangsForReplica {
		entry, err := buildEntryFromPodGang(pcs, oldPCSHash, pg)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// buildEntryFromPodGang constructs a PodGangEntry from an existing PodGang resource.
// It maps PodGroups to standalone PCLQ pod counts and PCSG replica counts.
func buildEntryFromPodGang(pcs *grovecorev1alpha1.PodCliqueSet, generationHash string, pg groveschedulerv1alpha1.PodGang) (grovecorev1alpha1.PodGangEntry, error) {
	entry := grovecorev1alpha1.PodGangEntry{
		Name:                       pg.Name,
		PodCliqueSetGenerationHash: generationHash,
		PodCliques:                 make(map[string]int32),
		PodCliqueScalingGroups:     make(map[string]int32),
	}

	for _, podGroup := range pg.Spec.PodGroups {
		cliqueName, err := extractCliqueName(podGroup.Name, pcs)
		if err != nil {
			return grovecorev1alpha1.PodGangEntry{}, err
		}
		pcsgConfig := componentutils.FindScalingGroupConfigForClique(pcs.Spec.Template.PodCliqueScalingGroupConfigs, cliqueName)
		if pcsgConfig == nil {
			entry.PodCliques[cliqueName] = int32(len(podGroup.PodReferences))
		}
	}

	// For PCSGs: count replicas by dividing total PodGroups for this PCSG by number of constituent cliques.
	for _, pcsgConfig := range pcs.Spec.Template.PodCliqueScalingGroupConfigs {
		var numPodGroupsForPCSG int32
		for _, podGroup := range pg.Spec.PodGroups {
			cliqueName, _ := extractCliqueName(podGroup.Name, pcs)
			if slices.Contains(pcsgConfig.CliqueNames, cliqueName) {
				numPodGroupsForPCSG++
			}
		}
		if numPodGroupsForPCSG > 0 {
			// Each PCSG replica contributes one PodGroup per constituent clique.
			// So replica count = total PodGroups for this PCSG / number of cliques in the PCSG.
			entry.PodCliqueScalingGroups[pcsgConfig.Name] = numPodGroupsForPCSG / int32(len(pcsgConfig.CliqueNames))
		}
	}

	return entry, nil
}

// extractCliqueName extracts the unqualified clique name from a PodGroup name (PCLQ FQN)
// by matching against known clique templates in the PCS spec.
// Returns an error if the PodGroup name does not match any known clique template.
func extractCliqueName(podGroupName string, pcs *grovecorev1alpha1.PodCliqueSet) (string, error) {
	for _, cliqueTemplate := range pcs.Spec.Template.Cliques {
		if cliqueTemplate == nil {
			continue
		}
		suffix := "-" + cliqueTemplate.Name
		if len(podGroupName) > len(suffix) && podGroupName[len(podGroupName)-len(suffix):] == suffix {
			return cliqueTemplate.Name, nil
		}
	}
	return "", fmt.Errorf("PodGroup name %q does not match any known clique template in PCS %s", podGroupName, pcs.Name)
}

// getOldPodCliqueSetGenerationHash returns the generation hash that existing PCLQs are currently running.
// At the start of a coherent update, this is the hash before the update was triggered.
// The exact value doesn't matter for computation — it only needs to differ from the new hash.
func getOldPodCliqueSetGenerationHash(pclqs []grovecorev1alpha1.PodClique) string {
	for _, pclq := range pclqs {
		if pclq.Status.CurrentPodCliqueSetGenerationHash != nil {
			return *pclq.Status.CurrentPodCliqueSetGenerationHash
		}
	}
	return ""
}

// getCreatedPodGangCount returns the number of PodGangs created so far for the given replica index.
func getCreatedPodGangCount(pcs *grovecorev1alpha1.PodCliqueSet, replicaIndex int) int32 {
	if pcs.Status.PodGangCounter == nil {
		return 0
	}
	return pcs.Status.PodGangCounter[strconv.Itoa(replicaIndex)]
}
