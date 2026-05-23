//go:build ignore

// /*
// Copyright 2026 The Grove Authors.
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

package update

import (
	"testing"

	tests "github.com/ai-dynamo/grove/operator/e2e/tests"
	"github.com/ai-dynamo/grove/operator/e2e/testctx"
)

const (
	coherentWorkloadName = "workload-mvu-test1"

	// Base: frontend=3, prefill=4, decode=5 → 21 pods
	coherentWorkloadYAML  = "../../yaml/workload-mvu-test1.yaml"
	coherentExpectedPods  = 21
	// Scale-out: frontend=3, prefill=6, decode=6 → 27 pods
	coherentScaleoutYAML  = "../../yaml/workload-mvu-test1-scaleout.yaml"
	coherentScaleoutPods  = 27
	// Minimal: frontend=1, prefill=2, decode=2 → 9 pods
	coherentMinimalYAML   = "../../yaml/workload-mvu-test1-minimal.yaml"
	coherentMinimalPods   = 9
	// Second scale-out: frontend=3, prefill=5, decode=5 → 23 pods
	coherentScaleout2YAML = "../../yaml/workload-mvu-test1-scaleout2.yaml"
	coherentScaleout2Pods = 23

	coherentWorkerNodes = 25
)

// mvu1Setup deploys the base workload (21 pods: frontend=3, prefill=4, decode=5).
func mvu1Setup(t *testing.T) (*testctx.TestContext, func()) {
	t.Helper()
	tc, cleanup, _ := setupTest(t, testConfig{
		workloadName: coherentWorkloadName,
		workloadYAML: coherentWorkloadYAML,
		workerNodes:  coherentWorkerNodes,
		expectedPods: coherentExpectedPods,
	})
	return tc, cleanup
}

// mvu1SetupScaleout deploys the scale-out workload (27 pods: frontend=3, prefill=6, decode=6).
// Starting state for tests that begin after the first scale-out.
func mvu1SetupScaleout(t *testing.T) (*testctx.TestContext, func()) {
	t.Helper()
	tc, cleanup, _ := setupTest(t, testConfig{
		workloadName: coherentWorkloadName,
		workloadYAML: coherentScaleoutYAML,
		workerNodes:  coherentWorkerNodes,
		expectedPods: coherentScaleoutPods,
	})
	return tc, cleanup
}

// mvu1SetupMinimal deploys the minimal workload (9 pods: frontend=1, prefill=2, decode=2).
// Starting state for tests that begin at minimal scale.
func mvu1SetupMinimal(t *testing.T) (*testctx.TestContext, func()) {
	t.Helper()
	tc, cleanup, _ := setupTest(t, testConfig{
		workloadName: coherentWorkloadName,
		workloadYAML: coherentMinimalYAML,
		workerNodes:  coherentWorkerNodes,
		expectedPods: coherentMinimalPods,
	})
	return tc, cleanup
}

// mvu1SetupScaleout2 deploys the second scale-out workload (23 pods: frontend=3, prefill=5, decode=5).
// Starting state for tests that begin after the second scale-out.
func mvu1SetupScaleout2(t *testing.T) (*testctx.TestContext, func()) {
	t.Helper()
	tc, cleanup, _ := setupTest(t, testConfig{
		workloadName: coherentWorkloadName,
		workloadYAML: coherentScaleout2YAML,
		workerNodes:  coherentWorkerNodes,
		expectedPods: coherentScaleout2Pods,
	})
	return tc, cleanup
}

// CU1 test series: PodGangMap lifecycle for workload-mvu-test1.
//
// Each test is self-contained: it deploys the workload fresh and fast-forwards to
// its own starting state using helpers from coherent_utils.go. Tests can be run
// individually or in any subset without depending on prior tests having executed.
//
// Scale-in and scale-out only occur in steady state (no coherent update in progress).
// A validating webhook blocks PCSG and PCLQ Spec.Replicas changes while a coherent
// update is in progress.
//
// PCS1 spec: frontend (standalone, 3 replicas/minAvailable=1), pleader/pworker/dleader/dworker
// (each 1 replica/minAvailable=1), PCSG prefill (4 replicas/minAvailable=2 over pleader+pworker),
// PCSG decode (5 replicas/minAvailable=2 over dleader+dworker).
// MVU template: {frontend:1, prefill:2, decode:2}
//
// All PodGangMap assertions use multiset equality on entry compositions, ignoring entry
// names and ordering (Scaled-PG and tail-PG names are not stable across reconciles).

// Test_CU1_01_InitialPGM verifies the PodGangMap immediately after deploying the workload.
//
// Starting state: fresh deploy, 21 pods.
// Asserts: PGM is {F:1,P:2,D:2}, {F:2,P:2,D:2}, {D:1}
func Test_CU1_01_InitialPGM(t *testing.T) {
	tc, cleanup := mvu1Setup(t)
	defer cleanup()

	tests.Logger.Info("CU1-01: Verify initial PodGangMap — {F:1,P:2,D:2}, {F:2,P:2,D:2}, {D:1}")
	if err := waitForPGMComposition(tc, coherentWorkloadName, 0, mvu1PGMAfterDeploy); err != nil {
		t.Fatalf("CU1-01 PGM mismatch: %v", err)
	}
}

// Test_CU1_02_ScaleOut scales out prefill 4→6 and decode 5→6 in steady state and verifies
// the PGM at each step. PCSG reconciler mints new Scaled-PG entries for each new replica.
//
// Starting state: fresh deploy, 21 pods.
// End state: 27 pods, prefill=6, decode=6.
// Final PGM: {F:1,P:2,D:2}, {F:2,P:2,D:2}, {P:1}, {P:1}, {D:1}, {D:1}
func Test_CU1_02_ScaleOut(t *testing.T) {
	const pcsName = coherentWorkloadName
	tc, cleanup := mvu1Setup(t)
	defer cleanup()

	tests.Logger.Info("CU1-02a: Scale out prefill PCSG 4 → 6 (expect 25 pods)")
	tc.ScalePCSGAcrossAllReplicasAndWait(pcsName, "prefill", 1, 6, 25, 0)
	if err := waitForPGMComposition(tc, pcsName, 0, []pgComposition{
		pg(map[string]int32{"frontend": 1}, map[string]int32{"prefill": 2, "decode": 2}),
		pg(map[string]int32{"frontend": 2}, map[string]int32{"prefill": 2, "decode": 2}),
		pg(nil, map[string]int32{"decode": 1}),
		pg(nil, map[string]int32{"prefill": 1}),
		pg(nil, map[string]int32{"prefill": 1}),
	}); err != nil {
		t.Fatalf("CU1-02a PGM mismatch: %v", err)
	}

	tests.Logger.Info("CU1-02b: Scale out decode PCSG 5 → 6 (expect 27 pods)")
	tc.ScalePCSGAcrossAllReplicasAndWait(pcsName, "decode", 1, 6, 27, 0)
	if err := waitForPGMComposition(tc, pcsName, 0, mvu1PGMAfterScaleOut); err != nil {
		t.Fatalf("CU1-02b PGM mismatch: %v", err)
	}
}

// Test_CU1_03_CoherentUpdates runs two coherent updates in succession (steady state).
// First: frontend, pleader, dleader. Second: frontend, pworker, dworker.
// After both updates all 6 prefill + 6 decode + 3 frontend form full MVU gangs.
//
// Starting state: 27 pods, prefill=6, decode=6, frontend=3.
// Final PGM: 3× {F:1,P:2,D:2}
func Test_CU1_03_CoherentUpdates(t *testing.T) {
	const pcsName = coherentWorkloadName
	tc, cleanup := mvu1SetupScaleout(t)
	defer cleanup()

	tests.Logger.Info("CU1-03a: Trigger coherent update of frontend, pleader, dleader")
	for _, clique := range []string{"frontend", "pleader", "dleader"} {
		if err := triggerPodCliqueUpdate(tc, clique); err != nil {
			t.Fatalf("Failed to trigger update on %s: %v", clique, err)
		}
	}
	if err := waitForCoherentUpdateComplete(tc, 1); err != nil {
		t.Fatalf("CU1-03a coherent update did not complete: %v", err)
	}
	if err := waitForPGMComposition(tc, pcsName, 0, mvu1PGMAfterScaleOut); err != nil {
		t.Fatalf("CU1-03a PGM mismatch (should be unchanged): %v", err)
	}

	tests.Logger.Info("CU1-03b: Trigger coherent update of frontend, pworker, dworker")
	for _, clique := range []string{"frontend", "pworker", "dworker"} {
		if err := triggerPodCliqueUpdate(tc, clique); err != nil {
			t.Fatalf("Failed to trigger update on %s: %v", clique, err)
		}
	}
	if err := waitForCoherentUpdateComplete(tc, 1); err != nil {
		t.Fatalf("CU1-03b coherent update did not complete: %v", err)
	}
	if err := waitForPGMComposition(tc, pcsName, 0, mvu1PGMAfterCoherentUpdates); err != nil {
		t.Fatalf("CU1-03b PGM mismatch: %v", err)
	}
}

// Test_CU1_04_ScaleIn cascades scale-in from 27 pods back to minimal counts (9 pods).
// All scale-in happens in steady state. Deletion follows Tier 1 (Scaled-PG entries)
// then Tier 2 (MPG/TailPG entries).
//
// Starting state: 27 pods, 3× {F:1,P:2,D:2} — reached by deploying the scale-out YAML
// and running both coherent updates.
// Final PGM: {F:1,P:2,D:2}
func Test_CU1_04_ScaleIn(t *testing.T) {
	const pcsName = coherentWorkloadName
	tc, cleanup := mvu1SetupScaleout(t)
	defer cleanup()

	tests.Logger.Info("CU1-04: Fast-forward through coherent updates (27 pods → 3× {F:1,P:2,D:2})")
	if err := reachMVU1StateAfterCoherentUpdates(tc, pcsName); err != nil {
		t.Fatalf("CU1-04 setup (coherent updates): %v", err)
	}

	tests.Logger.Info("CU1-04a: Scale-in frontend 3 → 1 (expect 25 pods)")
	if err := scalePodCliqueInPCS(tc, "frontend", 1); err != nil {
		t.Fatalf("Failed to scale frontend to 1: %v", err)
	}
	if err := tc.WaitForPods(25); err != nil {
		t.Fatalf("CU1-04a: did not reach 25 pods: %v", err)
	}
	if err := waitForPGMComposition(tc, pcsName, 0, []pgComposition{
		pg(map[string]int32{"frontend": 1}, map[string]int32{"prefill": 2, "decode": 2}),
		pg(nil, map[string]int32{"prefill": 2, "decode": 2}),
		pg(nil, map[string]int32{"prefill": 2, "decode": 2}),
	}); err != nil {
		t.Fatalf("CU1-04a PGM mismatch: %v", err)
	}

	tests.Logger.Info("CU1-04b: Scale-in prefill 6 → 4 (expect 21 pods)")
	tc.ScalePCSGAcrossAllReplicasAndWait(pcsName, "prefill", 1, 4, 21, 0)
	if err := waitForPGMComposition(tc, pcsName, 0, []pgComposition{
		pg(map[string]int32{"frontend": 1}, map[string]int32{"prefill": 2, "decode": 2}),
		pg(nil, map[string]int32{"prefill": 2, "decode": 2}),
		pg(nil, map[string]int32{"decode": 2}),
	}); err != nil {
		t.Fatalf("CU1-04b PGM mismatch: %v", err)
	}

	tests.Logger.Info("CU1-04c: Scale-in decode 6 → 4 (expect 17 pods)")
	tc.ScalePCSGAcrossAllReplicasAndWait(pcsName, "decode", 1, 4, 17, 0)
	if err := waitForPGMComposition(tc, pcsName, 0, []pgComposition{
		pg(map[string]int32{"frontend": 1}, map[string]int32{"prefill": 2, "decode": 2}),
		pg(nil, map[string]int32{"prefill": 2, "decode": 2}),
	}); err != nil {
		t.Fatalf("CU1-04c PGM mismatch: %v", err)
	}

	tests.Logger.Info("CU1-04d: Scale-in prefill 4 → 2 (expect 13 pods)")
	tc.ScalePCSGAcrossAllReplicasAndWait(pcsName, "prefill", 1, 2, 13, 0)
	if err := waitForPGMComposition(tc, pcsName, 0, []pgComposition{
		pg(map[string]int32{"frontend": 1}, map[string]int32{"prefill": 2, "decode": 2}),
		pg(nil, map[string]int32{"decode": 2}),
	}); err != nil {
		t.Fatalf("CU1-04d PGM mismatch: %v", err)
	}

	tests.Logger.Info("CU1-04e: Scale-in decode 4 → 2 (expect 9 pods)")
	tc.ScalePCSGAcrossAllReplicasAndWait(pcsName, "decode", 1, 2, 9, 0)
	if err := waitForPGMComposition(tc, pcsName, 0, mvu1PGMAfterScaleIn); err != nil {
		t.Fatalf("CU1-04e PGM mismatch: %v", err)
	}
}

// Test_CU1_05_GangTerminationRecreation exercises the gang-termination/recreation cycle
// at the minimal state. Cordoning all nodes prevents recreated pods from scheduling
// until uncordon.
//
// Starting state: 9 pods, frontend=1, prefill=2, decode=2.
// End state: 9 pods ready. PGM: {F:1,P:2,D:2}
func Test_CU1_05_GangTerminationRecreation(t *testing.T) {
	const pcsName = coherentWorkloadName
	tc, cleanup := mvu1SetupMinimal(t)
	defer cleanup()

	tests.Logger.Info("CU1-05a: Cordon all worker nodes")
	cordonedNodes := tc.SetupAndCordonNodes(coherentWorkerNodes)

	tests.Logger.Info("CU1-05b: Delete one frontend pod, await gang termination and recreation")
	if err := deleteFirstPodOfClique(tc, "frontend"); err != nil {
		t.Fatalf("Failed to delete frontend pod: %v", err)
	}
	if err := waitForGangTerminationAndRecreation(tc, 9); err != nil {
		t.Fatalf("CU1-05b gang termination/recreation failed: %v", err)
	}

	tests.Logger.Info("CU1-05c: Verify PGM after gang recreation — {F:1,P:2,D:2}")
	if err := waitForPGMComposition(tc, pcsName, 0, mvu1PGMAfterScaleIn); err != nil {
		t.Fatalf("CU1-05c PGM mismatch: %v", err)
	}

	tests.Logger.Info("CU1-05d: Uncordon all nodes, wait for 9 pods ready")
	tc.UncordonNodesAndWaitForPods(cordonedNodes, 9)
}

// Test_CU1_06_ScaleOutFromMinimal scales out from the minimal state (9 → 23 pods).
// PCLQ reconciler increments the highest-index MPG entry for frontend.
// PCSG reconciler mints new Scaled-PG entries for prefill and decode.
//
// Starting state: 9 pods, frontend=1, prefill=2, decode=2.
// Final PGM: {F:3,P:2,D:2}, {P:1}×3, {D:1}×3
func Test_CU1_06_ScaleOutFromMinimal(t *testing.T) {
	const pcsName = coherentWorkloadName
	tc, cleanup := mvu1SetupMinimal(t)
	defer cleanup()

	tests.Logger.Info("CU1-06a: Scale out frontend 1 → 3 (expect 11 pods)")
	if err := scalePodCliqueInPCS(tc, "frontend", 3); err != nil {
		t.Fatalf("Failed to scale frontend to 3: %v", err)
	}
	if err := tc.WaitForPods(11); err != nil {
		t.Fatalf("CU1-06a: did not reach 11 pods: %v", err)
	}
	if err := waitForPGMComposition(tc, pcsName, 0, []pgComposition{
		pg(map[string]int32{"frontend": 3}, map[string]int32{"prefill": 2, "decode": 2}),
	}); err != nil {
		t.Fatalf("CU1-06a PGM mismatch: %v", err)
	}

	tests.Logger.Info("CU1-06b: Scale out prefill 2 → 5 (expect 17 pods)")
	tc.ScalePCSGAcrossAllReplicasAndWait(pcsName, "prefill", 1, 5, 17, 0)
	if err := waitForPGMComposition(tc, pcsName, 0, []pgComposition{
		pg(map[string]int32{"frontend": 3}, map[string]int32{"prefill": 2, "decode": 2}),
		pg(nil, map[string]int32{"prefill": 1}),
		pg(nil, map[string]int32{"prefill": 1}),
		pg(nil, map[string]int32{"prefill": 1}),
	}); err != nil {
		t.Fatalf("CU1-06b PGM mismatch: %v", err)
	}

	tests.Logger.Info("CU1-06c: Scale out decode 2 → 5 (expect 23 pods)")
	tc.ScalePCSGAcrossAllReplicasAndWait(pcsName, "decode", 1, 5, 23, 0)
	if err := waitForPGMComposition(tc, pcsName, 0, mvu1PGMAfterSecondScaleOut); err != nil {
		t.Fatalf("CU1-06c PGM mismatch: %v", err)
	}
}

// Test_CU1_07_FrontendUpdateAndScaleOut runs a frontend-only coherent update then scales
// out prefill 5→6 and decode 5→6 in steady state.
// After the update, frontend pods move to standalone tail PGs; the PCSG-only entry
// remains as the head gang.
//
// Starting state: 23 pods, frontend=3, prefill=5, decode=5.
// Final PGM: {P:2,D:2}, {P:1}×4, {D:1}×4, {F:1}×3
func Test_CU1_07_FrontendUpdateAndScaleOut(t *testing.T) {
	const pcsName = coherentWorkloadName
	tc, cleanup := mvu1SetupScaleout2(t)
	defer cleanup()

	tests.Logger.Info("CU1-07a: Trigger coherent update of frontend")
	if err := triggerPodCliqueUpdate(tc, "frontend"); err != nil {
		t.Fatalf("Failed to trigger frontend update: %v", err)
	}
	if err := waitForCoherentUpdateComplete(tc, 1); err != nil {
		t.Fatalf("CU1-07a coherent update did not complete: %v", err)
	}
	if err := waitForPGMComposition(tc, pcsName, 0, []pgComposition{
		pg(nil, map[string]int32{"prefill": 2, "decode": 2}),
		pg(nil, map[string]int32{"prefill": 1}),
		pg(nil, map[string]int32{"prefill": 1}),
		pg(nil, map[string]int32{"prefill": 1}),
		pg(nil, map[string]int32{"decode": 1}),
		pg(nil, map[string]int32{"decode": 1}),
		pg(nil, map[string]int32{"decode": 1}),
		pg(map[string]int32{"frontend": 1}, nil),
		pg(map[string]int32{"frontend": 1}, nil),
		pg(map[string]int32{"frontend": 1}, nil),
	}); err != nil {
		t.Fatalf("CU1-07a PGM mismatch: %v", err)
	}

	tests.Logger.Info("CU1-07b: Scale out prefill 5 → 6 (expect 25 pods)")
	tc.ScalePCSGAcrossAllReplicasAndWait(pcsName, "prefill", 1, 6, 25, 0)
	if err := waitForPGMComposition(tc, pcsName, 0, []pgComposition{
		pg(nil, map[string]int32{"prefill": 2, "decode": 2}),
		pg(nil, map[string]int32{"prefill": 1}),
		pg(nil, map[string]int32{"prefill": 1}),
		pg(nil, map[string]int32{"prefill": 1}),
		pg(nil, map[string]int32{"decode": 1}),
		pg(nil, map[string]int32{"decode": 1}),
		pg(nil, map[string]int32{"decode": 1}),
		pg(map[string]int32{"frontend": 1}, nil),
		pg(map[string]int32{"frontend": 1}, nil),
		pg(map[string]int32{"frontend": 1}, nil),
		pg(nil, map[string]int32{"prefill": 1}),
	}); err != nil {
		t.Fatalf("CU1-07b PGM mismatch: %v", err)
	}

	tests.Logger.Info("CU1-07c: Scale out decode 5 → 6 (expect 27 pods)")
	tc.ScalePCSGAcrossAllReplicasAndWait(pcsName, "decode", 1, 6, 27, 0)
	if err := waitForPGMComposition(tc, pcsName, 0, mvu1PGMAfterFrontendUpdate); err != nil {
		t.Fatalf("CU1-07c PGM mismatch: %v", err)
	}
}

// Test_CU1_08_LeaderUpdate runs a coherent update of pleader and dleader.
// Prefill and decode tail PGs consolidate back into full MVU gangs.
//
// Starting state: 27 pods, {P:2,D:2}, {P:1}×4, {D:1}×4, {F:1}×3 — reached by deploying
// the second scale-out YAML and running the frontend coherent update + prefill/decode scale-out.
// Final PGM: 3× {P:2,D:2}, 3× {F:1}
func Test_CU1_08_LeaderUpdate(t *testing.T) {
	const pcsName = coherentWorkloadName
	tc, cleanup := mvu1SetupScaleout2(t)
	defer cleanup()

	tests.Logger.Info("CU1-08: Fast-forward through frontend update + scale-out (23 → 27 pods)")
	if err := reachMVU1StateAfterFrontendUpdate(tc, pcsName); err != nil {
		t.Fatalf("CU1-08 setup (frontend update): %v", err)
	}

	tests.Logger.Info("CU1-08a: Trigger coherent update of pleader, dleader")
	for _, clique := range []string{"pleader", "dleader"} {
		if err := triggerPodCliqueUpdate(tc, clique); err != nil {
			t.Fatalf("Failed to trigger update on %s: %v", clique, err)
		}
	}
	if err := waitForCoherentUpdateComplete(tc, 1); err != nil {
		t.Fatalf("CU1-08a coherent update did not complete: %v", err)
	}

	tests.Logger.Info("CU1-08b: Verify PGM — 3× {P:2,D:2}, 3× {F:1}")
	if err := waitForPGMComposition(tc, pcsName, 0, mvu1PGMAfterLeaderUpdate); err != nil {
		t.Fatalf("CU1-08b PGM mismatch: %v", err)
	}
}

// Test_CU1_09_FinalGangTerminationRecreation deletes all frontend pods, verifies gang
// termination and recreation, and confirms the PGM reconverges to full MVU entries.
//
// Starting state: 27 pods, 3× {P:2,D:2}, 3× {F:1} — reached by deploying the second
// scale-out YAML and running the frontend update + leader update.
// End state: 27 pods ready. PGM: 3× {F:1,P:2,D:2}
func Test_CU1_09_FinalGangTerminationRecreation(t *testing.T) {
	const pcsName = coherentWorkloadName
	tc, cleanup := mvu1SetupScaleout2(t)
	defer cleanup()

	tests.Logger.Info("CU1-09: Fast-forward through frontend update + leader update (23 → 27 pods)")
	if err := reachMVU1StateAfterFrontendUpdate(tc, pcsName); err != nil {
		t.Fatalf("CU1-09 setup (frontend update): %v", err)
	}
	if err := reachMVU1StateAfterLeaderUpdate(tc, pcsName); err != nil {
		t.Fatalf("CU1-09 setup (leader update): %v", err)
	}

	tests.Logger.Info("CU1-09a: Cordon all worker nodes")
	cordonedNodes := tc.SetupAndCordonNodes(coherentWorkerNodes)

	tests.Logger.Info("CU1-09b: Delete all frontend pods, await gang termination and recreation")
	if err := deleteAllPodsOfClique(tc, "frontend"); err != nil {
		t.Fatalf("Failed to delete all frontend pods: %v", err)
	}
	if err := waitForGangTerminationAndRecreation(tc, 27); err != nil {
		t.Fatalf("CU1-09b gang termination/recreation failed: %v", err)
	}

	tests.Logger.Info("CU1-09c: Verify PGM after recreation — 3× {F:1,P:2,D:2}")
	if err := waitForPGMComposition(tc, pcsName, 0, []pgComposition{
		pg(map[string]int32{"frontend": 1}, map[string]int32{"prefill": 2, "decode": 2}),
		pg(map[string]int32{"frontend": 1}, map[string]int32{"prefill": 2, "decode": 2}),
		pg(map[string]int32{"frontend": 1}, map[string]int32{"prefill": 2, "decode": 2}),
	}); err != nil {
		t.Fatalf("CU1-09c PGM mismatch: %v", err)
	}

	tests.Logger.Info("CU1-09d: Uncordon all nodes — CU1 series complete")
	tc.UncordonNodesAndWaitForPods(cordonedNodes, 27)
}
