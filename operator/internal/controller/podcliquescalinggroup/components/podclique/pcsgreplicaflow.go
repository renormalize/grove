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
	"maps"
	"slices"
	"sort"
	"strings"

	apicommon "github.com/ai-dynamo/grove/operator/api/common"
	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	"github.com/ai-dynamo/grove/operator/internal/controller/common/component"
	componentutils "github.com/ai-dynamo/grove/operator/internal/controller/common/component/utils"
	groveerr "github.com/ai-dynamo/grove/operator/internal/errors"
	"github.com/ai-dynamo/grove/operator/internal/utils"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reconcilePCSGReplicaDistribution drives the desired-state-driven sync flow for the PodCliques
// owned by a PodCliqueScalingGroup. It updates pcsg.Status.PodGangMapping to the desired
// PodGang→PCSG-replica-indices mapping and then reconciles live PCLQs to match.
//
// Direction of authority:
//   - Coherent update in progress: PodGangMap (PGM) drives. status.PodGangMapping is
//     overwritten from PGM entries each reconcile.
//   - Steady state: status.PodGangMapping drives. Spec.Replicas changes are translated into
//     mapping mutations: scale-out mints a Scaled-PG entry per new replica (claiming a free
//     index from [0, Spec.Replicas)); scale-in walks two tiers (Scaled-PGs first, then
//     MPGs/TailPGs) and pops indices until the deficit is absorbed.
//
// The index↔PodGang binding is now recorded explicitly in status, so applyPCSGPerPodGangDeltas
// can recover desired PCLQ placement directly from the status mapping without scanning live
// PCLQ labels.
func (r _resource) reconcilePCSGReplicaDistribution(logger logr.Logger, sc *syncContext) error {
	desiredMapping, err := r.computeDesiredPCSGReplicaMapping(sc)
	if err != nil {
		return err
	}
	if err = r.patchPCSGPodGangMapping(sc, desiredMapping); err != nil {
		return err
	}
	return r.applyPCSGPerPodGangDeltas(logger, sc, desiredMapping)
}

// computeDesiredPCSGReplicaMapping returns the desired PodGang→PCSG-replica-indices mapping.
//
// During a coherent update PodGangMap is the authoritative source and the mapping is
// rebuilt from PGM entries every reconcile. In steady state the existing status mapping
// is the source of truth and is mutated only when Spec.Replicas drifts from sum-of-lengths.
func (r _resource) computeDesiredPCSGReplicaMapping(sc *syncContext) (map[string][]int32, error) {
	if componentutils.IsCoherentUpdateInProgress(sc.pcs) {
		return r.buildMappingFromPodGangMap(sc), nil
	}

	var desired map[string][]int32
	if len(sc.pcsg.Status.PodGangMapping) == 0 {
		// Fresh PCSG — seed from PGM (PGM was created by the PCS reconciler from spec).
		desired = r.buildMappingFromPodGangMap(sc)
	} else {
		desired = cloneIndexMapping(sc.pcsg.Status.PodGangMapping)
	}

	pcsNameReplica := apicommon.ResourceNameReplica{Name: sc.pcs.Name, Replica: sc.pcsReplicaIndex}
	pcsgConfigName := apicommon.ExtractScalingGroupNameFromPCSGFQN(sc.pcsg.Name, pcsNameReplica)
	scaledPGPrefix := scaledPodGangNamePrefix(sc.pcs.Name, sc.pcsReplicaIndex, *sc.pcs.Status.CurrentGenerationHash, pcsgConfigName)

	currentSum := sumIndexCount(desired)
	diff := sc.pcsg.Spec.Replicas - currentSum
	switch {
	case diff > 0:
		// For each missing replica, claim the smallest free index in [0, Spec.Replicas) and
		// mint a new Scaled-PG name to hold it.
		freeIndices := computeFreeIndices(desired, sc.pcsg.Spec.Replicas)
		if int32(len(freeIndices)) < diff {
			return nil, groveerr.New(errCodeUpdateStatus,
				component.OperationSync,
				fmt.Sprintf("not enough free PCSG replica indices to mint %d new entries for PodCliqueScalingGroup %v: free=%v",
					diff, client.ObjectKeyFromObject(sc.pcsg), freeIndices))
		}
		newNames, err := generateScaledPodGangNames(desired, int(diff), sc.pcs.Name, sc.pcsReplicaIndex, *sc.pcs.Status.CurrentGenerationHash, pcsgConfigName, scaledPGPrefix)
		if err != nil {
			return nil, groveerr.WrapError(err,
				errCodeUpdateStatus,
				component.OperationSync,
				fmt.Sprintf("cannot mint Scaled-PG names for PodCliqueScalingGroup %v",
					client.ObjectKeyFromObject(sc.pcsg)))
		}
		for i, name := range newNames {
			desired[name] = []int32{freeIndices[i]}
		}
	case diff < 0:
		legacySPGPrefix := sc.pcsg.Name + "-"
		bpgName := apicommon.GenerateBasePodGangName(pcsNameReplica)
		if err := decrementPCSGMappingForScaleIn(desired, int(-diff), scaledPGPrefix, legacySPGPrefix, bpgName); err != nil {
			return nil, groveerr.WrapError(err,
				errCodeUpdateStatus,
				component.OperationSync,
				fmt.Sprintf("cannot decrement PodGangMapping for scale-in on PodCliqueScalingGroup %v",
					client.ObjectKeyFromObject(sc.pcsg)))
		}
	}
	// Drop entries with empty index slices so the index space stays compact.
	// decrementPCSGMappingForScaleIn leaves entries empty rather than removing them.
	for name, indices := range desired {
		if len(indices) == 0 {
			delete(desired, name)
		}
	}
	return desired, nil
}

// cloneIndexMapping deep-clones the slice values of a PodGang→indices mapping so the caller
// can mutate without aliasing the original (which is typically a status field).
func cloneIndexMapping(in map[string][]int32) map[string][]int32 {
	out := make(map[string][]int32, len(in))
	for k, v := range in {
		out[k] = slices.Clone(v)
	}
	return out
}

// sumIndexCount returns the total count of PCSG replicas across all PodGangs in the mapping.
func sumIndexCount(mapping map[string][]int32) int32 {
	var total int32
	for _, indices := range mapping {
		total += int32(len(indices))
	}
	return total
}

// computeFreeIndices returns indices in [0, specReplicas) that are not currently claimed by
// any entry in `mapping`. Returned sorted ascending so the smallest free index is taken first.
func computeFreeIndices(mapping map[string][]int32, specReplicas int32) []int32 {
	taken := make(map[int32]struct{})
	for _, indices := range mapping {
		for _, idx := range indices {
			taken[idx] = struct{}{}
		}
	}
	free := make([]int32, 0, specReplicas)
	for i := int32(0); i < specReplicas; i++ {
		if _, ok := taken[i]; !ok {
			free = append(free, i)
		}
	}
	return free
}

// buildMappingFromPodGangMap constructs a PodGang→PCSG-replica-indices mapping from the PCS
// replica's PodGangMap (cached on sc.podGangMap). Entries that don't reference this PCSG are
// skipped; entries with empty index slices are skipped to keep the mapping compact.
func (r _resource) buildMappingFromPodGangMap(sc *syncContext) map[string][]int32 {
	pcsgConfigName := apicommon.ExtractScalingGroupNameFromPCSGFQN(sc.pcsg.Name, apicommon.ResourceNameReplica{Name: sc.pcs.Name, Replica: sc.pcsReplicaIndex})
	mapping := make(map[string][]int32, len(sc.podGangMap.Spec.Entries))
	for _, entry := range sc.podGangMap.Spec.Entries {
		indices, ok := entry.PCSGReplicaIndices[pcsgConfigName]
		if !ok || len(indices) == 0 {
			continue
		}
		cloned := slices.Clone(indices)
		slices.Sort(cloned)
		mapping[entry.Name] = cloned
	}
	return mapping
}

// scaledPodGangNamePrefix returns the common prefix of all Scaled-PodGang names minted by
// this PCSG reconciler under the current PCS generation hash. Used to classify Tier 1 entries
// in the status mapping.
//
// Format: <pcsName>-<pcsReplicaIndex>-<pcsGenerationHash>-<pcsgConfigName>-
func scaledPodGangNamePrefix(pcsName string, pcsReplicaIndex int, pcsGenerationHash string, pcsgConfigName string) string {
	return fmt.Sprintf("%s-%d-%s-%s-", pcsName, pcsReplicaIndex, pcsGenerationHash, pcsgConfigName)
}

// generateScaledPodGangNames generates `count` new Scaled-PodGang names. The next Scaled-PG name
// index is derived from the highest existing Scaled-PodGang entry in the mapping that matches
// the Scaled-PG prefix. Names are unique relative to the current mapping; no separate counter
// field is stored on PCSG status.
//
// Returns an error if any existing Scaled-PG entry name in the mapping fails to parse — such
// a name violates the Grove naming contract and indicates either status corruption or a
// controller bug, neither of which should be silently ignored.
//
// Format: <pcsName>-<pcsReplicaIndex>-<pcsGenerationHash>-<pcsgConfigName>-<index>
func generateScaledPodGangNames(currentMapping map[string][]int32, count int, pcsName string, pcsReplicaIndex int, pcsGenerationHash string, pcsgConfigName string, scaledPGPrefix string) ([]string, error) {
	nextIndex, err := nextScaledPodGangIndex(currentMapping, scaledPGPrefix)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, count)
	for range count {
		names = append(names, apicommon.GenerateScaledPodGangName(pcsName, int32(pcsReplicaIndex), pcsGenerationHash, pcsgConfigName, nextIndex))
		nextIndex++
	}
	return names, nil
}

// nextScaledPodGangIndex returns the next available *name* index for a Scaled-PG. It scans
// entries in the mapping that match the Scaled-PG prefix and returns max(index)+1, or 0 if
// no Scaled-PG entries exist. Returns an error if any matching entry's name fails to parse.
//
// Note: this is the index inside the PodGang *name*, distinct from the PCSG replica index
// the entry holds.
func nextScaledPodGangIndex(mapping map[string][]int32, scaledPGPrefix string) (int, error) {
	maxIdx := -1
	for name := range mapping {
		if !strings.HasPrefix(name, scaledPGPrefix) {
			continue
		}
		idx, err := utils.ExtractPodGangIndex(name)
		if err != nil {
			return 0, fmt.Errorf("Scaled-PG entry name %q in PCSG.Status.PodGangMapping does not match the expected naming convention: %w", name, err)
		}
		if idx > maxIdx {
			maxIdx = idx
		}
	}
	return maxIdx + 1, nil
}

// decrementPCSGMappingForScaleIn pops `count` PCSG replica indices from the desired mapping.
// BPG (the base PodGang in the legacy convention) is excluded from the walk entirely:
// popping from it would breach the PCS-level MinAvailable invariant and cause gang termination
// of the PCS replica. The webhook ensures Spec.Replicas - count >= MinAvailable, so the walk
// has enough drainable replicas without touching BPG.
//
// Selection proceeds in three tiers (each tier popped highest-replica-index first within the
// tier and entry):
//   - Tier 1: Legacy SPG entries (pre-upgrade scaled PodGangs, name matching legacySPGPrefix).
//     Each holds 1 replica. Sorted by trailing PG-name index descending.
//   - Tier 2: New-convention Scaled-PG entries (post-upgrade steady-state mints). Each holds
//     1 replica. Sorted by trailing PG-name index descending.
//   - Tier 3: MPG/TailPG entries (no SPG prefix, not BPG). Sorted by trailing PG-name index
//     descending. An MPG with multiple replica indices absorbs multiple steps (popping its
//     highest replica index each time).
//
// Walking legacy SPGs ahead of new Scaled-PGs naturally compacts older state first across a
// Grove-upgrade boundary, leaving newer mints in the mapping.
//
// Empty entries (zero indices remaining) are left in the mapping and dropped by the caller.
//
// Returns an error if any entry name in the mapping fails to parse — Grove is the sole writer
// of these names so an unparseable name is a contract violation, not a soft skip.
func decrementPCSGMappingForScaleIn(desired map[string][]int32, count int, scaledPGPrefix, legacySPGPrefix, bpgName string) error {
	if count <= 0 {
		return nil
	}
	tierLegacySPG, tierNewScaledPG, tierMPGTail := partitionPodGangNamesByTier(desired, scaledPGPrefix, legacySPGPrefix, bpgName)
	if err := sortDescByPodGangIndex(tierLegacySPG); err != nil {
		return fmt.Errorf("tier 1 (legacy SPG) entries in PCSG.Status.PodGangMapping have an unparseable name: %w", err)
	}
	if err := sortDescByPodGangIndex(tierNewScaledPG); err != nil {
		return fmt.Errorf("tier 2 (new Scaled-PG) entries in PCSG.Status.PodGangMapping have an unparseable name: %w", err)
	}
	if err := sortDescByPodGangIndex(tierMPGTail); err != nil {
		return fmt.Errorf("tier 3 (MPG/TailPG) entries in PCSG.Status.PodGangMapping have an unparseable name: %w", err)
	}

	remaining := count
	for _, name := range tierLegacySPG {
		if remaining == 0 {
			break
		}
		if popHighestIndex(desired, name) {
			remaining--
		}
	}
	for _, name := range tierNewScaledPG {
		if remaining == 0 {
			break
		}
		if popHighestIndex(desired, name) {
			remaining--
		}
	}
	for _, name := range tierMPGTail {
		for len(desired[name]) > 0 && remaining > 0 {
			popHighestIndex(desired, name)
			remaining--
		}
		if remaining == 0 {
			break
		}
	}
	return nil
}

// popHighestIndex pops the largest replica index from the slice at desired[name]. Returns true
// if a pop happened, false if the slice was empty. The slice is sorted in place to find the
// highest index deterministically; the smaller indices are kept in sorted order.
func popHighestIndex(desired map[string][]int32, name string) bool {
	indices := desired[name]
	if len(indices) == 0 {
		return false
	}
	slices.Sort(indices)
	desired[name] = indices[:len(indices)-1]
	return true
}

// partitionPodGangNamesByTier splits the names in the mapping into three tiers for scale-in:
//   - tierLegacySPG: legacy scaled PodGangs (names matching legacySPGPrefix). Each holds 1 replica.
//   - tierNewScaledPG: new-convention Scaled-PGs (names matching scaledPGPrefix). Each holds 1 replica.
//   - tierMPGTail: everything else (MPGs, TailPGs).
//
// bpgName is excluded from all tiers — BPG is the base PodGang carrying MinAvailable replicas
// in the legacy convention and must never be popped during scale-in.
//
// Legacy and new-convention prefixes are disjoint by name shape (the new convention has a
// `<hash>` segment between `<pcsReplicaIndex>` and `<pcsgConfigName>` that the legacy
// convention lacks), so a name matches at most one prefix.
func partitionPodGangNamesByTier(mapping map[string][]int32, scaledPGPrefix, legacySPGPrefix, bpgName string) (tierLegacySPG, tierNewScaledPG, tierMPGTail []string) {
	for name := range mapping {
		switch {
		case name == bpgName:
			continue
		case strings.HasPrefix(name, scaledPGPrefix):
			tierNewScaledPG = append(tierNewScaledPG, name)
		case strings.HasPrefix(name, legacySPGPrefix):
			tierLegacySPG = append(tierLegacySPG, name)
		default:
			tierMPGTail = append(tierMPGTail, name)
		}
	}
	return
}

// sortDescByPodGangIndex sorts names by their trailing PodGang index in descending order.
// Returns an error if any name's suffix fails to parse — Grove is the sole writer of these
// names so an unparseable name is a contract violation, not a soft skip.
func sortDescByPodGangIndex(names []string) error {
	indices := make(map[string]int, len(names))
	for _, name := range names {
		idx, err := utils.ExtractPodGangIndex(name)
		if err != nil {
			return fmt.Errorf("PodGang entry name %q does not match the expected naming convention: %w", name, err)
		}
		indices[name] = idx
	}
	sort.SliceStable(names, func(i, j int) bool {
		return indices[names[i]] > indices[names[j]]
	})
	return nil
}

// patchPCSGPodGangMapping persists the desired mapping to pcsg.Status.PodGangMapping if it
// differs from the current value. The check avoids waking other reconcilers via a no-op watch
// event. Empty maps are normalized to nil for status hygiene.
func (r _resource) patchPCSGPodGangMapping(sc *syncContext, desired map[string][]int32) error {
	if equalIndexMappings(sc.pcsg.Status.PodGangMapping, desired) {
		return nil
	}
	patch := client.MergeFrom(sc.pcsg.DeepCopy())
	if len(desired) == 0 {
		sc.pcsg.Status.PodGangMapping = nil
	} else {
		sc.pcsg.Status.PodGangMapping = desired
	}
	if err := client.IgnoreNotFound(r.client.Status().Patch(sc.ctx, sc.pcsg, patch)); err != nil {
		return groveerr.WrapError(err,
			errCodeUpdateStatus,
			component.OperationSync,
			fmt.Sprintf("failed to patch PodGangMapping on PodCliqueScalingGroup %v",
				client.ObjectKeyFromObject(sc.pcsg)))
	}
	return nil
}

// equalIndexMappings reports whether two PodGang→indices mappings have the same keys and
// the same (set-wise, since slice order on status is not contractual) indices per key.
// Used as a pre-patch no-op check to avoid spurious status updates.
func equalIndexMappings(a, b map[string][]int32) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || len(av) != len(bv) {
			return false
		}
		aSorted := slices.Clone(av)
		bSorted := slices.Clone(bv)
		slices.Sort(aSorted)
		slices.Sort(bSorted)
		if !slices.Equal(aSorted, bSorted) {
			return false
		}
	}
	return true
}

// applyPCSGPerPodGangDeltas reconciles live PCLQs to the desired mapping. For each (pgName, indices)
// pair in desired, every index `i` should have one PCLQ per CliqueName at FQN
// <pcsgFQN>-<i>-<cliqueName> labeled with grove.io/podgang=<pgName>. Indices not in
// ∪slices(desired) get their PCLQs deleted entirely.
//
// PCLQs whose live PodGang label disagrees with status are deleted; the next reconcile creates
// them under the correct PodGang. Deletes go before creates so the controller does not race
// with itself in creating PCLQs at indices currently held by a doomed PCLQ.
func (r _resource) applyPCSGPerPodGangDeltas(logger logr.Logger, sc *syncContext, desired map[string][]int32) error {
	desiredIndexToPG := make(map[int]string)
	for pgName, indices := range desired {
		for _, idx := range indices {
			desiredIndexToPG[int(idx)] = pgName
		}
	}

	deletions, creations := computePCSGCountDeltas(desiredIndexToPG, sc.existingPCLQs)

	if len(deletions) > 0 {
		if err := r.deletePCSGReplicas(logger, sc, deletions); err != nil {
			return err
		}
	}
	if len(creations) > 0 {
		if err := r.createPCSGReplicas(logger, sc, creations); err != nil {
			return err
		}
	}
	return nil
}

// computePCSGCountDeltas compares desiredIndexToPG (the authoritative replica index → PodGang
// mapping from status) against the live PCLQs and returns:
//   - deletions: replica indices whose live PCLQs should be deleted. Sources:
//     1. Indices not in desiredIndexToPG (obsolete — index belongs to no PodGang).
//     2. Indices whose live LabelPodGang disagrees with desired (the PCLQ will be recreated
//     under the correct PodGang on the next reconcile).
//   - creations: index → PodGang for indices in desired that have no surviving live PCLQ.
func computePCSGCountDeltas(desiredIndexToPG map[int]string, livePCLQs []grovecorev1alpha1.PodClique) (deletionIndices []int, creations map[int]string) {
	creations = make(map[int]string, len(desiredIndexToPG))
	maps.Copy(creations, desiredIndexToPG)

	// liveByIndex: PCSG replica index -> set of live PodGang labels seen on PCLQs at that index.
	// Multiple PCLQs at the same index (one per clique) all share the same LabelPodGang in steady
	// state, so the set is normally a singleton. A divergent set (>1 distinct labels) indicates
	// inconsistent state and is treated as a deletion target.
	liveByIndex := make(map[int]map[string]struct{})
	for _, pclq := range livePCLQs {
		idxStr, ok := pclq.Labels[apicommon.LabelPodCliqueScalingGroupReplicaIndex]
		if !ok {
			continue
		}
		idx := idxStr
		var idxInt int
		if _, err := fmt.Sscanf(idx, "%d", &idxInt); err != nil {
			continue
		}
		pgLabel := pclq.Labels[apicommon.LabelPodGang]
		set := liveByIndex[idxInt]
		if set == nil {
			set = make(map[string]struct{})
			liveByIndex[idxInt] = set
		}
		set[pgLabel] = struct{}{}
	}

	for idx, labels := range liveByIndex {
		desiredPG, inDesired := desiredIndexToPG[idx]
		if !inDesired {
			// Obsolete index — delete all live PCLQs at this index.
			deletionIndices = append(deletionIndices, idx)
			continue
		}
		if len(labels) != 1 {
			// Divergent labels at this index — delete and recreate.
			deletionIndices = append(deletionIndices, idx)
			continue
		}
		var liveLabel string
		for k := range labels {
			liveLabel = k
		}
		if liveLabel != desiredPG {
			// Wrong PodGang label — delete; next reconcile creates under the correct PodGang.
			deletionIndices = append(deletionIndices, idx)
			continue
		}
		// Correct binding already in place — no creation needed for this index.
		delete(creations, idx)
	}

	return
}

// deletePCSGReplicas deletes all PCLQs belonging to the given PCSG replica indices.
func (r _resource) deletePCSGReplicas(logger logr.Logger, sc *syncContext, replicaIndices []int) error {
	deletionTasks := r.createDeleteTasks(logger, sc.pcs, sc.pcsg.Name, replicaIndices, "delete excess PCSG replicas")
	return r.triggerDeletionOfPodCliques(sc.ctx, logger, client.ObjectKeyFromObject(sc.pcsg), deletionTasks)
}

// createPCSGReplicas creates the PCLQs for the given PCSG replica index → PodGang name
// assignments. Each replica generates one PCLQ per CliqueName in the PCSG config. Creation
// uses doCreate (a plain Create) since this path only handles PCLQs that don't yet exist; the
// OnDelete strategy's "preserve existing replicas via CreateOrPatch" behavior is irrelevant for
// fresh PCLQs.
func (r _resource) createPCSGReplicas(logger logr.Logger, sc *syncContext, assignments map[int]string) error {
	tasks := make([]utils.Task, 0, len(assignments)*len(sc.pcsg.Spec.CliqueNames))
	// Sort assignments by index for deterministic creation order.
	indices := lo.Keys(assignments)
	sort.Ints(indices)
	for _, pcsgReplicaIndex := range indices {
		podGangName := assignments[pcsgReplicaIndex]
		for _, cliqueName := range sc.pcsg.Spec.CliqueNames {
			pclqFQN := apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{Name: sc.pcsg.Name, Replica: pcsgReplicaIndex}, cliqueName)
			pclqObjectKey := client.ObjectKey{Name: pclqFQN, Namespace: sc.pcsg.Namespace}
			pgName := podGangName
			replicaIdx := pcsgReplicaIndex
			tasks = append(tasks, utils.Task{
				Name: fmt.Sprintf("CreatePodClique-%s", pclqFQN),
				Fn: func(ctx context.Context) error {
					return r.doCreate(ctx, logger, sc.pcs, sc.pcsg, replicaIdx, pclqObjectKey, pgName)
				},
			})
		}
	}
	if runResult := utils.RunConcurrently(sc.ctx, logger, tasks); runResult.HasErrors() {
		return groveerr.WrapError(runResult.GetAggregatedError(),
			errCodeCreatePodCliques,
			component.OperationSync,
			fmt.Sprintf("Error creating PodCliques for PodCliqueScalingGroup: %v, run summary: %s",
				client.ObjectKeyFromObject(sc.pcsg), runResult.GetSummary()))
	}
	return nil
}
