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
	"strconv"
	"strings"

	apicommon "github.com/ai-dynamo/grove/operator/api/common"
	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	"github.com/ai-dynamo/grove/operator/internal/controller/common/component"
	componentutils "github.com/ai-dynamo/grove/operator/internal/controller/common/component/utils"
	groveerr "github.com/ai-dynamo/grove/operator/internal/errors"
	"github.com/ai-dynamo/grove/operator/internal/utils"
	k8sutils "github.com/ai-dynamo/grove/operator/internal/utils/kubernetes"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reconcilePCSGReplicaDistribution drives the desired-state-driven sync flow for the PodCliques
// owned by a PodCliqueScalingGroup. It updates pcsg.Status.PodGangMapping to the desired
// PodGang→PCSG-replica distribution and then reconciles live PCLQs to match.
//
// Direction of authority:
//   - Coherent update in progress: PodGangMap (PGM) drives. status.PodGangMapping is
//     overwritten from PGM entries each reconcile.
//   - Steady state: status.PodGangMapping drives. Spec.Replicas changes are translated into
//     mapping mutations: scale-out mints Scaled-PG entries (one per new replica) and adds
//     them to the mapping; scale-in walks two tiers (Scaled-PGs first, then MPGs/TailPGs)
//     and decrements until the deficit is absorbed.
//
// The index↔PodGang binding is recovered each reconcile from live PCLQ labels rather than
// persisted in status. Per-PodGang count diffs against the live binding identify holes
// (Case 1: external delete) and excess (Case 2: scale-in); new PCLQs draw indices from the
// unoccupied tail of [0, Spec.Replicas).
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

// computeDesiredPCSGReplicaMapping returns the desired PodGang→PCSG-replicas mapping.
//
// During a coherent update PodGangMap is an authoritative source and the mapping is
// rebuilt from PGM entries every reconcile.
// In steady state the existing status mapping is the source of truth and is
// mutated only when Spec.Replicas drifts from sum(mapping).
func (r _resource) computeDesiredPCSGReplicaMapping(sc *syncContext) (map[string]int32, error) {
	if componentutils.IsCoherentUpdateInProgress(sc.pcs) {
		return r.buildMappingFromPodGangMap(sc), nil
	}

	var desired map[string]int32
	if len(sc.pcsg.Status.PodGangMapping) == 0 {
		// Fresh PCSG — seed from PGM (PGM was created by the PCS reconciler from spec).
		desired = r.buildMappingFromPodGangMap(sc)
	} else {
		desired = maps.Clone(sc.pcsg.Status.PodGangMapping)
	}

	pcsNameReplica := apicommon.ResourceNameReplica{Name: sc.pcs.Name, Replica: sc.pcsReplicaIndex}
	pcsgConfigName := apicommon.ExtractScalingGroupNameFromPCSGFQN(sc.pcsg.Name, pcsNameReplica)
	scaledPGPrefix := scaledPodGangNamePrefix(sc.pcs.Name, sc.pcsReplicaIndex, *sc.pcs.Status.CurrentGenerationHash, pcsgConfigName)

	// currentSum is the total number of PCSG replicas across all PodGang mappings.
	currentSum := lo.Reduce(lo.Values(desired), func(agg int32, v int32, _ int) int32 { return agg + v }, int32(0))
	diff := sc.pcsg.Spec.Replicas - currentSum
	switch {
	case diff > 0:
		// Generate `diff` new Scaled-PG names. Each new entry holds one PCSG replica.
		newNames, err := generateScaledPodGangNames(desired, int(diff), sc.pcs.Name, sc.pcsReplicaIndex, *sc.pcs.Status.CurrentGenerationHash, pcsgConfigName, scaledPGPrefix)
		if err != nil {
			return nil, groveerr.WrapError(err,
				errCodeUpdateStatus,
				component.OperationSync,
				fmt.Sprintf("cannot mint Scaled-PG names for PodCliqueScalingGroup %v",
					client.ObjectKeyFromObject(sc.pcsg)))
		}
		for _, name := range newNames {
			desired[name] = 1
		}
	case diff < 0:
		// Legacy SPG name format (pre-upgrade): <pcsgFQN>-<index>. Same prefix as PCSG FQN + "-".
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
	return desired, nil
}

// buildMappingFromPodGangMap constructs a PodGang→PCSG-replica mapping from the PCS replica's
// PodGangMap (cached on sc.podGangMap). Entries that don't reference this PCSG are skipped;
// zero counts are skipped to keep the mapping compact.
func (r _resource) buildMappingFromPodGangMap(sc *syncContext) map[string]int32 {
	pcsgConfigName := apicommon.ExtractScalingGroupNameFromPCSGFQN(sc.pcsg.Name, apicommon.ResourceNameReplica{Name: sc.pcs.Name, Replica: sc.pcsReplicaIndex})
	mapping := make(map[string]int32, len(sc.podGangMap.Spec.Entries))
	for _, entry := range sc.podGangMap.Spec.Entries {
		count, ok := entry.PodCliqueScalingGroups[pcsgConfigName]
		if !ok || count == 0 {
			continue
		}
		mapping[entry.Name] = count
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

// generateScaledPodGangNames generates `count` new Scaled-PodGang names. The next Scaled-PG index
// is derived from the highest existing Scaled-PodGang entry in the mapping that matches the
// Scaled-PG prefix. Names are unique relative to the current mapping; no separate counter
// field is stored on PCSG status.
//
// Returns an error if any existing Scaled-PG entry name in the mapping fails to parse — such
// a name violates the Grove naming contract and indicates either status corruption or a
// controller bug, neither of which should be silently ignored.
//
// Format: <pcsName>-<pcsReplicaIndex>-<pcsGenerationHash>-<pcsgConfigName>-<index>
func generateScaledPodGangNames(currentMapping map[string]int32, count int, pcsName string, pcsReplicaIndex int, pcsGenerationHash string, pcsgConfigName string, scaledPGPrefix string) ([]string, error) {
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

// nextScaledPodGangIndex returns the next available index for a Scaled-PG. It scans entries in
// the mapping that match the Scaled-PG prefix and returns max(index)+1, or 0 if no Scaled-PG
// entries exist. Returns an error if any matching entry's name fails to parse — Grove is the
// sole writer of these names in steady state, so an unparseable name is a contract violation.
func nextScaledPodGangIndex(mapping map[string]int32, scaledPGPrefix string) (int, error) {
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

// decrementPCSGMappingForScaleIn decrements the desired mapping by `count` PCSG replicas.
// BPG (the base PodGang in the legacy convention) is excluded from the walk entirely:
// decrementing it would breach the PCS-level MinAvailable invariant and cause gang termination
// of the PCS replica. The webhook ensures Spec.Replicas - count >= MinAvailable, so the walk
// has enough drainable replicas without touching BPG.
//
// Selection proceeds in three tiers:
//   - Tier 1: Legacy SPG entries (pre-upgrade scaled PodGangs, name matching legacySPGPrefix).
//     Each holds 1 replica. Sorted by trailing index descending; decrement to 0.
//   - Tier 2: New-convention Scaled-PG entries (post-upgrade steady-state mints, name matching
//     scaledPGPrefix). Each holds 1 replica. Sorted by trailing index descending; decrement to 0.
//   - Tier 3: MPG/TailPG entries (no SPG prefix, not BPG). Sorted by trailing index descending,
//     which puts TailPGs (minted last in a coherent-update iteration → highest counter) ahead
//     of MPGs. Decrement by 1 per step; an MPG with count > 1 absorbs multiple steps.
//
// Walking legacy SPGs ahead of new Scaled-PGs naturally compacts older state first across a
// Grove-upgrade boundary, leaving newer mints in the mapping.
//
// Empty entries (count == 0) are dropped by the PGM consumer via removeEmptyEntries; this
// function leaves them in the mapping with value 0.
//
// Returns an error if any entry name in the mapping fails to parse — Grove is the sole writer
// of these names so an unparseable name is a contract violation, not a soft skip.
func decrementPCSGMappingForScaleIn(desired map[string]int32, count int, scaledPGPrefix, legacySPGPrefix, bpgName string) error {
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
	// Tier 1 — legacy SPGs, each holds at most 1 replica. Decrement to 0.
	for _, name := range tierLegacySPG {
		if remaining == 0 {
			break
		}
		if desired[name] > 0 {
			desired[name]--
			remaining--
		}
	}
	// Tier 2 — new Scaled-PGs, each holds at most 1 replica. Decrement to 0.
	for _, name := range tierNewScaledPG {
		if remaining == 0 {
			break
		}
		if desired[name] > 0 {
			desired[name]--
			remaining--
		}
	}
	// Tier 3 — MPGs/TailPGs. Decrement by 1 per step; an MPG may need multiple decrements.
	for _, name := range tierMPGTail {
		for desired[name] > 0 && remaining > 0 {
			desired[name]--
			remaining--
		}
		if remaining == 0 {
			break
		}
	}
	return nil
}

// partitionPodGangNamesByTier splits the names in the mapping into three tiers for scale-in:
//   - tierLegacySPG: legacy scaled PodGangs (names matching legacySPGPrefix). Each holds 1 replica.
//   - tierNewScaledPG: new-convention Scaled-PGs (names matching scaledPGPrefix). Each holds 1 replica.
//   - tierMPGTail: everything else (MPGs, TailPGs).
//
// bpgName is excluded from all tiers — BPG is the base PodGang carrying MinAvailable replicas
// in the legacy convention and must never be decremented during scale-in.
//
// Legacy and new-convention prefixes are disjoint by name shape (the new convention has a
// `<hash>` segment between `<pcsReplicaIndex>` and `<pcsgConfigName>` that the legacy
// convention lacks), so a name matches at most one prefix.
func partitionPodGangNamesByTier(mapping map[string]int32, scaledPGPrefix, legacySPGPrefix, bpgName string) (tierLegacySPG, tierNewScaledPG, tierMPGTail []string) {
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
func (r _resource) patchPCSGPodGangMapping(sc *syncContext, desired map[string]int32) error {
	if maps.Equal(sc.pcsg.Status.PodGangMapping, desired) {
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

// buildLiveIndexToPodGang returns the live PCSG-replica-index → PodGang-name binding,
// recovered from labels on non-terminating PCLQs. Multiple PCLQs at the same PCSG replica
// index (one per CliqueName) all share the same LabelPodGang, so writes to the same key are
// no-ops on the value. Returns an error if a label is missing or unparseable — Grove writes
// these labels, so a violation indicates a corrupt resource or controller bug.
func buildLiveIndexToPodGang(pclqs []grovecorev1alpha1.PodClique) (map[int]string, error) {
	out := make(map[int]string)
	for _, pclq := range pclqs {
		if k8sutils.IsResourceTerminating(pclq.ObjectMeta) {
			continue
		}
		pgName, ok := pclq.Labels[apicommon.LabelPodGang]
		if !ok {
			return nil, fmt.Errorf("%s label on PodClique %s is missing", apicommon.LabelPodGang, pclq.Name)
		}
		idxStr, ok := pclq.Labels[apicommon.LabelPodCliqueScalingGroupReplicaIndex]
		if !ok {
			return nil, fmt.Errorf("%s label on PodClique %s is missing", apicommon.LabelPodCliqueScalingGroupReplicaIndex, pclq.Name)
		}
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			return nil, fmt.Errorf("%s label on PodClique %s is not a valid integer: %q",
				apicommon.LabelPodCliqueScalingGroupReplicaIndex, pclq.Name, idxStr)
		}
		out[idx] = pgName
	}
	return out, nil
}

// applyPCSGPerPodGangDeltas reconciles live PCLQs to the desired mapping. For each PodGang
// where live count exceeds desired count it deletes the highest-indexed PCSG replicas in
// that PodGang. For each PodGang where live count is short of desired count it creates new
// PCLQs at indices drawn from the unoccupied tail of [0, Spec.Replicas). PCLQs whose live
// PodGang is no longer present in desired are deleted entirely (handles obsolete PodGangs
// after a coherent-update rebind).
//
// The free index pool is [0, Spec.Replicas) minus indices held by live PCLQs that are NOT
// being deleted in this pass. Indices being deleted in this pass remain occupied (their
// PCLQs are still terminating) and are not reused; subsequent reconciles will pick them up
// once the underlying PCLQ objects are gone.
func (r _resource) applyPCSGPerPodGangDeltas(logger logr.Logger, sc *syncContext, desired map[string]int32) error {
	liveIndexToPG, err := buildLiveIndexToPodGang(sc.existingPCLQs)
	if err != nil {
		return groveerr.WrapError(err,
			errCodeParsePodCliqueScalingGroupReplicaIndex,
			component.OperationSync,
			fmt.Sprintf("failed to build live index→PodGang map for PodCliqueScalingGroup %v",
				client.ObjectKeyFromObject(sc.pcsg)))
	}

	deletions, creationCounts := computePCSGCountDeltas(desired, liveIndexToPG)

	kept := sets.New[int]()
	for idx := range liveIndexToPG {
		kept.Insert(idx)
	}
	for _, idx := range deletions {
		kept.Delete(idx)
	}
	assignments := assignFreeIndicesToPodGangs(creationCounts, desired, kept, int(sc.pcsg.Spec.Replicas))

	// Deletes go first so the PCSG reconciler does not race with itself in creating PCLQs at
	// indices currently held by a doomed PCLQ.
	if len(deletions) > 0 {
		if err = r.deletePCSGReplicas(logger, sc, deletions); err != nil {
			return err
		}
	}
	if len(assignments) > 0 {
		if err = r.createPCSGReplicas(logger, sc, assignments); err != nil {
			return err
		}
	}
	return nil
}

// computePCSGCountDeltas compares the desired mapping against the live index→PodGang binding
// and returns (deletions, creationCounts):
//   - deletions: PCSG replica indices whose PCLQs should be deleted. Sources:
//     1. Per-PodGang excess — for each PodGang in desired with liveCount > desiredCount,
//     the highest live indices bound to that PodGang.
//     2. Obsolete PodGangs — PCLQs whose live PodGang is no longer in desired.
//   - creationCounts: for each PodGang in desired with liveCount < desiredCount, the number
//     of PCLQs to create.
func computePCSGCountDeltas(desired map[string]int32, liveIndexToPG map[int]string) (deletionIndices []int, creationCounts map[string]int) {
	livePGCounts := make(map[string]int)
	livePGIndices := make(map[string][]int)
	for idx, pgName := range liveIndexToPG {
		livePGCounts[pgName]++
		livePGIndices[pgName] = append(livePGIndices[pgName], idx)
	}

	creationCounts = make(map[string]int)

	for pgName, desiredCount := range desired {
		liveCount := livePGCounts[pgName]
		switch {
		case liveCount > int(desiredCount):
			excess := liveCount - int(desiredCount)
			indices := slices.Clone(livePGIndices[pgName])
			sort.Sort(sort.Reverse(sort.IntSlice(indices)))
			deletionIndices = append(deletionIndices, indices[:excess]...)
		case liveCount < int(desiredCount):
			creationCounts[pgName] = int(desiredCount) - liveCount
		}
	}

	for idx, pgName := range liveIndexToPG {
		if _, ok := desired[pgName]; !ok {
			deletionIndices = append(deletionIndices, idx)
		}
	}
	return
}

// assignFreeIndicesToPodGangs assigns PCSG replica indices to PodGangs that need new PCLQs.
// Indices are drawn in ascending order from [0, specReplicas) excluding indices in `kept`
// (live PCLQs that survive this reconcile pass). PodGangs are visited in alphabetical order
// for deterministic assignment.
func assignFreeIndicesToPodGangs(creationCounts map[string]int, desired map[string]int32, preserveIndices sets.Set[int], specReplicas int) map[int]string {
	if len(creationCounts) == 0 {
		return nil
	}
	freeIndices := make([]int, 0, specReplicas)
	for i := range specReplicas {
		if !preserveIndices.Has(i) {
			freeIndices = append(freeIndices, i)
		}
	}
	assignments := make(map[int]string)
	cursor := 0
	for _, pgName := range sortedPodGangNames(desired) {
		n := creationCounts[pgName]
		for range n {
			if cursor >= len(freeIndices) {
				return assignments
			}
			assignments[freeIndices[cursor]] = pgName
			cursor++
		}
	}
	return assignments
}

// sortedPodGangNames returns a stable, alphabetically sorted list of keys.
func sortedPodGangNames(m map[string]int32) []string {
	names := lo.Keys(m)
	sort.Strings(names)
	return names
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
	for pcsgReplicaIndex, podGangName := range assignments {
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
