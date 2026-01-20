//go:build e2e

package tests

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

import (
	"context"
	"fmt"
	"testing"

	corev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"
	"github.com/ai-dynamo/grove/operator/e2e/setup"
	"github.com/ai-dynamo/grove/operator/e2e/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// deployWorkloadAndGetPods deploys workload, waits for pods to be ready, and returns the pod list
func deployWorkloadAndGetPods(tc TestContext, expectedPods int) ([]v1.Pod, error) {
	if _, err := deployAndVerifyWorkload(tc); err != nil {
		return nil, fmt.Errorf("failed to deploy workload: %w", err)
	}

	logger.Info("Wait for all pods to be scheduled and running")
	if err := utils.WaitForPods(tc.Ctx, tc.RestConfig, []string{tc.Namespace}, tc.getLabelSelector(), expectedPods, tc.Timeout, tc.Interval, logger); err != nil {
		return nil, fmt.Errorf("failed to wait for pods ready: %w", err)
	}

	logger.Info("Get all pods once for verification")
	podList, err := listPods(tc)
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	return podList.Items, nil
}

// Test_TAS1_TopologyInfrastructure verifies that the operator creates ClusterTopology and KAI Topology CRs at startup
// 1. Verify ClusterTopology CR exists with the correct 4-level hierarchy (zone, block, rack, host)
// 2. Verify KAI Topology CR exists with matching levels
// 3. Verify KAI Topology has owner reference to ClusterTopology
// 4. Verify worker nodes have topology labels
func Test_TAS1_TopologyInfrastructure(t *testing.T) {
	ctx := context.Background()

	clientset, _, dynamicClient, cleanup := prepareTestCluster(ctx, t, 0)
	defer cleanup()

	logger.Info("1. Verify ClusterTopology CR exists with correct 4-level hierarchy")

	expectedLevels := []corev1alpha1.TopologyLevel{
		{Domain: corev1alpha1.TopologyDomainZone, Key: setup.TopologyLabelZone},
		{Domain: corev1alpha1.TopologyDomainBlock, Key: setup.TopologyLabelBlock},
		{Domain: corev1alpha1.TopologyDomainRack, Key: setup.TopologyLabelRack},
		{Domain: corev1alpha1.TopologyDomainHost, Key: setup.TopologyLabelHostname},
	}

	if err := utils.VerifyClusterTopologyLevels(ctx, dynamicClient, corev1alpha1.DefaultClusterTopologyName, expectedLevels, logger); err != nil {
		t.Fatalf("Failed to verify ClusterTopology levels: %v", err)
	}

	logger.Info("2. Verify KAI Topology CR exists with matching levels and owner reference")

	expectedKeys := []string{
		setup.TopologyLabelZone,
		setup.TopologyLabelBlock,
		setup.TopologyLabelRack,
		setup.TopologyLabelHostname,
	}

	if err := utils.VerifyKAITopologyLevels(ctx, dynamicClient, corev1alpha1.DefaultClusterTopologyName, expectedKeys, logger); err != nil {
		t.Fatalf("Failed to verify KAI Topology levels: %v", err)
	}

	logger.Info("3. Verify worker nodes have topology labels")

	// Use label selector to get only worker nodes by role label
	workerLabelSelector := setup.GetWorkerNodeLabelSelector()
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: workerLabelSelector,
	})
	if err != nil {
		t.Fatalf("Failed to list nodes: %v", err)
	}

	// Reuse expectedKeys from step 2 (same topology label keys)
	workerCount := len(nodes.Items)
	for _, node := range nodes.Items {
		for _, key := range expectedKeys {
			if value, ok := node.Labels[key]; !ok || value == "" {
				t.Errorf("Node %s missing %s label", node.Name, key)
			}
		}
	}

	if workerCount == 0 {
		t.Fatal("No worker nodes found in cluster")
	}

	logger.Infof("Successfully verified topology labels on %d worker nodes", workerCount)
	logger.Info("🎉 Topology Infrastructure test completed successfully!")
}

// Test_TAS2_MultipleCliquesWithDifferentConstraints tests PCS with multiple cliques having different topology constraints
// 1. Deploy workload with PCS (no constraint) containing 2 cliques:
//   - worker-rack: packDomain=rack (3 pods)
//   - worker-block: packDomain=block (4 pods)
//
// 2. Verify all 7 pods are scheduled successfully
// 3. Verify worker-rack pods (3) are in the same rack
// 4. Verify worker-block pods (4) are in the same block
// 5. Verify different cliques can have independent topology constraints
func Test_TAS2_MultipleCliquesWithDifferentConstraints(t *testing.T) {
	ctx := context.Background()

	logger.Info("1. Initialize a 28-node Grove cluster for topology testing")
	clientset, restConfig, dynamicClient, cleanup := prepareTestCluster(ctx, t, 28)
	defer cleanup()

	expectedPods := 7 // worker-rack: 3 pods, worker-block: 4 pods
	tc := TestContext{
		T:             t,
		Ctx:           ctx,
		Clientset:     clientset,
		RestConfig:    restConfig,
		DynamicClient: dynamicClient,
		Namespace:     "default",
		Timeout:       defaultPollTimeout,
		Interval:      defaultPollInterval,
		Workload: &WorkloadConfig{
			Name:         "tas-indep-clq",
			YAMLPath:     "../yaml/tas-indep-clq.yaml",
			Namespace:    "default",
			ExpectedPods: expectedPods,
		},
	}

	logger.Info("2. Deploy workload (TAS2: multiple cliques with different constraints)")
	allPods, err := deployWorkloadAndGetPods(tc, expectedPods)
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	logger.Info("3. Verify worker-rack pods (3) are in the same rack")
	rackPods := utils.FilterPodsByLabel(allPods, "grove.io/podclique", "tas-indep-clq-0-worker-rack")
	if len(rackPods) != 3 {
		t.Fatalf("Expected 3 worker-rack pods, got %d", len(rackPods))
	}

	if err := utils.VerifyPodsInSameTopologyDomain(tc.Ctx, tc.Clientset, rackPods, setup.TopologyLabelRack, logger); err != nil {
		t.Fatalf("Failed to verify worker-rack pods in same rack: %v", err)
	}

	logger.Info("4. Verify worker-block pods (4) are in the same block")
	blockPods := utils.FilterPodsByLabel(allPods, "grove.io/podclique", "tas-indep-clq-0-worker-block")
	if len(blockPods) != 4 {
		t.Fatalf("Expected 4 worker-block pods, got %d", len(blockPods))
	}

	if err := utils.VerifyPodsInSameTopologyDomain(tc.Ctx, tc.Clientset, blockPods, setup.TopologyLabelBlock, logger); err != nil {
		t.Fatalf("Failed to verify worker-block pods in same block: %v", err)
	}

	logger.Info("5. Verify KAI PodGroup has correct SubGroups with topology constraints")
	podGroups, err := utils.WaitForKAIPodGroups(tc.Ctx, tc.DynamicClient, tc.Namespace, "tas-indep-clq", tc.Timeout, tc.Interval, logger)
	if err != nil {
		t.Fatalf("Failed to get KAI PodGroups: %v", err)
	}

	podGroup, err := utils.FilterPodGroupByOwner(podGroups, "tas-indep-clq-0")
	if err != nil {
		t.Fatalf("Failed to find PodGroup for PodGang tas-indep-clq-0: %v", err)
	}

	// Verify top-level TopologyConstraint is empty (no PCS constraint in this test)
	if err := utils.VerifyKAIPodGroupTopologyConstraint(podGroup, "", "", logger); err != nil {
		t.Fatalf("Failed to verify KAI PodGroup top-level constraint: %v", err)
	}

	// Verify SubGroups (2 standalone PCLQs - no PCSG)
	expectedSubGroups := []utils.ExpectedSubGroup{
		{
			Name:                  "tas-indep-clq-0-worker-rack",
			MinMember:             3,
			Parent:                nil,
			RequiredTopologyLevel: setup.TopologyLabelRack,
		},
		{
			Name:                  "tas-indep-clq-0-worker-block",
			MinMember:             4,
			Parent:                nil,
			RequiredTopologyLevel: setup.TopologyLabelBlock,
		},
	}
	if err := utils.VerifyKAIPodGroupSubGroups(podGroup, expectedSubGroups, logger); err != nil {
		t.Fatalf("Failed to verify KAI PodGroup SubGroups: %v", err)
	}

	logger.Info("🎉 TAS2: Multiple Cliques with Different Constraints test completed successfully!")
}
