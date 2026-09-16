//go:build e2e

package scale

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

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"k8s.io/utils/ptr"

	"github.com/ai-dynamo/grove/operator/api/common"
	"github.com/ai-dynamo/grove/operator/e2e/diagnostics"
	"github.com/ai-dynamo/grove/operator/e2e/grove/config"
	"github.com/ai-dynamo/grove/operator/e2e/grove/workload"
	"github.com/ai-dynamo/grove/operator/e2e/k8s/resources"
	"github.com/ai-dynamo/grove/operator/e2e/log"
	"github.com/ai-dynamo/grove/operator/e2e/testctx"

	"github.com/ai-dynamo/grove/operator/e2e/measurement"
	"github.com/ai-dynamo/grove/operator/e2e/measurement/condition"
	"github.com/ai-dynamo/grove/operator/e2e/measurement/exporter"
)

// toOperatorMetadata converts GroveMetadata to the measurement package type.
func toOperatorMetadata(m *config.GroveMetadata) *measurement.OperatorMetadata {
	return &measurement.OperatorMetadata{
		GroveImage: m.Image,
		K8sClient: &measurement.K8sClientConfig{
			QPS:   m.Config.ClientConnection.QPS,
			Burst: m.Config.ClientConnection.Burst,
		},
		ControllerMaxReconcile: &measurement.ControllerMaxReconcile{
			PodCliqueSet:          ptr.Deref(m.Config.Controllers.PodCliqueSet.ConcurrentSyncs, 1),
			PodCliqueScalingGroup: ptr.Deref(m.Config.Controllers.PodCliqueScalingGroup.ConcurrentSyncs, 1),
			PodClique:             ptr.Deref(m.Config.Controllers.PodClique.ConcurrentSyncs, 1),
		},
	}
}

// Logger for the scale tests.
var Logger = log.NewTestLogger(log.InfoLevel)

const (
	defaultScalePollInterval = 100 * time.Millisecond
	defaultScalePCSCount     = 1
	defaultScaleWorkerNodes  = 100
	defaultScaleNamespace    = "default"

	runIDTimeFormat   = "20060102-150405"
	outputResultsFile = "scale-test-results.json"

	// steadyStateWindow keeps the pprof/measurement window open after a no-op reconcile
	// trigger so the full ~500-PodClique spec-hash-short-circuit burst has time to run.
	steadyStateWindow = 30 * time.Second

	// scaleReplicasEnvVar overrides the PCS replica count for Test_ScaleTest, letting the
	// same test run at 10x/50x/100x scale without editing the YAML. Pods = replicas * 2 (the
	// workload has a single clique with 2 pods per replica).
	scaleReplicasEnvVar  = "SCALE_PCS_REPLICAS"
	defaultScaleReplicas = 500
	scalePodsPerReplica  = 2
	scaleWorkloadName    = "scale-test"

	// scaleWorkloadEnvVar selects the workload topology. "flat" (default) keeps the
	// historical single standalone clique so results stay comparable to prior runs;
	// "disagg" renders a realistic disaggregated inference deployment (prefill + decode
	// PodCliqueScalingGroups, decode-heavy 1:2, tensor-parallel-sized cliques). Both
	// shapes total 1000*scaleMultiplier() pods so they are comparable to each other.
	scaleWorkloadEnvVar = "SCALE_WORKLOAD"
	scaleShapeFlat      = "flat"
	scaleShapeDisagg    = "disagg"

	// scalePCSCountEnvVar sets how many separate PodCliqueSet objects the workload is
	// spread across (default 1). It is orthogonal to SCALE_WORKLOAD: the total pod count
	// stays 1000*scaleMultiplier() regardless of N, so "one wide PCS" and "N smaller PCS"
	// are directly comparable. N>1 stresses the operator's per-PCS overhead (informer
	// keys, owner-reference trees, status subresources) rather than replica width.
	scalePCSCountEnvVar = "SCALE_PCS_COUNT"
	defaultScalePCSes   = 1

	// scaleBaseTimeout is the floor for the deploy→delete run; extra time is added
	// proportional to pod count so large runs don't time out mid-deploy.
	scaleBaseTimeout       = 10 * time.Minute
	scaleTimeoutPerKiloPod = 5 * time.Minute
)

// scaleWorkloadTemplate renders a single-clique PodCliqueSet at the requested replica
// count. Mirrors e2e/yaml/scale-test-1000.yaml (2 expert-worker pods per replica) so the
// scheduling/affinity/toleration surface is identical across scales.
const scaleWorkloadTemplate = `apiVersion: grove.io/v1alpha1
kind: PodCliqueSet
metadata:
  name: %[1]s
  labels:
    app: %[1]s
spec:
  replicas: %[2]d
  template:
    cliques:
      - name: expert-worker
        spec:
          roleName: expert
          replicas: 2
          minAvailable: 2
          podSpec:
            schedulerName: default-scheduler
            affinity:
              nodeAffinity:
                requiredDuringSchedulingIgnoredDuringExecution:
                  nodeSelectorTerms:
                    - matchExpressions:
                        - key: type
                          operator: In
                          values:
                            - kwok
            tolerations:
              - key: node_role.e2e.grove.nvidia.com
                operator: Equal
                value: agent
                effect: NoSchedule
            containers:
              - name: expert-worker
                image: registry:5001/nginx:alpine-slim
                resources:
                  requests:
                    memory: 1Mi
`

// scaleWorkloadTimeout returns a run timeout that grows with pod count.
func scaleWorkloadTimeout(pods int) time.Duration {
	return scaleBaseTimeout + time.Duration(pods/1000)*scaleTimeoutPerKiloPod
}

// disaggWorkloadTemplate renders a realistic disaggregated inference deployment: each PCS
// replica is one model-serving instance split into a prefill and a decode
// PodCliqueScalingGroup. Decode is the heavier pool (1:2 prefill:decode groups), and each
// group is tensor-parallel-sized (prefill TP=4 pods, decode TP=8 pods) — mirroring how
// NVIDIA Dynamo / vLLM disaggregated serving scale prefill and decode independently.
//
// Format args: %[1]s workload name, %[2]d PCS replicas, %[3]d prefill PCSG replicas,
// %[4]d decode PCSG replicas.
const disaggWorkloadTemplate = `apiVersion: grove.io/v1alpha1
kind: PodCliqueSet
metadata:
  name: %[1]s
  labels:
    app: %[1]s
spec:
  replicas: %[2]d
  template:
    podCliqueScalingGroups:
      - name: prefill
        replicas: %[3]d
        minAvailable: 1
        cliqueNames:
          - prefill-worker
      - name: decode
        replicas: %[4]d
        minAvailable: 1
        cliqueNames:
          - decode-worker
    cliques:
      - name: prefill-worker
        spec:
          roleName: prefill
          replicas: 4
          minAvailable: 4
          podSpec:
            schedulerName: default-scheduler
            affinity:
              nodeAffinity:
                requiredDuringSchedulingIgnoredDuringExecution:
                  nodeSelectorTerms:
                    - matchExpressions:
                        - key: type
                          operator: In
                          values:
                            - kwok
            tolerations:
              - key: node_role.e2e.grove.nvidia.com
                operator: Equal
                value: agent
                effect: NoSchedule
            containers:
              - name: prefill-worker
                image: registry:5001/nginx:alpine-slim
                resources:
                  requests:
                    memory: 1Mi
      - name: decode-worker
        spec:
          roleName: decode
          replicas: 8
          minAvailable: 8
          podSpec:
            schedulerName: default-scheduler
            affinity:
              nodeAffinity:
                requiredDuringSchedulingIgnoredDuringExecution:
                  nodeSelectorTerms:
                    - matchExpressions:
                        - key: type
                          operator: In
                          values:
                            - kwok
            tolerations:
              - key: node_role.e2e.grove.nvidia.com
                operator: Equal
                value: agent
                effect: NoSchedule
            containers:
              - name: decode-worker
                image: registry:5001/nginx:alpine-slim
                resources:
                  requests:
                    memory: 1Mi
`

const (
	// disaggPrefillTP / disaggDecodeTP are the tensor-parallel pod counts per worker group
	// (the PodClique replicas). Fixed across scales — TP size is a model property, not a
	// scaling knob; scale grows the number of deployments (PCS) and groups (PCSG) instead.
	disaggPrefillTP = 4
	disaggDecodeTP  = 8
)

// disaggTier is the (PCS, prefill-PCSG, decode-PCSG) shape for one canonical scale tier.
// Both PCS and PCSG counts grow across tiers while the 1:2 prefill:decode ratio and the
// TP sizes stay fixed. Each tier's total pod count is exactly 1000 * mult:
//
//	pods = PCS * (prefillPCSG*disaggPrefillTP + decodePCSG*disaggDecodeTP)
//	1x  : 50  * (1*4  + 2*8)  = 50  * 20  = 1,000
//	10x : 100 * (5*4  + 10*8) = 100 * 100 = 10,000
//	50x : 250 * (10*4 + 20*8) = 250 * 200 = 50,000
//	100x: 500 * (10*4 + 20*8) = 500 * 200 = 100,000
type disaggTier struct {
	pcsReplicas     int
	prefillPCSGReps int
	decodePCSGReps  int
}

// disaggTiers maps scaleMultiplier() (1/10/50/100) to its shape. Non-canonical multipliers
// (arbitrary SCALE_PCS_REPLICAS overrides) fall back to scaling PCS from the 1x base so
// any input still yields an exact 1000*mult total; see resolveDisaggTier.
var disaggTiers = map[int]disaggTier{
	1:   {pcsReplicas: 50, prefillPCSGReps: 1, decodePCSGReps: 2},
	10:  {pcsReplicas: 100, prefillPCSGReps: 5, decodePCSGReps: 10},
	50:  {pcsReplicas: 250, prefillPCSGReps: 10, decodePCSGReps: 20},
	100: {pcsReplicas: 500, prefillPCSGReps: 10, decodePCSGReps: 20},
}

// resolveDisaggTier returns the disagg shape for the given multiplier. Canonical tiers
// (1/10/50/100) use the hand-tuned table that grows both PCS and PCSG; any other multiplier
// keeps the 1x PCSG ratio and carries all growth on PCS replicas, preserving the
// 1000*mult total.
func resolveDisaggTier(mult int) disaggTier {
	if t, ok := disaggTiers[mult]; ok {
		return t
	}
	base := disaggTiers[1]
	return disaggTier{
		pcsReplicas:     base.pcsReplicas * mult,
		prefillPCSGReps: base.prefillPCSGReps,
		decodePCSGReps:  base.decodePCSGReps,
	}
}

// podsPerPCSReplica returns the pod count contributed by a single PCS replica for this tier.
func (t disaggTier) podsPerPCSReplica() int {
	return t.prefillPCSGReps*disaggPrefillTP + t.decodePCSGReps*disaggDecodeTP
}

// totalPods returns the total pod count across all PCS replicas for this tier.
func (t disaggTier) totalPods() int {
	return t.pcsReplicas * t.podsPerPCSReplica()
}

// renderedPCS is one PodCliqueSet object to deploy: its name, rendered YAML, and the
// number of pods it contributes when fully scheduled.
type renderedPCS struct {
	name string
	yaml []byte
	pods int
}

// scaleShape is the resolved workload for a run: the rendered PodCliqueSet objects plus
// the derived counts the harness needs. A single-PCS run (SCALE_PCS_COUNT=1) has one
// entry in pcsDocs; a multi-PCS run has scalePCSCount() entries whose per-PCS sizes sum to
// totalPods. totalPods and the flat/disagg shape are unchanged by the PCS count.
type scaleShape struct {
	name          string        // "flat" or "disagg"
	pcsDocs       []renderedPCS // one per PodCliqueSet object (len == scalePCSCount())
	totalPods     int
	scaleGroup    string // PCSG name the scale action targets (disagg only; "" for flat)
	scaleGroupCap int    // decode PCSG replicas (disagg scale-up target); 0 for flat
}

// pcsNames returns the names of every PodCliqueSet in the shape, in deploy order.
func (s scaleShape) pcsNames() []string {
	names := make([]string, len(s.pcsDocs))
	for i, d := range s.pcsDocs {
		names[i] = d.name
	}
	return names
}

// renderPCSDocs renders count PodCliqueSet objects for the current SCALE_WORKLOAD shape
// whose per-PCS sizes divide totalPods as evenly as possible (remainder on the last), so
// the objects together total totalPods. Each object's name is workloadName for count==1,
// else "<workloadName>-<i>". Shared by the deploy-N-PCS path (Test_ScaleTest) and the
// grow/shrink-PCS-count path (scale up/down): growing the PCS count means deploying more
// of these objects, not scaling any object's spec.replicas.
func renderPCSDocs(workloadName string, count, totalPods int) []renderedPCS {
	docs := make([]renderedPCS, count)
	name := func(i int) string {
		if count == 1 {
			return workloadName
		}
		return pcsSetName(workloadName, i)
	}
	if os.Getenv(scaleWorkloadEnvVar) == scaleShapeDisagg {
		// Split by PCS replicas so every object is a full prefill/decode deployment at the
		// tier's PCSG/TP shape; podsPerPCSReplica maps a replica count back to a pod budget.
		tier := resolveDisaggTier(scaleMultiplier())
		for i, reps := range splitAcross(totalPods/tier.podsPerPCSReplica(), count) {
			docs[i] = renderedPCS{
				name: name(i),
				yaml: []byte(fmt.Sprintf(disaggWorkloadTemplate, name(i), reps, tier.prefillPCSGReps, tier.decodePCSGReps)),
				pods: reps * tier.podsPerPCSReplica(),
			}
		}
		return docs
	}
	for i, reps := range splitAcross(totalPods/scalePodsPerReplica, count) {
		docs[i] = renderedPCS{
			name: name(i),
			yaml: []byte(fmt.Sprintf(scaleWorkloadTemplate, name(i), reps)),
			pods: reps * scalePodsPerReplica,
		}
	}
	return docs
}

// multiPCSScalePlan splits a workload's N PodCliqueSet objects into an initially-deployed
// prefix and a remainder added (scale-up) or removed (scale-down) during the measured
// phase. The scale action grows/shrinks the *number* of PCS objects, not any object's
// replicas. initialPods/finalPods are exact sums of the split (which puts any remainder on
// the last object), so milestones assert the true count regardless of divisibility.
type multiPCSScalePlan struct {
	initialDocs []renderedPCS // deployed up front
	scaleDocs   []renderedPCS // added on scale-up / removed on scale-down
	initialPods int           // pods from initialDocs
	finalPods   int           // pods from all docs
}

// planMultiPCSScale renders totalPods worth of PodCliqueSets across scalePCSCount() objects
// and splits them so ~half the objects deploy first and the rest are the scale delta. At
// least one object is always in each partition (guaranteed since this runs only for
// count>1).
func planMultiPCSScale(workloadName string, totalPods int) multiPCSScalePlan {
	docs := renderPCSDocs(workloadName, scalePCSCount(), totalPods)
	initial := len(docs) / 2
	if initial < 1 {
		initial = 1
	}
	plan := multiPCSScalePlan{initialDocs: docs[:initial], scaleDocs: docs[initial:]}
	for _, d := range docs {
		plan.finalPods += d.pods
	}
	for _, d := range plan.initialDocs {
		plan.initialPods += d.pods
	}
	return plan
}

// allDocs returns every PodCliqueSet in the plan (initial + scale), in deploy order.
func (p multiPCSScalePlan) allDocs() []renderedPCS {
	return append(append([]renderedPCS{}, p.initialDocs...), p.scaleDocs...)
}

// names returns the names of every PodCliqueSet in the plan.
func (p multiPCSScalePlan) names() []string {
	all := p.allDocs()
	names := make([]string, len(all))
	for i, d := range all {
		names[i] = d.name
	}
	return names
}

// scaleWorkloadShape reads SCALE_WORKLOAD and SCALE_PCS_COUNT and renders the selected
// topology at the current scaleMultiplier(), spread across scalePCSCount() PodCliqueSet
// objects. flat keeps the single standalone clique (pods = replicas*2); disagg renders the
// prefill/decode deployment from the tier table. The total is 1000*mult pods regardless of
// how many PCS objects it is split across, so all runs stay comparable. A count of 1 yields
// a single PCS named workloadName, identical to prior behavior.
func scaleWorkloadShape(workloadName string) scaleShape {
	count := scalePCSCount()
	if os.Getenv(scaleWorkloadEnvVar) == scaleShapeDisagg {
		tier := resolveDisaggTier(scaleMultiplier())
		return scaleShape{
			name:          scaleShapeDisagg,
			pcsDocs:       renderPCSDocs(workloadName, count, tier.totalPods()),
			totalPods:     tier.totalPods(),
			scaleGroup:    "decode",
			scaleGroupCap: tier.decodePCSGReps,
		}
	}
	totalPods := envInt(scaleReplicasEnvVar, defaultScaleReplicas) * scalePodsPerReplica
	return scaleShape{
		name:      scaleShapeFlat,
		pcsDocs:   renderPCSDocs(workloadName, count, totalPods),
		totalPods: totalPods,
	}
}

// isDisaggShape reports whether SCALE_WORKLOAD selects the disaggregated topology.
func isDisaggShape() bool { return os.Getenv(scaleWorkloadEnvVar) == scaleShapeDisagg }

// disaggScalePlan is the two-phase scale-up/down plan for the disagg shape. Scaling grows
// (or shrinks) both dimensions ~2x: first the PCS replica count (add/remove whole model
// deployments), then the decode PCSG replicas within every PCS replica (widen/narrow the
// decode pool). The initial state is half the tier on each dimension; the final state is
// the full tier, so the final pod count equals the tier total (1000*mult).
//
// The prefill PCSG count is fixed across the plan (prefill is the lighter, less elastic
// pool). Pod counts at each step (1x tier example, 50 PCS / prefill 1 / decode 2):
//
//	initial : 25 PCS, decode 1 -> 25*(1*4 + 1*8) = 300
//	afterPCS: 50 PCS, decode 1 -> 50*(1*4 + 1*8) = 600
//	final   : 50 PCS, decode 2 -> 50*(1*4 + 2*8) = 1000
type disaggScalePlan struct {
	prefillPCSGReps int
	initialPCS      int
	targetPCS       int
	initialDecode   int
	targetDecode    int
}

// disaggWorkloadName is the PCS name used by the disagg scale-up/down variants (and the
// prefix for their per-replica PCSG names).
const disaggWorkloadName = "scale-disagg"

// resolveDisaggScalePlan derives the two-phase plan from the current tier. The tier's PCS
// and decode counts are the targets; initial values are half (both must be even, which the
// canonical tiers and the PCS-only fallback guarantee).
func resolveDisaggScalePlan() disaggScalePlan {
	tier := resolveDisaggTier(scaleMultiplier())
	return disaggScalePlan{
		prefillPCSGReps: tier.prefillPCSGReps,
		initialPCS:      tier.pcsReplicas / 2,
		targetPCS:       tier.pcsReplicas,
		initialDecode:   tier.decodePCSGReps / 2,
		targetDecode:    tier.decodePCSGReps,
	}
}

// pods returns the total pod count for a given PCS replica and decode-PCSG count under this
// plan (prefill PCSG count is fixed).
func (p disaggScalePlan) pods(pcs, decode int) int {
	return pcs * (p.prefillPCSGReps*disaggPrefillTP + decode*disaggDecodeTP)
}

func (p disaggScalePlan) initialPods() int  { return p.pods(p.initialPCS, p.initialDecode) }
func (p disaggScalePlan) afterPCSPods() int { return p.pods(p.targetPCS, p.initialDecode) }
func (p disaggScalePlan) finalPods() int    { return p.pods(p.targetPCS, p.targetDecode) }

// yaml renders the disagg PodCliqueSet at the plan's initial PCS/decode counts.
func (p disaggScalePlan) yaml(workloadName string) []byte {
	return []byte(fmt.Sprintf(disaggWorkloadTemplate, workloadName, p.initialPCS, p.prefillPCSGReps, p.initialDecode))
}

// scaleTestConfig parameterizes a single scale test run.
type scaleTestConfig struct {
	name         string
	workload     string
	yamlPath     string
	expectedPods int
	pcsCount     int
	workerNodes  int
	timeout      time.Duration
	pollInterval time.Duration
	// labelSelectorOverride counts pods across a multi-PCS workload (several PodCliqueSets
	// with distinct names) via a shared label. Empty keeps the per-name part-of selector.
	labelSelectorOverride string
}

// scaleTestPhases registers test-specific phases on the tracker. Implementations
// receive the prepared TestContext and the runID (for unique trigger keys) and add
// deploy/delete/etc phases via tracker.AddPhase.
type scaleTestPhases func(tracker *measurement.TimelineTracker, tc *testctx.TestContext, runID string)

// runScaleTest provides the shared scaffolding (cluster setup, tracker, pprof,
// output dir, result export) used by every scale test in this package.
func runScaleTest(t *testing.T, cfg scaleTestConfig, addPhases scaleTestPhases) {
	diagDir := os.Getenv(diagnostics.DirEnvVar)
	Logger.Infof("starting scale test %s: %d expected pods, timeout %v", cfg.name, cfg.expectedPods, cfg.timeout)

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	Logger.Infof("preparing test cluster with %d worker nodes", cfg.workerNodes)
	tc, cleanup := testctx.PrepareTest(ctx, t, cfg.workerNodes,
		testctx.WithTimeout(cfg.timeout),
		testctx.WithInterval(cfg.pollInterval),
		testctx.WithWorkload(&testctx.WorkloadConfig{
			Name:                  cfg.workload,
			YAMLPath:              cfg.yamlPath,
			Namespace:             defaultScaleNamespace,
			ExpectedPods:          cfg.expectedPods,
			LabelSelectorOverride: cfg.labelSelectorOverride,
		}),
		testctx.WithSkipCleanupWait(),
	)
	defer cleanup()

	metadata, err := config.NewOperatorConfig(tc.Client).ReadGroveMetadata(ctx)
	if err != nil {
		t.Fatalf("failed to read grove metadata: %v", err)
	}

	runID := fmt.Sprintf("run-%s", time.Now().Format(runIDTimeFormat))
	Logger.Infof("test config: runID=%s, namespace=%s, pcsName=%s", runID, tc.Namespace, tc.Workload.Name)

	outputDir := filepath.Join(cfg.name, runID)
	if diagDir != "" {
		outputDir = filepath.Join(diagDir, cfg.name, runID)
	}
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		t.Fatalf("failed to create output directory: %v", err)
	}

	pprofOpt, pprofCleanup := setupPprofHook(ctx, tc.Client, runID, outputDir, loadPyroscopeConfig())
	defer pprofCleanup()

	opts := []measurement.TimelineOption{
		measurement.WithPollInterval(cfg.pollInterval),
		measurement.WithLogger(Logger.GetLogr()),
	}
	if pprofOpt != nil {
		opts = append(opts, pprofOpt)
	}

	tracker := measurement.NewTimelineTracker(
		cfg.name,
		runID,
		tc.Namespace,
		cfg.pcsCount,
		opts...,
	)

	addPhases(tracker, tc, runID)

	Logger.Info("running timeline tracker")
	result, err := tracker.Run(ctx, toOperatorMetadata(metadata))
	if err != nil {
		t.Fatalf("Timeline tracker run failed: %v", err)
	}
	tracker.Wait()

	Logger.Info("exporting results")
	exportResult(t, result, outputDir)
	Logger.Infof("scale test completed successfully in %.1fs", result.TestDurationSeconds)
}

// Test_ScaleTest validates deploy, steady-state reconcile, and the user-facing delete
// request latency of a PodCliqueSet. The replica count (and thus pod count) is controlled
// by the SCALE_PCS_REPLICAS env var (default 500 replicas = 1000 pods), so the same test
// drives 1x/10x/50x/100x runs. SCALE_PCS_COUNT>1 spreads the same total pod count across N
// separate PodCliqueSet objects instead of one, stressing per-PCS overhead; see
// runMultiPCSScaleTest. It intentionally excludes Kubernetes cascade-cleanup latency after
// the delete request returns.
func Test_ScaleTest(t *testing.T) {
	shape := scaleWorkloadShape(scaleWorkloadName)
	Logger.Infof("scale test workload shape=%s: %d PCS object(s), %d pods total", shape.name, len(shape.pcsDocs), shape.totalPods)

	if isMultiPCS() {
		runMultiPCSScaleTest(t, shape)
		return
	}

	const expectedReplicas = 1
	expectedPods := shape.totalPods
	workloadYAML := shape.pcsDocs[0].yaml

	runScaleTest(t, scaleTestConfig{
		name:         "ScaleTest",
		workload:     scaleWorkloadName,
		yamlPath:     "", // templated in-line; see deploy ActionFn below
		expectedPods: expectedPods,
		pcsCount:     defaultScalePCSCount,
		workerNodes:  defaultScaleWorkerNodes,
		timeout:      scaleWorkloadTimeout(expectedPods),
		pollInterval: defaultScalePollInterval,
	}, func(tracker *measurement.TimelineTracker, tc *testctx.TestContext, runID string) {
		tracker.AddPhase(measurement.PhaseDefinition{
			Name: "deploy",
			ActionFn: func(ctx context.Context) error {
				_, err := resources.NewResourceManager(tc.Client, Logger).ApplyYAMLData(ctx, workloadYAML, tc.Namespace)
				return err
			},
			Milestones: []measurement.MilestoneDefinition{
				{
					Name: "pods-created",
					Condition: &condition.PodsCreatedCondition{
						Client:        tc.Client.Client,
						Namespace:     tc.Namespace,
						LabelSelector: tc.GetLabelSelector(),
						ExpectedCount: expectedPods,
					},
				},
				{
					Name: "pods-ready",
					Condition: &condition.PodsReadyCondition{
						Client:        tc.Client.Client,
						Namespace:     tc.Namespace,
						LabelSelector: tc.GetLabelSelector(),
						ExpectedCount: expectedPods,
					},
				},
				{
					Name: "pcs-available",
					Condition: &condition.PCSAvailableCondition{
						Client:        tc.Client.Client,
						Name:          tc.Workload.Name,
						Namespace:     tc.Namespace,
						ExpectedCount: expectedReplicas,
					},
				},
			},
		})

		// steady-state-reconcile: patch a metadata annotation to force one reconcile cycle
		// without touching spec. With the spec-hash short-circuit in place, the PCS→PodClique
		// update path should fire cache hits for every PodClique. pprof captured during this
		// window isolates the no-op reconcile cost.
		steadyStateTriggerID := fmt.Sprintf("steady-%s", runID)
		tracker.AddPhase(measurement.PhaseDefinition{
			Name: "steady-state-reconcile",
			ActionFn: func(ctx context.Context) error {
				Logger.Info("triggering no-op PCS reconcile")
				return workload.NewWorkloadManager(tc.Client, Logger).TriggerPCSReconcile(ctx, tc.Namespace, tc.Workload.Name, steadyStateTriggerID)
			},
			Milestones: []measurement.MilestoneDefinition{
				{
					Name: "pcs-still-available",
					Condition: &condition.PCSAvailableCondition{
						Client:        tc.Client.Client,
						Name:          tc.Workload.Name,
						Namespace:     tc.Namespace,
						ExpectedCount: expectedReplicas,
					},
				},
				{
					Name:      "steady-state-window",
					Condition: &condition.TimerCondition{Duration: steadyStateWindow},
				},
			},
		})

		tracker.AddPhase(measurement.PhaseDefinition{
			Name: "delete",
			ActionFn: func(ctx context.Context) error {
				return workload.NewWorkloadManager(tc.Client, Logger).DeletePCS(ctx, tc.Namespace, tc.Workload.Name)
			},
			Milestones: []measurement.MilestoneDefinition{
				{
					Name: "pcs-deleted",
					Condition: &condition.PCSDeletedCondition{
						Client:    tc.Client.Client,
						Name:      tc.Workload.Name,
						Namespace: tc.Namespace,
					},
				},
			},
		})
	})
}

// runMultiPCSScaleTest is the SCALE_PCS_COUNT>1 counterpart to Test_ScaleTest. It reaches
// the same total pod count by deploying N separate PodCliqueSet objects, then runs the
// same three phases — deploy, steady-state reconcile, delete — fanned out across all N.
// Pods are counted via the managed-by selector (the scale namespace is single-tenant, so
// this spans exactly the N PCS under test); PCS-level per-name conditions are replaced by
// topology-agnostic pod-count milestones since no single PCS name covers the workload.
func runMultiPCSScaleTest(t *testing.T, shape scaleShape) {
	expectedPods := shape.totalPods
	names := shape.pcsNames()

	runScaleTest(t, scaleTestConfig{
		name:                  "ScaleTest",
		workload:              scaleWorkloadName,
		yamlPath:              "", // templated in-line
		expectedPods:          expectedPods,
		pcsCount:              len(names),
		workerNodes:           defaultScaleWorkerNodes,
		timeout:               scaleWorkloadTimeout(expectedPods),
		pollInterval:          defaultScalePollInterval,
		labelSelectorOverride: managedByLabelSelector(),
	}, func(tracker *measurement.TimelineTracker, tc *testctx.TestContext, runID string) {
		rm := resources.NewResourceManager(tc.Client, Logger)
		wm := workload.NewWorkloadManager(tc.Client, Logger)

		tracker.AddPhase(measurement.PhaseDefinition{
			Name: "deploy",
			ActionFn: func(ctx context.Context) error {
				for _, doc := range shape.pcsDocs {
					if _, err := rm.ApplyYAMLData(ctx, doc.yaml, tc.Namespace); err != nil {
						return fmt.Errorf("applying PCS %s: %w", doc.name, err)
					}
				}
				return nil
			},
			Milestones: podsReadyMilestones(tc, expectedPods),
		})

		// steady-state-reconcile: bump the reconcile-trigger annotation on every PCS so all N
		// run one no-op reconcile cycle; the window keeps pprof open to capture their cost.
		steadyStateTriggerID := fmt.Sprintf("steady-%s", runID)
		tracker.AddPhase(measurement.PhaseDefinition{
			Name: "steady-state-reconcile",
			ActionFn: func(ctx context.Context) error {
				Logger.Infof("triggering no-op reconcile on %d PCS objects", len(names))
				for _, name := range names {
					if err := wm.TriggerPCSReconcile(ctx, tc.Namespace, name, steadyStateTriggerID); err != nil {
						return fmt.Errorf("triggering reconcile on PCS %s: %w", name, err)
					}
				}
				return nil
			},
			Milestones: []measurement.MilestoneDefinition{
				{
					Name:      "steady-state-window",
					Condition: &condition.TimerCondition{Duration: steadyStateWindow},
				},
			},
		})

		tracker.AddPhase(measurement.PhaseDefinition{
			Name: "delete",
			ActionFn: func(ctx context.Context) error {
				for _, name := range names {
					if err := wm.DeletePCS(ctx, tc.Namespace, name); err != nil {
						return err
					}
				}
				return nil
			},
			Milestones: []measurement.MilestoneDefinition{
				{
					Name: "pods-deleted",
					Condition: &condition.PodsScaledDownToCountCondition{
						Client:        tc.Client.Client,
						Namespace:     tc.Namespace,
						LabelSelector: tc.GetLabelSelector(),
						ExpectedCount: 0,
					},
				},
			},
		})
	})
}

func exportResult(t *testing.T, result *measurement.TrackerResult, outputDir string) {
	t.Helper()

	outputPath := filepath.Join(outputDir, outputResultsFile)
	jsonFile, err := os.Create(outputPath)
	if err != nil {
		t.Fatalf("Failed to create JSON output file: %v", err)
	}
	defer jsonFile.Close()

	multi := exporter.NewMultiExporter(
		exporter.NewSummaryExporter(os.Stdout),
		exporter.NewJSONExporter(jsonFile),
	)

	if err := multi.Export(result); err != nil {
		t.Fatalf("Failed to export results: %v", err)
	}
}

// scaleMultiplier returns SCALE_PCS_REPLICAS/defaultScaleReplicas (min 1) so the whole
// suite scales off the same knob as Test_ScaleTest: 500->1, 5000->10, 50000->100.
func scaleMultiplier() int {
	m := envInt(scaleReplicasEnvVar, defaultScaleReplicas) / defaultScaleReplicas
	if m < 1 {
		m = 1
	}
	return m
}

// envInt reads a positive integer from an env var, falling back to def when unset,
// unparseable, or non-positive.
func envInt(key string, def int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// scalePCSCount returns SCALE_PCS_COUNT (min 1): the number of separate PodCliqueSet
// objects the workload is spread across. 1 preserves the historical single-PCS behavior.
func scalePCSCount() int {
	return envInt(scalePCSCountEnvVar, defaultScalePCSes)
}

// isMultiPCS reports whether the workload spans more than one PodCliqueSet object.
func isMultiPCS() bool { return scalePCSCount() > 1 }

// pcsSetName returns the name of the i-th PodCliqueSet in a multi-PCS set: "<base>-<i>".
// The suffix keeps each PCS (and its part-of label) distinct while sharing the base.
func pcsSetName(base string, i int) string { return fmt.Sprintf("%s-%d", base, i) }

// managedByLabelSelector selects every grove-managed pod in the namespace, spanning all
// PodCliqueSets in a multi-PCS run. The scale namespace holds only the workload under
// test (single-tenant), so this is an exact per-run pod count across N PCS objects.
func managedByLabelSelector() string {
	return fmt.Sprintf("%s=%s", common.LabelManagedByKey, common.LabelManagedByValue)
}

// splitAcross partitions total into n parts as evenly as possible, giving the remainder
// to the last part so the parts always sum to total. Used to divide a workload's replicas
// (flat) or PCS replicas (disagg) across n PodCliqueSet objects while keeping the total
// pod count exact. Panics on n<1 (callers guarantee n>=1 via scalePCSCount).
func splitAcross(total, n int) []int {
	parts := make([]int, n)
	base := total / n
	for i := range parts {
		parts[i] = base
	}
	parts[n-1] += total - base*n
	return parts
}
