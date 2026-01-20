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

package setup

import (
	"context"
	"fmt"
	"sort"

	"github.com/ai-dynamo/grove/operator/e2e/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// WorkerNodeLabelKey is the label key used to identify worker nodes in e2e tests.
	// This can be changed if infrastructure changes.
	WorkerNodeLabelKey = "node_role.e2e.grove.nvidia.com"
	// WorkerNodeLabelValue is the label value for worker node identification in e2e tests.
	WorkerNodeLabelValue = "agent"

	// TopologyLabelZone is the Kubernetes label key for zone topology domain.
	TopologyLabelZone = "kubernetes.io/zone"
	// TopologyLabelBlock is the Kubernetes label key for the block topology domain.
	TopologyLabelBlock = "kubernetes.io/block"
	// TopologyLabelRack is the Kubernetes label key for the rack topology domain.
	TopologyLabelRack = "kubernetes.io/rack"
	// TopologyLabelHostname is the Kubernetes label key for the hostname topology domain.
	TopologyLabelHostname = "kubernetes.io/hostname"

	// NodesPerZone is the number of nodes per zone.
	NodesPerZone = 28
	// NodesPerBlock is the number of nodes per block (28 / 2 blocks).
	NodesPerBlock = 14
	// NodesPerRack is the number of nodes per rack (28 / 4 racks).
	NodesPerRack = 7
)

// GetZoneForNodeIndex returns the zone label for a given node index.
// Both the index parameter and the returned zone number are 0-based.
// e.g., nodes 0-27 → zone-0, nodes 28-55 → zone-1, etc.
func GetZoneForNodeIndex(idx int) string {
	zoneNum := idx / NodesPerZone
	return fmt.Sprintf("zone-%d", zoneNum)
}

// GetBlockForNodeIndex returns the block label for a given node index.
// Both the index parameter and the returned block number are 0-based.
// e.g., nodes 0-13 → block-0, nodes 14-27 → block-1
func GetBlockForNodeIndex(idx int) string {
	blockNum := idx / NodesPerBlock
	return fmt.Sprintf("block-%d", blockNum)
}

// GetRackForNodeIndex returns the rack label for a given node index.
// Both the index parameter and the returned rack number are 0-based.
// e.g., nodes 0-6 → rack-0, nodes 7-13 → rack-1, etc.
func GetRackForNodeIndex(idx int) string {
	rackNum := idx / NodesPerRack
	return fmt.Sprintf("rack-%d", rackNum)
}

// GetWorkerNodeLabelSelector returns the label selector for worker nodes in e2e tests.
// Returns a formatted string "key=value" for use with Kubernetes label selectors.
func GetWorkerNodeLabelSelector() string {
	return fmt.Sprintf("%s=%s", WorkerNodeLabelKey, WorkerNodeLabelValue)
}

// applyTopologyLabels applies hierarchical topology labels to worker nodes in the k3d cluster.
// Creates a 4-level topology hierarchy: zone -> block -> rack -> host (kubernetes.io/hostname already exists)
// Distribution strategy for 28 worker nodes:
//   - Zone: all nodes in "zone-0"
//   - Block: nodes 0-13 in "block-0", nodes 14-27 in "block-1"
//   - Rack: 4 racks total (2 per block), 7 hosts per rack
func applyTopologyLabels(ctx context.Context, restConfig *rest.Config, logger *utils.Logger) error {
	logger.Info("🏷️  Applying hierarchical topology labels to worker nodes...")

	// Create clientset
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create clientset: %w", err)
	}

	// Get all worker nodes (filter by label set during cluster creation)
	workerLabelSelector := GetWorkerNodeLabelSelector()
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: workerLabelSelector,
	})
	if err != nil {
		return fmt.Errorf("failed to list worker nodes: %w", err)
	}

	if len(nodes.Items) == 0 {
		logger.Warn("⚠️  No worker nodes found for topology labeling")
		return nil
	}

	sortedNodes := make([]v1.Node, len(nodes.Items))
	copy(sortedNodes, nodes.Items)
	sort.Slice(sortedNodes, func(i, j int) bool { return sortedNodes[i].Name < sortedNodes[j].Name })

	for idx, node := range sortedNodes {
		topologyLabels := fmt.Sprintf(`{"metadata":{"labels":{"%s":"%s","%s":"%s","%s":"%s"}}}`,
			TopologyLabelZone, GetZoneForNodeIndex(idx), TopologyLabelBlock, GetBlockForNodeIndex(idx), TopologyLabelRack, GetRackForNodeIndex(idx))

		_, err := clientset.CoreV1().Nodes().Patch(
			ctx,
			node.Name,
			k8stypes.StrategicMergePatchType,
			[]byte(topologyLabels),
			metav1.PatchOptions{},
		)
		if err != nil {
			return fmt.Errorf("failed to patch node %s with topology labels: %w", node.Name, err)
		}
	}
	logger.Infof("✅ Applied topology labels to %d worker nodes", len(sortedNodes))
	return nil
}
