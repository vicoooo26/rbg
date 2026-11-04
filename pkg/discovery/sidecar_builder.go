/*
Copyright 2026 The RBG Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package discovery

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// InjectEngineRuntimeFragments merges the referenced profiles' pod-level
// fragments into one position's rendered pod spec, in reference order:
// initContainers, containers (sidecars) and volumes append-if-missing
// (name-keyed, therefore idempotent), then the role-side Containers override
// appends env/args to profile-provided containers. Fragment application
// cannot fail: the resolver has already fetched and validated the profiles,
// so overlapping fragment delivery and unknown override names are no-ops.
func InjectEngineRuntimeFragments(
	ctx context.Context, podSpec *v1.PodTemplateSpec, fragments []EngineRuntimeFragment,
) {
	logger := log.FromContext(ctx)
	for _, fragment := range fragments {
		profileContainerNames := make(
			map[string]struct{},
			len(fragment.Profile.Spec.InitContainers)+len(fragment.Profile.Spec.Containers),
		)
		for _, initContainer := range fragment.Profile.Spec.InitContainers {
			profileContainerNames[initContainer.Name] = struct{}{}
			if !hasNamedContainer(podSpec.Spec.InitContainers, initContainer.Name) {
				podSpec.Spec.InitContainers = append(podSpec.Spec.InitContainers, initContainer)
			}
		}
		for _, container := range fragment.Profile.Spec.Containers {
			profileContainerNames[container.Name] = struct{}{}
			if !hasNamedContainer(podSpec.Spec.Containers, container.Name) {
				podSpec.Spec.Containers = append(podSpec.Spec.Containers, container)
			}
		}
		for _, vol := range fragment.Profile.Spec.Volumes {
			if !hasNamedVolume(podSpec.Spec.Volumes, vol.Name) {
				podSpec.Spec.Volumes = append(podSpec.Spec.Volumes, vol)
			}
		}
		// override containers based on the role-side runtime entry
		for _, container := range fragment.Runtime.Containers {
			if _, owned := profileContainerNames[container.Name]; !owned {
				logger.V(1).Info(fmt.Sprintf(
					"rbg runtime has container %s but not in clusterEngineRuntime, skip override",
					container.Name,
				))
				continue
			}
			for i, c := range podSpec.Spec.Containers {
				if c.Name == container.Name {
					if len(container.Env) > 0 {
						podSpec.Spec.Containers[i].Env = append(podSpec.Spec.Containers[i].Env, container.Env...)
					}
					if len(container.Args) > 0 {
						podSpec.Spec.Containers[i].Args = append(podSpec.Spec.Containers[i].Args, container.Args...)
					}
					break
				}
			}
		}
	}
}

func hasNamedContainer(containers []v1.Container, name string) bool {
	for _, c := range containers {
		if c.Name == name {
			return true
		}
	}
	return false
}

func hasNamedVolume(volumes []v1.Volume, name string) bool {
	for _, vol := range volumes {
		if vol.Name == name {
			return true
		}
	}
	return false
}
