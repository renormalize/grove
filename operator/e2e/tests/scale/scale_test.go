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
	// same test run at 10x/100x scale without editing the YAML. Pods = replicas * 2 (the
	// workload has a single clique with 2 pods per replica).
	scaleReplicasEnvVar  = "SCALE_PCS_REPLICAS"
	defaultScaleReplicas = 500
	scalePodsPerReplica  = 2
	scaleWorkloadName    = "scale-test"

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
			Name:         cfg.workload,
			YAMLPath:     cfg.yamlPath,
			Namespace:    defaultScaleNamespace,
			ExpectedPods: cfg.expectedPods,
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
// drives 1x/10x/100x runs. It intentionally excludes Kubernetes cascade-cleanup latency
// after the delete request returns.
func Test_ScaleTest(t *testing.T) {
	const expectedReplicas = 1

	replicas := envInt(scaleReplicasEnvVar, defaultScaleReplicas)
	expectedPods := replicas * scalePodsPerReplica
	workloadYAML := []byte(fmt.Sprintf(scaleWorkloadTemplate, scaleWorkloadName, replicas))

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
