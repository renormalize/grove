package utils

import (
	grovecorev1alpha1 "github.com/NVIDIA/grove/operator/api/core/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/ptr"
	"time"
)

type PodBuilder struct {
	pod *corev1.Pod
}

// NewPodBuilder creates a new PodBuilder with the given name and namespace.
func NewPodBuilder(name, namespace string) *PodBuilder {
	return &PodBuilder{
		pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{},
			},
		},
	}
}

func NewPodWithBuilderWithDefaultSpec(name, namespace string) *PodBuilder {
	return &PodBuilder{
		pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: createDefaultPodSpec(),
		},
	}
}

func (b *PodBuilder) Build() *corev1.Pod {
	return b.pod
}

// WithOwner adds an owner reference to the Pod.
func (b *PodBuilder) WithOwner(ownerName string) *PodBuilder {
	ownerRef := metav1.OwnerReference{
		APIVersion:         grovecorev1alpha1.SchemeGroupVersion.String(),
		Kind:               grovecorev1alpha1.KindPodClique,
		Name:               ownerName,
		UID:                uuid.NewUUID(),
		BlockOwnerDeletion: ptr.To(true),
		Controller:         ptr.To(true),
	}
	b.pod.ObjectMeta.OwnerReferences = append(b.pod.ObjectMeta.OwnerReferences, ownerRef)
	return b
}

// WithLabels adds labels to the Pod.
func (b *PodBuilder) WithLabels(labels map[string]string) *PodBuilder {
	if b.pod.ObjectMeta.Labels == nil {
		b.pod.ObjectMeta.Labels = make(map[string]string)
	}
	for key, value := range labels {
		b.pod.ObjectMeta.Labels[key] = value
	}
	return b
}

// MarkForTermination sets the DeletionTimestamp on the Pod.
func (b *PodBuilder) MarkForTermination() *PodBuilder {
	b.pod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	return b
}

// WithCondition adds a condition to the Pod's status.
func (b *PodBuilder) WithCondition(cond corev1.PodCondition) *PodBuilder {
	b.pod.Status.Conditions = append(b.pod.Status.Conditions, cond)
	return b
}

// WithPhase adds a pod phase to the Pod's status.
func (b *PodBuilder) WithPhase(phase corev1.PodPhase) *PodBuilder {
	b.pod.Status.Phase = phase
	return b
}

func createDefaultPodSpec() corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:    "test-container",
				Image:   "alpine:3.21",
				Command: []string{"/bin/sh", "-c", "sleep 2m"},
			},
		},
		RestartPolicy:                 corev1.RestartPolicyAlways,
		TerminationGracePeriodSeconds: ptr.To[int64](30),
	}
}
