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
	"strings"

	apicommon "github.com/ai-dynamo/grove/operator/api/common"
	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	"github.com/ai-dynamo/grove/operator/internal/controller/common/component"
	componentutils "github.com/ai-dynamo/grove/operator/internal/controller/common/component/utils"
	groveerr "github.com/ai-dynamo/grove/operator/internal/errors"
	"github.com/ai-dynamo/grove/operator/internal/utils"

	groveschedulerv1alpha1 "github.com/ai-dynamo/grove/scheduler/api/core/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/samber/lo"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// syncContext holds the state required during the PodGangMap sync flow.
type syncContext struct {
	pcs              *grovecorev1alpha1.PodCliqueSet
	logger           logr.Logger
	pclqsByReplica   map[int][]grovecorev1alpha1.PodClique
	pcsgsByReplica   map[int][]grovecorev1alpha1.PodCliqueScalingGroup
	existingPGMNames []string
}

// prepareSyncFlow fetches the state needed for the sync flow.
func (r _resource) prepareSyncFlow(ctx context.Context, logger logr.Logger, pcs *grovecorev1alpha1.PodCliqueSet) (*syncContext, error) {
	var (
		err error
		sc  = &syncContext{
			pcs:    pcs,
			logger: logger,
		}
	)
	sc.pclqsByReplica, err = r.getPCLQsByReplica(ctx, pcs)
	if err != nil {
		return nil, err
	}
	sc.pcsgsByReplica, err = r.getPCSGsByReplica(ctx, pcs)
	if err != nil {
		return nil, err
	}
	sc.existingPGMNames, err = r.GetExistingResourceNames(ctx, logger, pcs.ObjectMeta)
	if err != nil {
		return nil, err
	}

	return sc, nil
}

// getPCLQsByReplica fetches all PCLQs for the PCS and groups them by PCS replica index.
func (r _resource) getPCLQsByReplica(ctx context.Context, pcs *grovecorev1alpha1.PodCliqueSet) (map[int][]grovecorev1alpha1.PodClique, error) {
	existingPCLQs, err := componentutils.GetPCLQsMatchingLabels(ctx, r.client, pcs.Namespace, apicommon.GetDefaultLabelsForPodCliqueSetManagedResources(pcs.Name))
	if err != nil {
		return nil, groveerr.WrapError(err,
			errCodeListPCLQs,
			component.OperationSync,
			fmt.Sprintf("Error listing PodCliques for PodCliqueSet: %v", client.ObjectKeyFromObject(pcs)),
		)
	}
	pclqsByReplica, err := componentutils.GroupPCLQsByPCSReplicaIndex(existingPCLQs)
	if err != nil {
		return nil, groveerr.WrapError(err,
			errCodeListPCLQs,
			component.OperationSync,
			fmt.Sprintf("Error grouping PodCliques by replica index for PodCliqueSet: %v", client.ObjectKeyFromObject(pcs)),
		)
	}
	return pclqsByReplica, nil
}

// getPCSGsByReplica fetches all PCSGs for the PCS and groups them by PCS replica index.
func (r _resource) getPCSGsByReplica(ctx context.Context, pcs *grovecorev1alpha1.PodCliqueSet) (map[int][]grovecorev1alpha1.PodCliqueScalingGroup, error) {
	existingPCSGs, err := componentutils.ListPCSGsForPCS(ctx, r.client, client.ObjectKeyFromObject(pcs))
	if err != nil {
		return nil, groveerr.WrapError(err,
			errCodeListPCLQs,
			component.OperationSync,
			fmt.Sprintf("Error listing PCSGs for PodCliqueSet: %v", client.ObjectKeyFromObject(pcs)),
		)
	}
	pcsgsByReplica, err := componentutils.GroupPCSGsByPCSReplicaIndex(existingPCSGs)
	if err != nil {
		return nil, groveerr.WrapError(err,
			errCodeListPCLQs,
			component.OperationSync,
			fmt.Sprintf("Error grouping PCSGs by replica index for PodCliqueSet: %v", client.ObjectKeyFromObject(pcs)),
		)
	}
	return pcsgsByReplica, nil
}

// runSyncFlow dispatches to the appropriate sync strategy based on PCS state.
func (r _resource) runSyncFlow(ctx context.Context, sc *syncContext) error {
	if componentutils.IsCoherentUpdateInProgress(sc.pcs) {
		return r.syncCoherentUpdateEntries(ctx, sc)
	}
	return r.syncSteadyStateEntries(ctx, sc)
}

// syncCoherentUpdateEntries computes and persists PodGangMap entries during a coherent update.
func (r _resource) syncCoherentUpdateEntries(ctx context.Context, sc *syncContext) error {
	expectedPGMNames := make([]string, 0, sc.pcs.Spec.Replicas)
	for pcsReplicaIndex := range sc.pcs.Spec.Replicas {
		pgmName := apicommon.GeneratePodGangMapName(apicommon.ResourceNameReplica{Name: sc.pcs.Name, Replica: int(pcsReplicaIndex)})
		expectedPGMNames = append(expectedPGMNames, pgmName)

		entries, err := r.computeCoherentUpdateEntries(ctx, sc.pcs, int(pcsReplicaIndex), sc.pclqsByReplica[int(pcsReplicaIndex)])
		if err != nil {
			return groveerr.WrapError(err,
				errCodeSyncPodGangMap,
				component.OperationSync,
				fmt.Sprintf("Error computing entries for PodGangMap %s: %v", pgmName, client.ObjectKeyFromObject(sc.pcs)),
			)
		}
		if err := r.createOrPatchPodGangMap(ctx, sc.pcs, pgmName, int(pcsReplicaIndex), entries); err != nil {
			return err
		}
	}

	// Delete excess PodGangMaps (from scale-in).
	for _, excessPGMName := range lo.Filter(sc.existingPGMNames, func(n string, _ int) bool { return !slices.Contains(expectedPGMNames, n) }) {
		pgm := emptyPodGangMap(client.ObjectKey{Namespace: sc.pcs.Namespace, Name: excessPGMName})
		if err := r.client.Delete(ctx, pgm); err != nil {
			return groveerr.WrapError(err,
				errCodeSyncPodGangMap,
				component.OperationSync,
				fmt.Sprintf("Error deleting excess PodGangMap %s for PodCliqueSet: %v", excessPGMName, client.ObjectKeyFromObject(sc.pcs)),
			)
		}
		sc.logger.Info("Deleted excess PodGangMap", "name", excessPGMName)
	}

	sc.logger.Info("Successfully synced PodGangMap resources during coherent update")
	return nil
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
	entryBuilder := componentutils.NewPodGangEntryBuilder(pcs.Name, int32(replicaIndex), newGenerationHash, &nextPodGangIndex)

	// MPG names from prior iterations of this update — Tail-PGs created in this iteration depend on them.
	mpgNames := collectMPGNamesFromEntries(existingNewEntries)

	// Compute next iteration's state.
	state := computeNextPodGangMapState(*template, oldEntries, mpgNames, entryBuilder)

	// Combine: updated old entries + previously created new entries + this iteration's new entries.
	var allEntries []grovecorev1alpha1.PodGangEntry
	allEntries = append(allEntries, state.oldEntries...)
	allEntries = append(allEntries, existingNewEntries...)
	allEntries = append(allEntries, state.newEntries...)
	return allEntries, nil
}

// collectMPGNamesFromEntries returns the names of MVU PodGang entries in the given slice.
// MPG entries have no DependsOn; Tail-PG entries depend on MPGs.
func collectMPGNamesFromEntries(entries []grovecorev1alpha1.PodGangEntry) []string {
	var names []string
	for _, entry := range entries {
		if len(entry.DependsOn) == 0 {
			names = append(names, entry.Name)
		}
	}
	return names
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
// whose generation hash differs from the PCS current generation hash, and builds PodGangEntries
// from their PodGroup specs.
func (r _resource) buildOldEntriesFromExistingPodGangs(ctx context.Context, pcs *grovecorev1alpha1.PodCliqueSet, replicaIndex int, pclqs []grovecorev1alpha1.PodClique) ([]grovecorev1alpha1.PodGangEntry, error) {
	existingPodGangs, err := componentutils.GetExistingPodGangs(ctx, r.client, pcs.ObjectMeta, pcs.Namespace)
	if err != nil {
		return nil, err
	}

	podGangsForReplica := lo.Filter(existingPodGangs, func(pg groveschedulerv1alpha1.PodGang, _ int) bool {
		pgReplicaIndex, ok := getPodGangPCSReplicaIndex(pg, pcs.Name)
		if !ok || pgReplicaIndex != replicaIndex {
			return false
		}
		return pg.Labels[apicommon.LabelPodCliqueSetGenerationHash] != *pcs.Status.CurrentGenerationHash
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

	populateDependsOnForReconstructedEntries(entries)
	return entries, nil
}

// populateDependsOnForReconstructedEntries sets DependsOn on entries reconstructed from existing
// PodGangs. Entries with non-empty PodCliques (BPG or MPG) have no deps. Entries with empty
// PodCliques (SPG or Tail-PG) depend on every sibling entry that has non-empty PodCliques.
func populateDependsOnForReconstructedEntries(entries []grovecorev1alpha1.PodGangEntry) {
	var anchorNames []string
	for _, entry := range entries {
		if len(entry.PodCliques) > 0 {
			anchorNames = append(anchorNames, entry.Name)
		}
	}
	if len(anchorNames) == 0 {
		return
	}
	for i := range entries {
		if len(entries[i].PodCliques) == 0 {
			entries[i].DependsOn = anchorNames
		}
	}
}

// getPodGangPCSReplicaIndex returns the PodCliqueSet replica index that owns this PodGang.
// Prefers the LabelPodCliqueSetReplicaIndex label; falls back to parsing the PodGang name
// for legacy PodGangs created before the label was stamped. PodGang names are of the form
// <pcsName>-<replicaIdx>[-<suffix>], so the segment immediately after <pcsName>- is the
// replica index. Returns (index, true) on success, (0, false) on parse failure.
func getPodGangPCSReplicaIndex(pg groveschedulerv1alpha1.PodGang, pcsName string) (int, bool) {
	if v, ok := pg.Labels[apicommon.LabelPodCliqueSetReplicaIndex]; ok {
		if idx, err := strconv.Atoi(v); err == nil {
			return idx, true
		}
	}
	prefix := pcsName + "-"
	if !strings.HasPrefix(pg.Name, prefix) {
		return 0, false
	}
	rest := pg.Name[len(prefix):]
	if dash := strings.IndexByte(rest, '-'); dash >= 0 {
		rest = rest[:dash]
	}
	idx, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return idx, true
}

// buildEntryFromPodGang constructs a PodGangEntry from an existing PodGang resource.
// It maps PodGroups to standalone PCLQ pod counts and PCSG replica counts.
func buildEntryFromPodGang(pcs *grovecorev1alpha1.PodCliqueSet, pcsGenerationHash string, pg groveschedulerv1alpha1.PodGang) (grovecorev1alpha1.PodGangEntry, error) {
	entry := grovecorev1alpha1.PodGangEntry{
		Name:                       pg.Name,
		PodCliqueSetGenerationHash: pcsGenerationHash,
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

// syncSteadyStateEntries ensures PodGangMap resources exist and reflect the current PCLQ/PCSG state.
// For each PCS replica:
//
//	  If its PodGangMap doesn't exist it is created:
//			1. For a new PCS replica it uses the PCS spec to create the expected PodGangs since nothing exists yet.
//			2. If the PCS replica exists, and it has Base and Scaled PodGangs, then it will compute expected based
//			on what should exist when using Base and Scaled PodGangs for a PCS replica.
//	  If the PodGangMap exists, it is synced from PCLQ/PCSG statuses.
func (r _resource) syncSteadyStateEntries(ctx context.Context, sc *syncContext) error {
	existingPGMNames := sets.New[string](sc.existingPGMNames...)
	for pcsReplicaIndex := range sc.pcs.Spec.Replicas {
		pgmName := apicommon.GeneratePodGangMapName(apicommon.ResourceNameReplica{Name: sc.pcs.Name, Replica: int(pcsReplicaIndex)})
		if !existingPGMNames.Has(pgmName) {
			if err := r.createPodGangMapForReplica(ctx, sc, pgmName, int(pcsReplicaIndex)); err != nil {
				return err
			}
			continue
		}
		entries := buildEntriesFromStatuses(sc.pcs, int(pcsReplicaIndex), sc.pclqsByReplica[int(pcsReplicaIndex)], sc.pcsgsByReplica[int(pcsReplicaIndex)])
		if err := r.createOrPatchPodGangMap(ctx, sc.pcs, pgmName, int(pcsReplicaIndex), entries); err != nil {
			return err
		}
	}
	return nil
}

// createPodGangMapForReplica creates a PodGangMap for a PCS replica that doesn't have one yet.
// For existing PCS with BasePodGang/ScaledPodGang structure it computes entries from existing resources.
// For new replicas (or new PCS) it computes MVU entries from PCS spec.
func (r _resource) createPodGangMapForReplica(ctx context.Context, sc *syncContext, pgmName string, pcsReplicaIndex int) error {
	var entries []grovecorev1alpha1.PodGangEntry
	if !hasMVUPodGangs(sc.pcs) && len(sc.pclqsByReplica[pcsReplicaIndex]) > 0 {
		entries = buildBaseAndScaledPodGangEntries(sc.pcs, pcsReplicaIndex, sc.pclqsByReplica[pcsReplicaIndex], sc.pcsgsByReplica[pcsReplicaIndex])
	} else {
		entries = computeMVUEntriesFromSpec(sc.pcs, pcsReplicaIndex)
	}
	return r.createOrPatchPodGangMap(ctx, sc.pcs, pgmName, pcsReplicaIndex, entries)
}

// hasMVUPodGangs returns true if the PCS has MVU-based PodGangs.
func hasMVUPodGangs(pcs *grovecorev1alpha1.PodCliqueSet) bool {
	return len(pcs.Status.PodGangCounter) > 0
}

// buildBaseAndScaledPodGangEntries constructs PodGangMap entries using Base and Scaled PodGang structure.
func buildBaseAndScaledPodGangEntries(pcs *grovecorev1alpha1.PodCliqueSet, pcsReplicaIndex int, pclqs []grovecorev1alpha1.PodClique, pcsgs []grovecorev1alpha1.PodCliqueScalingGroup) []grovecorev1alpha1.PodGangEntry {
	pcsNameReplica := apicommon.ResourceNameReplica{Name: pcs.Name, Replica: pcsReplicaIndex}
	pcsGenerationHash := *pcs.Status.CurrentGenerationHash

	entries := []grovecorev1alpha1.PodGangEntry{
		buildBasePodGangEntry(pcs, pcsNameReplica, pcsGenerationHash, pclqs),
	}
	entries = append(entries, buildScaledPodGangEntries(pcs, pcsNameReplica, pcsGenerationHash, pcsgs)...)
	return entries
}

// buildBasePodGangEntry constructs the Base PodGang entry containing all standalone PCLQs
// and minAvailable replicas of each PCSG.
func buildBasePodGangEntry(pcs *grovecorev1alpha1.PodCliqueSet, pcsNameReplica apicommon.ResourceNameReplica, pcsGenerationHash string, pclqs []grovecorev1alpha1.PodClique) grovecorev1alpha1.PodGangEntry {
	bpgPodCliques := make(map[string]int32)
	for i := range pclqs {
		if !componentutils.IsStandalonePCLQ(&pclqs[i]) {
			continue
		}
		cliqueName, _ := utils.GetPodCliqueNameFromPodCliqueFQN(pclqs[i].ObjectMeta)
		bpgPodCliques[cliqueName] = pclqs[i].Spec.Replicas
	}

	bpgPCSGs := make(map[string]int32)
	for _, pcsgConfig := range pcs.Spec.Template.PodCliqueScalingGroupConfigs {
		bpgPCSGs[pcsgConfig.Name] = *pcsgConfig.MinAvailable
	}

	return grovecorev1alpha1.PodGangEntry{
		Name:                       apicommon.GenerateBasePodGangName(pcsNameReplica),
		PodCliqueSetGenerationHash: pcsGenerationHash,
		PodCliques:                 bpgPodCliques,
		PodCliqueScalingGroups:     bpgPCSGs,
	}
}

// buildScaledPodGangEntries constructs Scaled PodGang entries — one per PCSG replica above minAvailable.
func buildScaledPodGangEntries(pcs *grovecorev1alpha1.PodCliqueSet, pcsNameReplica apicommon.ResourceNameReplica, pcsGenerationHash string, pcsgs []grovecorev1alpha1.PodCliqueScalingGroup) []grovecorev1alpha1.PodGangEntry {
	var entries []grovecorev1alpha1.PodGangEntry
	bpgName := apicommon.GenerateBasePodGangName(pcsNameReplica)
	for _, pcsgConfig := range pcs.Spec.Template.PodCliqueScalingGroupConfigs {
		pcsgFQN := apicommon.GeneratePodCliqueScalingGroupName(pcsNameReplica, pcsgConfig.Name)
		currentReplicas := getPCSGCurrentReplicas(pcsgs, pcsgFQN, pcsgConfig)
		minAvailable := int(*pcsgConfig.MinAvailable)
		for spgIndex := 0; spgIndex < currentReplicas-minAvailable; spgIndex++ {
			entries = append(entries, grovecorev1alpha1.PodGangEntry{
				Name:                       apicommon.CreatePodGangNameFromPCSGFQN(pcsgFQN, spgIndex),
				PodCliqueSetGenerationHash: pcsGenerationHash,
				PodCliqueScalingGroups:     map[string]int32{pcsgConfig.Name: 1},
				DependsOn:                  []string{bpgName},
			})
		}
	}
	return entries
}

// getPCSGCurrentReplicas returns the current replica count for a PCSG.
// If the PCSG resource exists, uses its Spec.Replicas; otherwise falls back to the config default.
func getPCSGCurrentReplicas(pcsgs []grovecorev1alpha1.PodCliqueScalingGroup, pcsgFQN string, pcsgConfig grovecorev1alpha1.PodCliqueScalingGroupConfig) int {
	for _, pcsg := range pcsgs {
		if pcsg.Name == pcsgFQN {
			return int(pcsg.Spec.Replicas)
		}
	}
	return int(*pcsgConfig.Replicas)
}

// computeMVUEntriesFromSpec computes all MVU PodGang and Tail-PG entries for a PCS replica from spec.
func computeMVUEntriesFromSpec(pcs *grovecorev1alpha1.PodCliqueSet, pcsReplicaIndex int) []grovecorev1alpha1.PodGangEntry {
	template := componentutils.ComputeMVUTemplateFromPCS(pcs)
	pcsGenerationHash := *pcs.Status.CurrentGenerationHash
	standalonePCLQReplicas := componentutils.GetStandalonePCLQReplicasFromPCS(pcs)
	pcsgReplicas := componentutils.GetPCSGReplicasFromPCS(pcs)

	var podGangIndex int32
	entryBuilder := componentutils.NewPodGangEntryBuilder(pcs.Name, int32(pcsReplicaIndex), pcsGenerationHash, &podGangIndex)
	return computeAllMVUPodGangEntries(template, standalonePCLQReplicas, pcsgReplicas, entryBuilder)
}

// computeAllMVUPodGangEntries computes all MPG and Tail-PG entries by distributing
// the given standalone PCLQ replicas and PCSG replicas into MVU PodGangs.
func computeAllMVUPodGangEntries(
	template componentutils.MVUTemplate,
	standalonePCLQReplicas map[string]int32,
	pcsgReplicas map[string]int32,
	entryBuilder componentutils.PodGangEntryBuilder,
) []grovecorev1alpha1.PodGangEntry {
	var entries []grovecorev1alpha1.PodGangEntry
	var mpgNames []string

	canFormAnotherMVU := canFormMVUFromRemainingReplicas(template, standalonePCLQReplicas, pcsgReplicas)
	for canFormAnotherMVU {
		mvuPCLQs := make(map[string]int32)
		for name, minAvail := range template.StandalonePCLQs {
			mvuPCLQs[name] = minAvail
			standalonePCLQReplicas[name] -= minAvail
		}
		mvuPCSGs := make(map[string]int32)
		for name, minAvail := range template.PCSGs {
			mvuPCSGs[name] = minAvail
			pcsgReplicas[name] -= minAvail
		}

		canFormAnotherMVU = canFormMVUFromRemainingReplicas(template, standalonePCLQReplicas, pcsgReplicas)
		if !canFormAnotherMVU {
			for name := range template.StandalonePCLQs {
				if standalonePCLQReplicas[name] > 0 {
					mvuPCLQs[name] += standalonePCLQReplicas[name]
					standalonePCLQReplicas[name] = 0
				}
			}
		}

		mpgEntry := entryBuilder(mvuPCLQs, mvuPCSGs, nil)
		mpgNames = append(mpgNames, mpgEntry.Name)
		entries = append(entries, mpgEntry)
	}

	for name := range template.PCSGs {
		for pcsgReplicas[name] > 0 {
			entries = append(entries, entryBuilder(nil, map[string]int32{name: 1}, mpgNames))
			pcsgReplicas[name]--
		}
	}

	return entries
}

// canFormMVUFromRemainingReplicas checks if there are enough remaining replicas to form a complete MVU PodGang.
func canFormMVUFromRemainingReplicas(template componentutils.MVUTemplate, standalonePCLQReplicas, pcsgReplicas map[string]int32) bool {
	if len(template.StandalonePCLQs) == 0 && len(template.PCSGs) == 0 {
		return false
	}
	for name, minAvail := range template.StandalonePCLQs {
		if standalonePCLQReplicas[name] < minAvail {
			return false
		}
	}
	for name, minAvail := range template.PCSGs {
		if pcsgReplicas[name] < minAvail {
			return false
		}
	}
	return true
}

// buildEntriesFromStatuses constructs PodGangMap entries by reading PodGangMapping from PCLQ and PCSG statuses.
func buildEntriesFromStatuses(pcs *grovecorev1alpha1.PodCliqueSet, pcsReplicaIndex int, pclqs []grovecorev1alpha1.PodClique, pcsgs []grovecorev1alpha1.PodCliqueScalingGroup) []grovecorev1alpha1.PodGangEntry {
	entryMap := make(map[string]*grovecorev1alpha1.PodGangEntry)
	pcsGenerationHash := *pcs.Status.CurrentGenerationHash

	for i := range pclqs {
		cliqueName, _ := utils.GetPodCliqueNameFromPodCliqueFQN(pclqs[i].ObjectMeta)
		for pgName, podCount := range pclqs[i].Status.PodGangMapping {
			entry := getOrCreateEntry(entryMap, pgName, pcsGenerationHash)
			if entry.PodCliques == nil {
				entry.PodCliques = make(map[string]int32)
			}
			entry.PodCliques[cliqueName] = podCount
		}
	}

	for i := range pcsgs {
		pcsgName := apicommon.ExtractScalingGroupNameFromPCSGFQN(pcsgs[i].Name, apicommon.ResourceNameReplica{Name: pcs.Name, Replica: pcsReplicaIndex})
		for pgName, replicaCount := range pcsgs[i].Status.PodGangMapping {
			entry := getOrCreateEntry(entryMap, pgName, pcsGenerationHash)
			if entry.PodCliqueScalingGroups == nil {
				entry.PodCliqueScalingGroups = make(map[string]int32)
			}
			entry.PodCliqueScalingGroups[pcsgName] = replicaCount
		}
	}

	entries := make([]grovecorev1alpha1.PodGangEntry, 0, len(entryMap))
	for _, entry := range entryMap {
		entries = append(entries, *entry)
	}
	return entries
}

func getOrCreateEntry(entryMap map[string]*grovecorev1alpha1.PodGangEntry, pgName, pcsGenerationHash string) *grovecorev1alpha1.PodGangEntry {
	if entry, ok := entryMap[pgName]; ok {
		return entry
	}
	entry := &grovecorev1alpha1.PodGangEntry{
		Name:                       pgName,
		PodCliqueSetGenerationHash: pcsGenerationHash,
	}
	entryMap[pgName] = entry
	return entry
}

// createOrPatchPodGangMap creates or patches a PodGangMap with the given entries.
func (r _resource) createOrPatchPodGangMap(ctx context.Context, pcs *grovecorev1alpha1.PodCliqueSet, pgmName string, pcsReplicaIndex int, entries []grovecorev1alpha1.PodGangEntry) error {
	pgm := emptyPodGangMap(client.ObjectKey{Namespace: pcs.Namespace, Name: pgmName})
	if _, err := controllerutil.CreateOrPatch(ctx, r.client, pgm, func() error {
		return r.buildResource(pgm, pcs, pcsReplicaIndex, entries)
	}); err != nil {
		return groveerr.WrapError(err, errCodeSyncPodGangMap, component.OperationSync,
			fmt.Sprintf("Error creating or updating PodGangMap %s for PodCliqueSet: %v", pgmName, client.ObjectKeyFromObject(pcs)))
	}
	return nil
}

// hasInFlightPodGangs returns true if the orchestrator has in-flight PodGangs awaiting availability.
func hasInFlightPodGangs(pcs *grovecorev1alpha1.PodCliqueSet) bool {
	return pcs.Status.UpdateProgress != nil &&
		len(pcs.Status.UpdateProgress.CurrentlyUpdating) > 0 &&
		len(pcs.Status.UpdateProgress.CurrentlyUpdating[0].InFlightPodGangs) > 0
}
