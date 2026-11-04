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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/rbgs/api/workloads/constants"
	workloadsv1alpha2 "sigs.k8s.io/rbgs/api/workloads/v1alpha2"
)

// StandalonePositionName is the internal sentinel position for
// standalonePattern roles. The standalone position vocabulary is empty — an
// explicit component is a render error — so no authored entry can name the
// sentinel. A customComponents component may still legally be named
// "standalone" (InstanceComponent.Name carries no pattern or length marker),
// but patterns are mutually exclusive per role, so the sentinel and a
// component name never co-key one plan — the collision is harmless, not
// impossible.
const StandalonePositionName = "standalone"

// EngineRuntimeFragment is one reference's pod-level fragments routed to one
// position: the fetched profile plus the role-side Containers override.
type EngineRuntimeFragment struct {
	Profile *workloadsv1alpha2.ClusterEngineRuntimeProfile
	Runtime workloadsv1alpha2.EngineRuntime
}

// EngineRuntimePatch is one reference's profile-side patch rules routed to
// one position. Targets holds the reference's patchContainers list
// verbatim — the bare container names the reference consents to, in
// authored order, all within the reference's named component.
type EngineRuntimePatch struct {
	ProfileName string
	Patches     []workloadsv1alpha2.ContainerPatch
	Targets     []string
}

// EngineRuntimeRolePatch is one reference's role-side patches routed to one
// position. DeclaredContainers is the profile-declared name set (init
// containers and sidecars), captured at resolve time; the render pass uses
// it only for error wording, since the resolver already rejected unknown
// targets.
type EngineRuntimeRolePatch struct {
	ProfileName        string
	Patches            []workloadsv1alpha2.EngineRuntimeContainerPatch
	DeclaredContainers map[string]struct{}
}

// EngineRuntimePlan is the resolver's per-role output: the three routing
// artifacts the render passes consume, keyed by position. FragmentApplications
// feeds the fragment pass — pod-level fragments and the legacy containers
// merge follow the reference's component scope (its effective set);
// append-only, consent-free, idempotent. PatchApplications feeds the
// profile-side patch pass (the profile's patches at the consented addresses
// of the named component); RolePatchApplications feeds the role-side patch
// pass (the reference's patches into profile-declared containers of the
// named component). All preserve role-list order within a position.
type EngineRuntimePlan struct {
	FragmentApplications  map[string][]EngineRuntimeFragment
	PatchApplications     map[string][]EngineRuntimePatch
	RolePatchApplications map[string][]EngineRuntimeRolePatch
	// Vocabulary lists the positions the role's pattern defines, in authored
	// order (leader/worker; component names; the standalone sentinel) — the
	// only positions the applications above may key by. It is recorded only
	// when the role references engine runtimes; a plan that resolved none
	// records no vocabulary and accepts any position.
	Vocabulary []string
	// RoleName is the role the plan was resolved for, used to attribute
	// role-side patch errors at render time.
	RoleName string
}

// HasPosition reports whether name is a position the role's pattern defines.
// A plan that recorded no vocabulary (the role references no engine runtimes)
// accepts every position: nothing is routed, so there is nothing to mis-key.
func (p *EngineRuntimePlan) HasPosition(name string) bool {
	if len(p.Vocabulary) == 0 {
		return true
	}
	for _, position := range p.Vocabulary {
		if position == name {
			return true
		}
	}
	return false
}

// ResolveEngineRuntimes runs once per role render, before any per-template
// pass. It computes each reference's effective component set, routes its
// fragments and both patch dimensions, and validates what no per-template
// pass can see whole: unknown components, patch fields without a component
// (the D7 re-check), duplicate patchContainers names, role-side targets the
// profile does not declare, and the D6 scope-ownership rule across two
// references to the same profile. The unaimed-patch warning is emitted here,
// once per reference per render — a per-template pass would duplicate it
// under leaderWorkerPattern once both templates run a pass.
func ResolveEngineRuntimes(
	ctx context.Context,
	k8sClient client.Client,
	recorder record.EventRecorder,
	rbg *workloadsv1alpha2.RoleBasedGroup,
	role *workloadsv1alpha2.RoleSpec,
) (*EngineRuntimePlan, error) {
	plan := &EngineRuntimePlan{
		FragmentApplications:  map[string][]EngineRuntimeFragment{},
		PatchApplications:     map[string][]EngineRuntimePatch{},
		RolePatchApplications: map[string][]EngineRuntimeRolePatch{},
	}
	if len(role.EngineRuntimes) == 0 {
		return plan, nil
	}
	plan.RoleName = role.Name

	vocabulary, positions, defaults := positionVocabulary(role)
	plan.Vocabulary = positions

	// Scope ownership state (D6): profileName -> the components it owns.
	componentOwners := map[string]map[string]struct{}{}

	for _, runtime := range role.EngineRuntimes {
		profile := &workloadsv1alpha2.ClusterEngineRuntimeProfile{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: runtime.ProfileName}, profile); err != nil {
			err = fmt.Errorf("get engine runtime %s, error: %s", runtime.ProfileName, err.Error())
			return plan, failRef(ctx, recorder, rbg, runtime, err)
		}

		effective, err := effectivePositions(role, runtime, vocabulary, defaults)
		if err != nil {
			return plan, failRef(ctx, recorder, rbg, runtime, err)
		}

		for _, position := range effective {
			plan.FragmentApplications[position] = append(
				plan.FragmentApplications[position],
				EngineRuntimeFragment{Profile: profile, Runtime: runtime},
			)
		}

		if len(runtime.PatchContainers) > 0 || len(runtime.ContainerPatches) > 0 {
			if runtime.Component == "" {
				// Defense in depth for the D7 CEL rule: render is the
				// authoritative gate, so it re-checks what write time
				// guarantees.
				return plan, failRef(ctx, recorder, rbg, runtime, fmt.Errorf(
					"role %q reference %q: patchContainers and containerPatches require component to be set",
					role.Name, runtime.ProfileName))
			}
		}

		if runtime.Component != "" {
			owners := componentOwners[runtime.ProfileName]
			if owners == nil {
				owners = map[string]struct{}{}
				componentOwners[runtime.ProfileName] = owners
			}
			if _, taken := owners[runtime.Component]; taken {
				return plan, failRef(ctx, recorder, rbg, runtime, fmt.Errorf(
					"role %q lists profileName %q twice in engineRuntimes naming the same component %q; "+
						"each component of a profile is targeted by exactly one reference — "+
						"merge the two references or publish profile variants",
					role.Name, runtime.ProfileName, runtime.Component))
			}
			owners[runtime.Component] = struct{}{}
		}

		if len(runtime.PatchContainers) == 0 {
			if len(profile.Spec.ContainerPatches) > 0 {
				emitEngineRuntimeWarning(ctx, recorder, rbg, ReasonUnaimedContainerPatch, fmt.Sprintf(
					"profile %q declares containerPatches but the reference lists no patchContainers; "+
						"the patches do not fire — set component and patchContainers on the reference to opt in",
					runtime.ProfileName))
			}
		} else {
			seen := make(map[string]struct{}, len(runtime.PatchContainers))
			for _, name := range runtime.PatchContainers {
				if _, dup := seen[name]; dup {
					return plan, failRef(ctx, recorder, rbg, runtime, fmt.Errorf(
						"role %q reference %q lists container %q twice in patchContainers",
						role.Name, runtime.ProfileName, name))
				}
				seen[name] = struct{}{}
			}
			if len(profile.Spec.ContainerPatches) > 0 {
				plan.PatchApplications[runtime.Component] = append(
					plan.PatchApplications[runtime.Component],
					EngineRuntimePatch{
						ProfileName: runtime.ProfileName,
						Patches:     profile.Spec.ContainerPatches,
						Targets:     runtime.PatchContainers,
					})
			}
		}

		if len(runtime.ContainerPatches) > 0 {
			declared := make(map[string]struct{},
				len(profile.Spec.InitContainers)+len(profile.Spec.Containers))
			for _, c := range profile.Spec.InitContainers {
				declared[c.Name] = struct{}{}
			}
			for _, c := range profile.Spec.Containers {
				declared[c.Name] = struct{}{}
			}
			for i, entry := range runtime.ContainerPatches {
				if _, ok := declared[entry.Container]; !ok {
					return plan, failRef(ctx, recorder, rbg, runtime, fmt.Errorf(
						"role %q reference %q containerPatches[%d] targets container %q, "+
							"which the profile does not declare",
						role.Name, runtime.ProfileName, i, entry.Container))
				}
				if entry.JSONPatch == nil {
					return plan, failRef(ctx, recorder, rbg, runtime, fmt.Errorf(
						"role %q reference %q containerPatches[%d]: no patch field is set; exactly one must be",
						role.Name, runtime.ProfileName, i))
				}
			}
			plan.RolePatchApplications[runtime.Component] = append(
				plan.RolePatchApplications[runtime.Component],
				EngineRuntimeRolePatch{
					ProfileName:        runtime.ProfileName,
					Patches:            runtime.ContainerPatches,
					DeclaredContainers: declared,
				})
		}
	}
	return plan, nil
}

// failRef emits the render-failed warning for a reference-scoped error and
// returns the error unchanged.
func failRef(
	ctx context.Context,
	recorder record.EventRecorder,
	rbg *workloadsv1alpha2.RoleBasedGroup,
	runtime workloadsv1alpha2.EngineRuntime,
	err error,
) error {
	emitEngineRuntimeWarning(ctx, recorder, rbg, ReasonEngineRuntimeRenderFailed,
		fmt.Sprintf("engine runtime %q render failed: %s", runtime.ProfileName, err.Error()))
	return err
}

// effectivePositions computes a reference's effective position set: the
// single named component when component is set, else the pattern default
// (every component — the compatibility expansion). The explicit form is
// validated against the vocabulary; a standalonePattern role defines none.
func effectivePositions(
	role *workloadsv1alpha2.RoleSpec,
	runtime workloadsv1alpha2.EngineRuntime,
	vocabulary map[string]struct{},
	defaults []string,
) ([]string, error) {
	if runtime.Component == "" {
		return defaults, nil
	}
	if len(vocabulary) == 0 {
		return nil, fmt.Errorf(
			"role %q reference %q names component %q, but a standalonePattern role has no component "+
				"vocabulary — leave component absent",
			role.Name, runtime.ProfileName, runtime.Component)
	}
	if _, legal := vocabulary[runtime.Component]; !legal {
		return nil, fmt.Errorf(
			"role %q reference %q names component %q, which the role's pattern does not define",
			role.Name, runtime.ProfileName, runtime.Component)
	}
	return []string{runtime.Component}, nil
}

// positionVocabulary returns the legal position names of the role's pattern,
// the render vocabulary (the ordered positions a plan's applications may key
// by — the vocabulary plus the standalone sentinel where the pattern defines
// none), and the default effective set used when component is absent — every
// component of the pattern (main's published behavior).
func positionVocabulary(role *workloadsv1alpha2.RoleSpec) (map[string]struct{}, []string, []string) {
	switch {
	case role.GetStandalonePattern() != nil:
		return map[string]struct{}{}, []string{StandalonePositionName}, []string{StandalonePositionName}
	case role.GetLeaderWorkerPattern() != nil:
		leader := string(constants.LeaderComponentType)
		worker := string(constants.WorkerComponentType)
		return map[string]struct{}{leader: {}, worker: {}}, []string{leader, worker}, []string{leader, worker}
	case role.GetCustomComponentsPattern() != nil:
		components := role.GetCustomComponentsPattern().Components
		vocabulary := make(map[string]struct{}, len(components))
		names := make([]string, 0, len(components))
		for _, component := range components {
			vocabulary[component.Name] = struct{}{}
			names = append(names, component.Name)
		}
		return vocabulary, names, names
	default:
		// The construct dispatch rejects a pattern-less role before the
		// resolver runs; the single implicit position is the least
		// surprising fallback for direct callers.
		return map[string]struct{}{}, []string{StandalonePositionName}, []string{StandalonePositionName}
	}
}

// emitEngineRuntimeWarning logs the warning and, when a recorder is set,
// emits a v1.EventTypeWarning event on the referencing RBG. Nil recorder is
// tolerated (log-only).
func emitEngineRuntimeWarning(
	ctx context.Context, recorder record.EventRecorder,
	rbg *workloadsv1alpha2.RoleBasedGroup, reason, message string,
) {
	log.FromContext(ctx).Info(message)
	if recorder != nil {
		recorder.Event(rbg, v1.EventTypeWarning, reason, message)
	}
}
