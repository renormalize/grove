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
	"testing"

	apicommon "github.com/ai-dynamo/grove/operator/api/common"
	grovecorev1alpha1 "github.com/ai-dynamo/grove/operator/api/core/v1alpha1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestBuildBaseAndScaledPodGangTuples_ErrorWhenGenerationHashNotSet(t *testing.T) {
	// CurrentGenerationHash not set → error.
	pcs := newTestPCS("my-pcs", "", []grovecorev1alpha1.PodCliqueTemplateSpec{
		{Name: "prefill", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 2}},
	}, nil)

	r := &_resource{}
	tuples, err := r.buildBaseAndScaledPodGangTuples(pcs, 0, nil, nil)

	require.Error(t, err)
	assert.Nil(t, tuples)
}

func TestBuildBaseAndScaledPodGangTuples_StandaloneOnly(t *testing.T) {
	// PCS with 2 standalone PCLQs, no PCSGs. Live PCLQs match template replicas.
	// Expect: one BasePodGang tuple with both PCLQs, no ScaledPodGang tuples.
	pcs := newTestPCS("my-pcs", "abc12xyz", []grovecorev1alpha1.PodCliqueTemplateSpec{
		{Name: "prefill", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 2}},
		{Name: "decode", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 4}},
	}, nil)
	livePCLQs := []grovecorev1alpha1.PodClique{
		newLivePCLQ("my-pcs", 0, "prefill", 2),
		newLivePCLQ("my-pcs", 0, "decode", 4),
	}

	r := &_resource{}
	tuples, err := r.buildBaseAndScaledPodGangTuples(pcs, 0, nil, livePCLQs)

	require.NoError(t, err)
	assert.Len(t, tuples, 1)
	basePodGang := tuples[0]
	assert.Equal(t, "my-pcs-0", basePodGang.Name)
	assert.Equal(t, "abc12xyz", basePodGang.PodCliqueSetGenerationHash)
	assert.Equal(t, int32(2), basePodGang.PodCliques["my-pcs-0-prefill"])
	assert.Equal(t, int32(4), basePodGang.PodCliques["my-pcs-0-decode"])
	assert.Empty(t, basePodGang.PodCliqueScalingGroups)
}

func TestBuildBaseAndScaledPodGangTuples_LiveReplicaCountTakesPrecedence(t *testing.T) {
	// Live PCLQ has a different replica count than the template (e.g. scaled directly).
	// Expect: BasePodGang tuple uses live replica count, not template.
	pcs := newTestPCS("my-pcs", "abc12xyz", []grovecorev1alpha1.PodCliqueTemplateSpec{
		{Name: "prefill", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 2}},
	}, nil)
	livePCLQs := []grovecorev1alpha1.PodClique{
		newLivePCLQ("my-pcs", 0, "prefill", 5), // scaled to 5, template says 2
	}

	r := &_resource{}
	tuples, err := r.buildBaseAndScaledPodGangTuples(pcs, 0, nil, livePCLQs)

	require.NoError(t, err)
	assert.Len(t, tuples, 1)
	assert.Equal(t, int32(5), tuples[0].PodCliques["my-pcs-0-prefill"])
}

func TestBuildBaseAndScaledPodGangTuples_FallsBackToTemplateWhenPCLQNotYetCreated(t *testing.T) {
	// No live PCLQs (first reconcile before PCLQ component runs).
	// Expect: BasePodGang tuple uses template replica count as fallback.
	pcs := newTestPCS("my-pcs", "abc12xyz", []grovecorev1alpha1.PodCliqueTemplateSpec{
		{Name: "prefill", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 3}},
	}, nil)

	r := &_resource{}
	tuples, err := r.buildBaseAndScaledPodGangTuples(pcs, 0, nil, nil)

	require.NoError(t, err)
	assert.Len(t, tuples, 1)
	assert.Equal(t, int32(3), tuples[0].PodCliques["my-pcs-0-prefill"])
}

func TestBuildBaseAndScaledPodGangTuples_WithPCSGNoScaling(t *testing.T) {
	// PCS with one standalone PCLQ and one PCSG whose Replicas == MinAvailable.
	// Expect: one BasePodGang tuple (standalone PCLQ + PCSG at MinAvailable), no ScaledPodGang tuples.
	pcs := newTestPCS("my-pcs", "abc12xyz",
		[]grovecorev1alpha1.PodCliqueTemplateSpec{
			{Name: "prefill", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 2}},
			{Name: "decode", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 4}},
		},
		[]grovecorev1alpha1.PodCliqueScalingGroupConfig{
			{Name: "sga", CliqueNames: []string{"decode"}, MinAvailable: ptr.To[int32](2)},
		},
	)
	pcsg := newPCSG("my-pcs-0-sga", 2, 2) // Replicas == MinAvailable → no ScaledPodGangs
	livePCLQs := []grovecorev1alpha1.PodClique{
		newLivePCLQ("my-pcs", 0, "prefill", 2),
	}

	r := &_resource{}
	tuples, err := r.buildBaseAndScaledPodGangTuples(pcs, 0, []grovecorev1alpha1.PodCliqueScalingGroup{pcsg}, livePCLQs)

	require.NoError(t, err)
	assert.Len(t, tuples, 1)
	basePodGang := tuples[0]
	assert.Equal(t, "my-pcs-0", basePodGang.Name)
	assert.Equal(t, int32(2), basePodGang.PodCliques["my-pcs-0-prefill"])
	// "decode" is owned by PCSG — must not appear in PodCliques.
	_, decodePresent := basePodGang.PodCliques["my-pcs-0-decode"]
	assert.False(t, decodePresent, "PCSG-owned PCLQ must not appear in BasePodGang PodCliques")
	assert.Equal(t, int32(2), basePodGang.PodCliqueScalingGroups["my-pcs-0-sga"])
}

func TestBuildBaseAndScaledPodGangTuples_WithPCSGScaledBeyondMinAvailable(t *testing.T) {
	// PCSG with Replicas=5, MinAvailable=3 → 1 BasePodGang tuple + 2 ScaledPodGang tuples.
	pcs := newTestPCS("my-pcs", "abc12xyz",
		[]grovecorev1alpha1.PodCliqueTemplateSpec{
			{Name: "decode", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 4}},
		},
		[]grovecorev1alpha1.PodCliqueScalingGroupConfig{
			{Name: "sga", CliqueNames: []string{"decode"}, MinAvailable: ptr.To[int32](3)},
		},
	)
	pcsg := newPCSG("my-pcs-0-sga", 5, 3)

	r := &_resource{}
	tuples, err := r.buildBaseAndScaledPodGangTuples(pcs, 0, []grovecorev1alpha1.PodCliqueScalingGroup{pcsg}, nil)

	require.NoError(t, err)
	// 1 BasePodGang + 2 ScaledPodGangs (replicas 3 and 4 beyond MinAvailable=3)
	assert.Len(t, tuples, 3)

	basePodGang := tuples[0]
	assert.Equal(t, "my-pcs-0", basePodGang.Name)
	assert.Equal(t, int32(3), basePodGang.PodCliqueScalingGroups["my-pcs-0-sga"])

	scaledPodGang0 := tuples[1]
	assert.Equal(t, "my-pcs-0-sga-0", scaledPodGang0.Name)
	assert.Equal(t, int32(1), scaledPodGang0.PodCliqueScalingGroups["my-pcs-0-sga"])
	assert.Empty(t, scaledPodGang0.PodCliques)

	scaledPodGang1 := tuples[2]
	assert.Equal(t, "my-pcs-0-sga-1", scaledPodGang1.Name)
	assert.Equal(t, int32(1), scaledPodGang1.PodCliqueScalingGroups["my-pcs-0-sga"])
}

func TestBuildBaseAndScaledPodGangTuples_GenerationHashPropagated(t *testing.T) {
	// Verify the generation hash appears on every tuple.
	pcs := newTestPCS("my-pcs", "hashval99",
		[]grovecorev1alpha1.PodCliqueTemplateSpec{
			{Name: "prefill", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 1}},
		}, nil)
	livePCLQs := []grovecorev1alpha1.PodClique{
		newLivePCLQ("my-pcs", 0, "prefill", 1),
	}

	r := &_resource{}
	tuples, err := r.buildBaseAndScaledPodGangTuples(pcs, 0, nil, livePCLQs)

	require.NoError(t, err)
	assert.Len(t, tuples, 1)
	assert.Equal(t, "hashval99", tuples[0].PodCliqueSetGenerationHash)
}

func TestComputeTuples_CoherentUpdateInProgressReturnsNil(t *testing.T) {
	// Coherent update in progress → TODO path returns nil (stub).
	pcs := newTestPCS("my-pcs", "abc12xyz", nil, nil)
	pcs.Spec.UpdateStrategy = &grovecorev1alpha1.PodCliqueSetUpdateStrategy{
		Type: grovecorev1alpha1.CoherentStrategy,
	}
	pcs.Status.CoherentUpdateProgress = &grovecorev1alpha1.CoherentUpdateProgress{
		UpdateStartedAt: metav1.Now(),
		UpdateEndedAt:   nil,
	}

	r := &_resource{}
	tuples, err := r.computeTuples(pcs, 0, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, tuples)
}

func TestComputeTuples_CoherentNoUpdateReturnsNil(t *testing.T) {
	// Coherent strategy, no update in progress → TODO path returns nil (stub).
	pcs := newTestPCS("my-pcs", "abc12xyz", nil, nil)
	pcs.Spec.UpdateStrategy = &grovecorev1alpha1.PodCliqueSetUpdateStrategy{
		Type: grovecorev1alpha1.CoherentStrategy,
	}

	r := &_resource{}
	tuples, err := r.computeTuples(pcs, 0, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, tuples)
}

func TestComputeTuples_OnDeleteUsesBasePodGangTuples(t *testing.T) {
	// OnDelete strategy uses the same BasePodGang/ScaledPodGang tuple structure as RollingRecreate.
	pcs := newTestPCS("my-pcs", "abc12xyz", []grovecorev1alpha1.PodCliqueTemplateSpec{
		{Name: "prefill", Spec: grovecorev1alpha1.PodCliqueSpec{Replicas: 2}},
	}, nil)
	pcs.Spec.UpdateStrategy = &grovecorev1alpha1.PodCliqueSetUpdateStrategy{
		Type: grovecorev1alpha1.OnDeleteStrategy,
	}
	livePCLQs := []grovecorev1alpha1.PodClique{
		newLivePCLQ("my-pcs", 0, "prefill", 2),
	}

	r := &_resource{}
	tuples, err := r.computeTuples(pcs, 0, nil, livePCLQs)

	require.NoError(t, err)
	assert.Len(t, tuples, 1)
	assert.Equal(t, "my-pcs-0", tuples[0].Name)
	assert.Equal(t, int32(2), tuples[0].PodCliques["my-pcs-0-prefill"])
}

// newTestPCS creates a minimal PodCliqueSet for use in unit tests.
func newTestPCS(name string, generationHash string, cliques []grovecorev1alpha1.PodCliqueTemplateSpec, pcsgConfigs []grovecorev1alpha1.PodCliqueScalingGroupConfig) *grovecorev1alpha1.PodCliqueSet {
	cliquePtrs := make([]*grovecorev1alpha1.PodCliqueTemplateSpec, len(cliques))
	for i := range cliques {
		cliquePtrs[i] = &cliques[i]
	}
	pcs := &grovecorev1alpha1.PodCliqueSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: grovecorev1alpha1.PodCliqueSetSpec{
			Replicas: 1,
			Template: grovecorev1alpha1.PodCliqueSetTemplateSpec{
				Cliques:                      cliquePtrs,
				PodCliqueScalingGroupConfigs: pcsgConfigs,
			},
		},
	}
	if generationHash != "" {
		pcs.Status.CurrentGenerationHash = &generationHash
	}
	return pcs
}

// newPCSG creates a minimal PodCliqueScalingGroup for use in unit tests.
func newPCSG(name string, replicas, minAvailable int32) grovecorev1alpha1.PodCliqueScalingGroup {
	return grovecorev1alpha1.PodCliqueScalingGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: grovecorev1alpha1.PodCliqueScalingGroupSpec{
			Replicas:     replicas,
			MinAvailable: ptr.To(minAvailable),
		},
	}
}

// newLivePCLQ creates a minimal standalone PodClique (owned by PCS) with the given replica count.
func newLivePCLQ(pcsName string, replicaIndex int, cliqueName string, replicas int32) grovecorev1alpha1.PodClique {
	fqn := apicommon.GeneratePodCliqueName(apicommon.ResourceNameReplica{Name: pcsName, Replica: replicaIndex}, cliqueName)
	return grovecorev1alpha1.PodClique{
		ObjectMeta: metav1.ObjectMeta{Name: fqn},
		Spec:       grovecorev1alpha1.PodCliqueSpec{Replicas: replicas},
	}
}
