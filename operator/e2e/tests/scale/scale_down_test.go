//go:build e2e

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

package scale

import (
	"context"
	"fmt"
	"testing"

	"github.com/ai-dynamo/grove/operator/e2e/grove/workload"
	"github.com/ai-dynamo/grove/operator/e2e/k8s/resources"
	"github.com/ai-dynamo/grove/operator/e2e/measurement"
	"github.com/ai-dynamo/grove/operator/e2e/measurement/condition"
	"github.com/ai-dynamo/grove/operator/e2e/testctx"
)

const (
	// scaleDownWorkerNodes is the 1x base node count for the non-tiny variants,
	// matching scaleUpWorkerNodes. ~1000 kwok pods on 30 nodes (~33 pods/node) is
	// under the default 110-pod kubelet limit. It is multiplied by scaleMultiplier()
	// so the schedulable node count tracks the scaled pod count (30->300->3000).
	scaleDownWorkerNodes = 30
)

// scaleDownVariant configures a single scale-down scenario. Each variant boots a
// PCS at the YAML-encoded initialReplicas and then patches spec.replicas to
// targetReplicas so the timeline isolates the marginal scale-down cost.
type scaleDownVariant struct {
	name            string
	workloadName    string
	yamlPath        string
	initialReplicas int
	initialPods     int
	targetReplicas  int
	targetPods      int
	// workerNodes overrides scaleDownWorkerNodes for variants that need fewer
	// nodes (e.g. the tiny sanity test). Zero means "use the default".
	workerNodes int
}

// Test_ScaleDown_Tiny is a sanity-check variant that scales from 5 replicas
// (10 pods) down to 0. It runs the same code paths as the real benchmarks but
// completes in seconds — use it to validate cluster setup and the new
// PodsAtCountCondition before running the 500/1000-pod scenarios.
func Test_ScaleDown_Tiny(t *testing.T) {
	runScaleDownTest(t, scaleDownVariant{
		name:            "ScaleDown_Tiny",
		workloadName:    "scale-down-tiny",
		yamlPath:        "../../yaml/scale-down-tiny.yaml",
		initialReplicas: 5,
		initialPods:     10,
		targetReplicas:  0,
		targetPods:      0,
		workerNodes:     5,
	})
}

// Test_ScaleDown_ToZero scales an existing 500-replica PCS (1000 pods) down to 0.
// Captures the cascade-delete-everything case where every child must be torn down.
func Test_ScaleDown_ToZero(t *testing.T) {
	runScaleDownTest(t, scaleDownVariant{
		name:            "ScaleDown_ToZero",
		workloadName:    "scale-down-to-zero",
		yamlPath:        "../../yaml/scale-down-to-zero.yaml",
		initialReplicas: 500,
		initialPods:     1000,
		targetReplicas:  0,
		targetPods:      0,
	})
}

// Test_ScaleDown scales an existing 1000-pod PCS by 0.5x (to 500 pods).
// Captures the burst-shrink case where the controller has to tear down as
// many replicas as it keeps.
func Test_ScaleDown(t *testing.T) {
	runScaleDownTest(t, scaleDownVariant{
		name:            "ScaleDown",
		workloadName:    "scale-down",
		yamlPath:        "../../yaml/scale-down.yaml",
		initialReplicas: 500,
		initialPods:     1000,
		targetReplicas:  250,
		targetPods:      500,
	})
}

// runScaleDownTest builds the deploy → scale-down → delete timeline for a variant.
// The deploy phase brings the PCS up at the initial replica count; the scale-down
// phase is the measurement of interest and milestones-out at pod-count-at-target.
//
// Non-tiny variants (workerNodes == 0) scale their replica/pod/node counts and
// timeout by scaleMultiplier() so the whole suite tracks SCALE_PCS_REPLICAS. Tiny
// variants set workerNodes explicitly and stay at their fixed 1x sizes.
func runScaleDownTest(t *testing.T, v scaleDownVariant) {
	workerNodes := v.workerNodes
	if workerNodes == 0 {
		mult := scaleMultiplier()
		workerNodes = scaleDownWorkerNodes * mult
		v.initialReplicas *= mult
		v.initialPods *= mult
		v.targetReplicas *= mult
		v.targetPods *= mult
	}
	// expectedPods is the upper bound used to size cluster fixtures; the scale-down
	// phase shrinks below it.
	runScaleTest(t, scaleTestConfig{
		name:         v.name,
		workload:     v.workloadName,
		yamlPath:     v.yamlPath,
		expectedPods: v.initialPods,
		pcsCount:     defaultScalePCSCount,
		workerNodes:  workerNodes,
		timeout:      scaleWorkloadTimeout(v.initialPods),
		pollInterval: defaultScalePollInterval,
	}, func(tracker *measurement.TimelineTracker, tc *testctx.TestContext, _ string) {
		baseline := &operatorBaseline{}
		addOperatorBaselinePhase(tracker, tc, baseline)

		// Non-tiny variants render the workload from the shared template so the initial
		// replica count reflects the scaled value; the YAML files hardcode 1x counts.
		// Tiny variants keep using their YAML file unchanged.
		deployFn := func(ctx context.Context) error {
			_, err := resources.NewResourceManager(tc.Client, Logger).ApplyYAMLFile(ctx, tc.Workload.YAMLPath, tc.Namespace)
			return err
		}
		if v.workerNodes == 0 {
			workloadYAML := []byte(fmt.Sprintf(scaleWorkloadTemplate, tc.Workload.Name, v.initialReplicas))
			deployFn = func(ctx context.Context) error {
				_, err := resources.NewResourceManager(tc.Client, Logger).ApplyYAMLData(ctx, workloadYAML, tc.Namespace)
				return err
			}
		}
		tracker.AddPhase(measurement.PhaseDefinition{
			Name:     "deploy",
			ActionFn: deployFn,
			Milestones: []measurement.MilestoneDefinition{
				{
					Name: "initial-pods-created",
					Condition: &condition.PodsCreatedCondition{
						Client:        tc.Client.Client,
						Namespace:     tc.Namespace,
						LabelSelector: tc.GetLabelSelector(),
						ExpectedCount: v.initialPods,
					},
				},
				{
					Name: "initial-pods-ready",
					Condition: &condition.PodsReadyCondition{
						Client:        tc.Client.Client,
						Namespace:     tc.Namespace,
						LabelSelector: tc.GetLabelSelector(),
						ExpectedCount: v.initialPods,
					},
				},
			},
		})

		tracker.AddPhase(measurement.PhaseDefinition{
			Name: "scale-down",
			ActionFn: func(ctx context.Context) error {
				Logger.Infof("scaling %s from %d to %d PCS replicas (target %d pods)",
					tc.Workload.Name, v.initialReplicas, v.targetReplicas, v.targetPods)
				return workload.NewWorkloadManager(tc.Client, Logger).ScalePCS(ctx, tc.Namespace, tc.Workload.Name, v.targetReplicas)
			},
			Milestones: []measurement.MilestoneDefinition{
				{
					Name: "pods-at-target",
					Condition: &condition.PodsScaledDownToCountCondition{
						Client:        tc.Client.Client,
						Namespace:     tc.Namespace,
						LabelSelector: tc.GetLabelSelector(),
						ExpectedCount: v.targetPods,
					},
				},
			},
		})

		addFinalCheckAndDeletePhases(tracker, tc, scaleFinalCheckConfig{
			targetReplicas: v.targetReplicas,
			targetPods:     v.targetPods,
			workloadName:   v.workloadName,
		}, baseline)
	})
}
