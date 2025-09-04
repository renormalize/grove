package errors

import "errors"

var (
	// ErrMissingPodTemplateHashLabel is a sentinel error indicating that a pod is missing the pod-template-hash label.
	ErrMissingPodTemplateHashLabel = errors.New("missing pod template hash label")
)
