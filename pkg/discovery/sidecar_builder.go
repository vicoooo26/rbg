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
// appends env/args to profile-provided containers, once per reference exactly
// as on main. Env appending joins the KEP-531 shared env-name conflict check
// spanning all three passes: an override entry whose name the target
// container already declares — from the role's own template, the profile, an
// earlier reference's merge or anything else already rendered at that point —
// is a render error attributed to the role-side override (role name,
// reference and containers index), never a silent second entry. Args
// appending carries no conflict check; main's behavior is untouched.
// roleName attributes those errors to the role whose engineRuntimes authored
// the overrides. Overlapping fragment delivery and unknown override names
// remain no-ops.
func InjectEngineRuntimeFragments(
	ctx context.Context, podSpec *v1.PodTemplateSpec, fragments []EngineRuntimeFragment, roleName string,
) error {
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
		for i, container := range fragment.Runtime.Containers {
			if _, owned := profileContainerNames[container.Name]; !owned {
				logger.V(1).Info(fmt.Sprintf(
					"rbg runtime has container %s but not in clusterEngineRuntime, skip override",
					container.Name,
				))
				continue
			}
			for j, c := range podSpec.Spec.Containers {
				if c.Name != container.Name {
					continue
				}
				target := &podSpec.Spec.Containers[j]
				for _, env := range container.Env {
					if hasNamedEnv(target.Env, env.Name) {
						// The legacy merge appends; it cannot compose with an
						// existing value (join semantics), so the collision is
						// an error attributed to the role-side override rather
						// than a silent second entry — the same refusal rule
						// the patch passes enforce.
						return fmt.Errorf(
							"role %q engineRuntimes[%q].containers[%d] appends env var %q to container %q, "+
								"which already declares it; the legacy containers merge cannot join values — "+
								"a same-named entry must come from the role template or the override, never both",
							roleName, fragment.Runtime.ProfileName, i, env.Name, container.Name,
						)
					}
					target.Env = append(target.Env, env)
				}
				if len(container.Args) > 0 {
					target.Args = append(target.Args, container.Args...)
				}
				break
			}
		}
	}
	return nil
}

func hasNamedContainer(containers []v1.Container, name string) bool {
	for _, c := range containers {
		if c.Name == name {
			return true
		}
	}
	return false
}

// hasNamedEnv reports whether the env list already carries the given name —
// the legacy containers merge's share of the shared env-name conflict check.
func hasNamedEnv(envs []v1.EnvVar, name string) bool {
	for _, env := range envs {
		if env.Name == name {
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
