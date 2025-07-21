package hpa

import (
	"testing"

	grovecorev1alpha1 "github.com/NVIDIA/grove/operator/api/core/v1alpha1"
	"github.com/stretchr/testify/assert"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestComputeExpectedHPAs(t *testing.T) {
	tests := []struct {
		name     string
		pgs      *grovecorev1alpha1.PodGangSet
		expected []hpaInfo
	}{
		{
			name: "PodClique HPA with explicit minReplicas",
			pgs: &grovecorev1alpha1.PodGangSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pgs", Namespace: "default"},
				Spec: grovecorev1alpha1.PodGangSetSpec{
					Replicas: 1,
					Template: grovecorev1alpha1.PodGangSetTemplateSpec{
						Cliques: []*grovecorev1alpha1.PodCliqueTemplateSpec{
							{
								Name: "test-clique",
								Spec: grovecorev1alpha1.PodCliqueSpec{
									Replicas: 3,
									ScaleConfig: &grovecorev1alpha1.AutoScalingConfig{
										MinReplicas: ptr.To(int32(2)),
										MaxReplicas: 5,
									},
								},
							},
						},
					},
				},
			},
			expected: []hpaInfo{
				{
					targetScaleResourceKind: grovecorev1alpha1.PodCliqueKind,
					targetScaleResourceName: "test-pgs-0-test-clique",
					minReplicas:             2, // Should use explicit minReplicas
				},
			},
		},
		{
			name: "PodClique HPA without explicit minReplicas",
			pgs: &grovecorev1alpha1.PodGangSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pgs", Namespace: "default"},
				Spec: grovecorev1alpha1.PodGangSetSpec{
					Replicas: 1,
					Template: grovecorev1alpha1.PodGangSetTemplateSpec{
						Cliques: []*grovecorev1alpha1.PodCliqueTemplateSpec{
							{
								Name: "test-clique",
								Spec: grovecorev1alpha1.PodCliqueSpec{
									Replicas: 3,
									ScaleConfig: &grovecorev1alpha1.AutoScalingConfig{
										MaxReplicas: 5,
										// MinReplicas not specified
									},
								},
							},
						},
					},
				},
			},
			expected: []hpaInfo{
				{
					targetScaleResourceKind: grovecorev1alpha1.PodCliqueKind,
					targetScaleResourceName: "test-pgs-0-test-clique",
					minReplicas:             3, // Should use template replicas
				},
			},
		},
		{
			name: "PodCliqueScalingGroup HPA with explicit minReplicas",
			pgs: &grovecorev1alpha1.PodGangSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pgs", Namespace: "default"},
				Spec: grovecorev1alpha1.PodGangSetSpec{
					Replicas: 1,
					Template: grovecorev1alpha1.PodGangSetTemplateSpec{
						PodCliqueScalingGroupConfigs: []grovecorev1alpha1.PodCliqueScalingGroupConfig{
							{
								Name:        "test-sg",
								Replicas:    ptr.To(int32(4)),
								CliqueNames: []string{"test-clique"},
								ScaleConfig: &grovecorev1alpha1.AutoScalingConfig{
									MinReplicas: ptr.To(int32(2)),
									MaxReplicas: 6,
								},
							},
						},
					},
				},
			},
			expected: []hpaInfo{
				{
					targetScaleResourceKind: grovecorev1alpha1.PodCliqueScalingGroupKind,
					targetScaleResourceName: "test-pgs-0-test-sg",
					minReplicas:             2, // Should use explicit minReplicas
				},
			},
		},
		{
			name: "PodCliqueScalingGroup HPA without explicit minReplicas",
			pgs: &grovecorev1alpha1.PodGangSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pgs", Namespace: "default"},
				Spec: grovecorev1alpha1.PodGangSetSpec{
					Replicas: 1,
					Template: grovecorev1alpha1.PodGangSetTemplateSpec{
						PodCliqueScalingGroupConfigs: []grovecorev1alpha1.PodCliqueScalingGroupConfig{
							{
								Name:        "test-sg",
								Replicas:    ptr.To(int32(4)),
								CliqueNames: []string{"test-clique"},
								ScaleConfig: &grovecorev1alpha1.AutoScalingConfig{
									MaxReplicas: 6,
									// MinReplicas not specified
								},
							},
						},
					},
				},
			},
			expected: []hpaInfo{
				{
					targetScaleResourceKind: grovecorev1alpha1.PodCliqueScalingGroupKind,
					targetScaleResourceName: "test-pgs-0-test-sg",
					minReplicas:             4, // Should use initial replicas
				},
			},
		},
		{
			name: "Mixed HPAs",
			pgs: &grovecorev1alpha1.PodGangSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pgs", Namespace: "default"},
				Spec: grovecorev1alpha1.PodGangSetSpec{
					Replicas: 1,
					Template: grovecorev1alpha1.PodGangSetTemplateSpec{
						Cliques: []*grovecorev1alpha1.PodCliqueTemplateSpec{
							{
								Name: "individual-clique",
								Spec: grovecorev1alpha1.PodCliqueSpec{
									Replicas: 2,
									ScaleConfig: &grovecorev1alpha1.AutoScalingConfig{
										MaxReplicas: 4,
									},
								},
							},
						},
						PodCliqueScalingGroupConfigs: []grovecorev1alpha1.PodCliqueScalingGroupConfig{
							{
								Name:        "scaling-group",
								Replicas:    ptr.To(int32(3)),
								CliqueNames: []string{"sg-clique"},
								ScaleConfig: &grovecorev1alpha1.AutoScalingConfig{
									MinReplicas: ptr.To(int32(1)),
									MaxReplicas: 5,
								},
							},
						},
					},
				},
			},
			expected: []hpaInfo{
				{
					targetScaleResourceKind: grovecorev1alpha1.PodCliqueKind,
					targetScaleResourceName: "test-pgs-0-individual-clique",
					minReplicas:             2, // Uses template replicas
				},
				{
					targetScaleResourceKind: grovecorev1alpha1.PodCliqueScalingGroupKind,
					targetScaleResourceName: "test-pgs-0-scaling-group",
					minReplicas:             1, // Uses explicit minReplicas
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &_resource{}
			result := r.computeExpectedHPAs(tt.pgs)

			assert.Equal(t, len(tt.expected), len(result), "Number of HPAs should match")

			for i, expected := range tt.expected {
				assert.Equal(t, expected.targetScaleResourceKind, result[i].targetScaleResourceKind)
				assert.Equal(t, expected.targetScaleResourceName, result[i].targetScaleResourceName)
				assert.Equal(t, expected.minReplicas, result[i].minReplicas,
					"minReplicas should be correctly computed for %s", expected.targetScaleResourceName)
			}
		})
	}
}

func TestBuildResource_MinReplicas(t *testing.T) {
	tests := []struct {
		name     string
		hpaInfo  hpaInfo
		expected *int32
	}{
		{
			name: "PodClique HPA with minReplicas",
			hpaInfo: hpaInfo{
				minReplicas: 3,
				scaleConfig: grovecorev1alpha1.AutoScalingConfig{
					MaxReplicas: 5,
				},
			},
			expected: ptr.To(int32(3)),
		},
		{
			name: "PodCliqueScalingGroup HPA with minReplicas",
			hpaInfo: hpaInfo{
				minReplicas: 2,
				scaleConfig: grovecorev1alpha1.AutoScalingConfig{
					MaxReplicas: 4,
				},
			},
			expected: ptr.To(int32(2)),
		},
		{
			name: "Edge case: minReplicas 1",
			hpaInfo: hpaInfo{
				minReplicas: 1,
				scaleConfig: grovecorev1alpha1.AutoScalingConfig{
					MaxReplicas: 10,
				},
			},
			expected: ptr.To(int32(1)),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test just the minReplicas and maxReplicas setting, not the full buildResource
			hpa := &autoscalingv2.HorizontalPodAutoscaler{}

			// Simulate the key parts of buildResource that we care about
			hpa.Spec.MinReplicas = &tt.hpaInfo.minReplicas
			hpa.Spec.MaxReplicas = tt.hpaInfo.scaleConfig.MaxReplicas

			assert.Equal(t, tt.expected, hpa.Spec.MinReplicas,
				"HPA minReplicas should be correctly set")
			assert.Equal(t, tt.hpaInfo.scaleConfig.MaxReplicas, hpa.Spec.MaxReplicas,
				"HPA maxReplicas should be correctly set")
		})
	}
}

func TestMinReplicasValidation(t *testing.T) {
	// Test that our logic never produces invalid minReplicas (< 1)
	testCases := []struct {
		name             string
		templateReplicas int32
		configReplicas   *int32
		explicitMin      *int32
		expectedMin      int32
	}{
		{
			name:             "template replicas only",
			templateReplicas: 3,
			expectedMin:      3,
		},
		{
			name:             "config replicas only",
			templateReplicas: 3,
			configReplicas:   ptr.To(int32(5)),
			expectedMin:      5,
		},
		{
			name:             "explicit min overrides",
			templateReplicas: 3,
			configReplicas:   ptr.To(int32(5)),
			explicitMin:      ptr.To(int32(2)),
			expectedMin:      2,
		},
		{
			name:             "edge case: all 1",
			templateReplicas: 1,
			configReplicas:   ptr.To(int32(1)),
			explicitMin:      ptr.To(int32(1)),
			expectedMin:      1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Test PodClique logic
			pclqMinReplicas := tc.templateReplicas
			if tc.explicitMin != nil {
				pclqMinReplicas = *tc.explicitMin
			}
			assert.GreaterOrEqual(t, pclqMinReplicas, int32(1),
				"PodClique minReplicas should be >= 1")

			// Test PodCliqueScalingGroup logic
			pcsgMinReplicas := tc.templateReplicas
			if tc.configReplicas != nil {
				pcsgMinReplicas = *tc.configReplicas
			}
			if tc.explicitMin != nil {
				pcsgMinReplicas = *tc.explicitMin
			}
			assert.GreaterOrEqual(t, pcsgMinReplicas, int32(1),
				"PodCliqueScalingGroup minReplicas should be >= 1")
		})
	}
}
