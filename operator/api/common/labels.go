package common

// Common label keys to be placed on all resources managed by grove operator.
const (
	// LabelAppNameKey is a key of a label which sets the name of the resource.
	LabelAppNameKey = "app.kubernetes.io/name"
	// LabelManagedByKey is a key of a label which sets the operator which manages this resource.
	LabelManagedByKey = "app.kubernetes.io/managed-by"
	// LabelPartOfKey is a key of a label which sets the type of the resource.
	LabelPartOfKey = "app.kubernetes.io/part-of"
	// LabelManagedByValue is the value for LabelManagedByKey
	LabelManagedByValue = "grove-operator"
	// LabelComponentKey is a key for a label that sets the component type on resources provisioned for a PodGangSet.
	LabelComponentKey = "app.kubernetes.io/component"
	// LabelPodClique is a key for a label that sets the PodClique name.
	LabelPodClique = "grove.io/podclique"
	// LabelPodGang is a key for a label that sets the PodGang name.
	LabelPodGang = "grove.io/podgang"
	// LabelBasePodGang is a key for a label that sets the base PodGang name for scaled PodGangs.
	// This label is present on scaled PodGangs (beyond MinAvailable) and points to their base PodGang.
	LabelBasePodGang = "grove.io/base-podgang"
	// LabelPodGangSetReplicaIndex is a key for a label that sets the replica index of a PodGangSet.
	LabelPodGangSetReplicaIndex = "grove.io/podgangset-replica-index"
	// LabelPodCliqueScalingGroup is a key for a label that sets the PodCliqueScalingGroup name.
	LabelPodCliqueScalingGroup = "grove.io/podcliquescalinggroup"
	// LabelPodCliqueScalingGroupReplicaIndex is a key for a label that sets the replica index of a PodCliqueScalingGroup within PodGangSet.
	LabelPodCliqueScalingGroupReplicaIndex = "grove.io/podcliquescalinggroup-replica-index"
	// LabelPodTemplateHash is a key for a label that sets the hash of the PodSpec. This label will be set on a PodClique and will be shared by all pods in the PodClique.
	LabelPodTemplateHash = "grove.io/pod-template-hash"
)

// GetDefaultLabelsForPodGangSetManagedResources gets the default labels for resources managed by PodGangset.
func GetDefaultLabelsForPodGangSetManagedResources(pgsName string) map[string]string {
	return map[string]string{
		LabelManagedByKey: LabelManagedByValue,
		LabelPartOfKey:    pgsName,
	}
}
