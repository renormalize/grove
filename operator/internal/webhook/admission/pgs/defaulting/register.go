// /*
// Copyright 2024 The Grove Authors.
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

package defaulting

import (
	"github.com/NVIDIA/grove/operator/api/core/v1alpha1"

	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	// HandlerName is the name of the default webhook handler for PodGangSet.
	HandlerName = "podgangset-default-webhook"
	webhookPath = "/webhooks/default-podgangset"
)

// RegisterWithManager registers the webhook with the manager.
func (h *Handler) RegisterWithManager(mgr manager.Manager) error {
	webhook := admission.
		WithCustomDefaulter(mgr.GetScheme(), &v1alpha1.PodGangSet{}, h).
		WithRecoverPanic(true)
	mgr.GetWebhookServer().Register(webhookPath, webhook)
	return nil
}
