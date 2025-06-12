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

package main

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	grovecorev1alpha1 "github.com/NVIDIA/grove/operator/api/core/v1alpha1"
	groveclientset "github.com/NVIDIA/grove/operator/client/clientset/versioned"
	groveinformers "github.com/NVIDIA/grove/operator/client/informers/externalversions"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// knownPodCliqueState contains the last known state of the readiness of all ancestor PodCliques
type knownPodCliqueState struct {
	mutex         *sync.Mutex
	ancestorReady map[string]bool
}

// newKnownPodCliqueState creates and initializes all ancestor PodCliques with an unready state
func newKnownPodCliqueState(cliqueNames []string) knownPodCliqueState {
	state := knownPodCliqueState{
		mutex:         &sync.Mutex{},
		ancestorReady: map[string]bool{},
	}

	// initialize the keys explicitly
	for _, cliqueName := range cliqueNames {
		state.ancestorReady[cliqueName] = false
	}
	return state
}

// verifyAncestorReadiness updates the known readiness state of the PodCliques
func verifyAncestorReadiness(pclq *grovecorev1alpha1.PodClique, state *knownPodCliqueState, readyCh chan<- bool) {
	l.Info("Ancestor PodClique:",
		"PodClique.Name", pclq.Name,
		"PodClique.Spec.Replicas", pclq.Spec.Replicas,
		"PodClique.Status.ReadyReplicas", pclq.Status.ReadyReplicas,
	)
	// check if the PodClique was already ready
	if state.ancestorReady[pclq.Name] {
		if pclq.Spec.Replicas != pclq.Status.ReadyReplicas {
			state.ancestorReady[pclq.Name] = false
			readyCh <- false
			l.Info("The ancestor PodClique that was previously ready is not ready any more", "PodClique", pclq.Name)
		}
		l.Info("The ancestor PodClique was already ready", "PodClique", pclq.Name)
		return
	}
	// The PodClique was not ready previously, check if it is ready now
	if pclq.Spec.Replicas != pclq.Status.ReadyReplicas {
		l.Info("The ancestor PodClique is not ready", "PodClique", pclq.Name)
	} else {
		state.ancestorReady[pclq.Name] = true
		readyCh <- true
		l.Info("The ancestor PodClique has become ready", "PodClique", pclq.Name)
	}
}

// run waits for all PodCliques to be ready, and exits either when all are ready or some error occurs.
func run(ctx context.Context, initConfig InitConfig) error {
	ancestorPodCliqueNames, ancestorPodCliqueNamespace := initConfig.PodCliqueNames(), initConfig.PodCliqueNamespace()
	l.Info("The clique names are:", "cliques", ancestorPodCliqueNames)
	l.Info("The clique namespace is:", "cliquesNamespace", ancestorPodCliqueNamespace)

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to fetch the in cluster config with error %w", err)
	}
	clientSet, err := groveclientset.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create clientSet with the fetched restConfig with error %w", err)
	}

	// The knownPodCliqueState's map stores the last known state of each ancestor PodClique's readiness.
	// When the clique was not ready:
	// 1. Events that show the clique is unready will be ignored, and the wg is not reduced.
	// 2. Events that show the clique has become ready will set the readiness to true, and wg is reduced.
	// When the clique was already ready:
	// 1. Events that show the clique is ready will be ignored, and the wg is not reduced.
	// 2. Events that show the clique has become unready will set the readiness to false, and wg is increased.
	// Once all cliques are ready, the wg will be 0, and wg.Wait will be exit.
	state := newKnownPodCliqueState(ancestorPodCliqueNames)

	// the readyCh channel is written to in two cases:
	// 1. When an unready clique becomes ready: true
	// 2. When a ready clique becomes unready: false
	readyCh := make(chan bool, len(ancestorPodCliqueNames))
	var wg sync.WaitGroup
	wg.Add(len(ancestorPodCliqueNames))
	go func() {
		for ready := range readyCh {
			if !ready {
				wg.Add(1) // a ready PodClique has become unready
			} else {
				wg.Done() // an unready PodClique has become ready
			}
		}
	}()

	factory := groveinformers.NewSharedInformerFactoryWithOptions(clientSet, time.Second, groveinformers.WithNamespace(ancestorPodCliqueNamespace))
	typedInformer := factory.Grove().V1alpha1().PodCliques().Informer()
	_, err = typedInformer.AddEventHandlerWithOptions(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			state.mutex.Lock()
			defer state.mutex.Unlock()
			pclq, ok := obj.(*grovecorev1alpha1.PodClique)
			if !ok {
				return
			}
			if !slices.Contains(ancestorPodCliqueNames, pclq.Name) {
				// pclq is not an ancestor
				return
			}
			verifyAncestorReadiness(pclq, &state, readyCh)
		},
		UpdateFunc: func(_, obj any) {
			state.mutex.Lock()
			defer state.mutex.Unlock()
			pclq, ok := obj.(*grovecorev1alpha1.PodClique)
			if !ok {
				return
			}
			if !slices.Contains(ancestorPodCliqueNames, pclq.Name) {
				// pclq is not an ancestor
				return
			}
			verifyAncestorReadiness(pclq, &state, readyCh)
		},
		DeleteFunc: func(_ any) {
			// the delete function should not be entered, since PodCliques are immutable in a PodGangSet.
			l.Error(
				fmt.Errorf("PodCliques were deleted"),
				"PodCliques which are immutable in a PodGangSet were deleted while the initcontainer was waiting for startup",
			)
			return
		},
	}, cache.HandlerOptions{Logger: &l})
	if err != nil {
		return fmt.Errorf("failed to add the event handler to the informer")
	}

	synced := factory.WaitForCacheSync(ctx.Done())
	for v, ok := range synced {
		if !ok {
			l.Error(fmt.Errorf("failed to sync informer cache"), "Caches failed to sync", "type", v)
		}
	}

	factory.Start(ctx.Done())

	wg.Wait()
	close(readyCh)

	return nil
}
