//go:build e2e

package utils

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

import "fmt"

// GetBasePodGangName constructs the base PodGang name for a specific PCS replica.
// Format: <pgs-name>-<replica-index>
func GetBasePodGangName(workloadName string, pcsReplica int) string {
	return fmt.Sprintf("%s-%d", workloadName, pcsReplica)
}

// GetStandalonePCLQSubGroupName constructs the SubGroup name for a standalone PodClique.
// Format: <pcs-name>-<pcs-replica>-<clique-name>
func GetStandalonePCLQSubGroupName(pcsName string, pcsReplica int, cliqueName string) string {
	return fmt.Sprintf("%s-%d-%s", pcsName, pcsReplica, cliqueName)
}

// GetPCSGParentSubGroupName constructs the SubGroup name for a PCSG parent (scaling group replica).
// Format: <pcs-name>-<pcs-replica>-<sg-name>-<sg-replica>
func GetPCSGParentSubGroupName(pcsName string, pcsReplica int, sgName string, sgReplica int) string {
	return fmt.Sprintf("%s-%d-%s-%d", pcsName, pcsReplica, sgName, sgReplica)
}

// GetPCLQInPCSGSubGroupName constructs the SubGroup name for a PodClique within a PCSG.
// Format: <pcs-name>-<pcs-replica>-<sg-name>-<sg-replica>-<clique-name>
func GetPCLQInPCSGSubGroupName(pcsName string, pcsReplica int, sgName string, sgReplica int, cliqueName string) string {
	return fmt.Sprintf("%s-%d-%s-%d-%s", pcsName, pcsReplica, sgName, sgReplica, cliqueName)
}
