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
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/rbgs/api/workloads/constants"
	workloadsv1alpha2 "sigs.k8s.io/rbgs/api/workloads/v1alpha2"
)

// leaderPos and workerPos name the leaderWorkerPattern positions the same way
// the construct paths do.
func leaderPos() string { return string(constants.LeaderComponentType) }

func workerPos() string { return string(constants.WorkerComponentType) }

// renderSpec is the role-declared template one position's render passes
// mutate: a single engine container with a declared command, args and env.
func renderSpec() *v1.PodTemplateSpec {
	return &v1.PodTemplateSpec{Spec: v1.PodSpec{Containers: []v1.Container{{
		Name: "engine", Image: "sglang:latest",
		Command: []string{"python"}, Args: []string{"-m", "sglang.launch_server"},
		Env: []v1.EnvVar{{Name: "PYTHONPATH", Value: "/usr/lib/python"}},
	}}}}
}

// sidecarProfile builds a profile carrying one initContainer, one sidecar
// container (named name+"-sidecar") and the volume makeProfile provides.
func sidecarProfile(name string, patches []workloadsv1alpha2.ContainerPatch,
) *workloadsv1alpha2.ClusterEngineRuntimeProfile {
	profile := makeProfile(name, patches)
	profile.Spec.InitContainers = []v1.Container{{Name: name + "-init", Image: name + "-init:latest"}}
	profile.Spec.Containers = []v1.Container{{Name: name + "-sidecar", Image: name + "-sidecar:latest"}}
	return profile
}

func containerNames(spec *v1.PodTemplateSpec) []string {
	names := make([]string, 0, len(spec.Spec.Containers))
	for _, c := range spec.Spec.Containers {
		names = append(names, c.Name)
	}
	return names
}

func TestInjectEngineRuntimeFragments(t *testing.T) {
	// The render passes pull their logger from the context; the package's
	// null logger keeps V(1) skip logs out of the test output.
	ctx := context.Background()

	tests := []struct {
		name      string
		podSpec   *v1.PodTemplateSpec
		fragments []EngineRuntimeFragment
		verify    func(t *testing.T, spec *v1.PodTemplateSpec)
	}{
		{
			name:    "appends initContainers, containers and volumes when names are absent",
			podSpec: renderSpec(),
			fragments: []EngineRuntimeFragment{{
				Profile: sidecarProfile("ik", nil),
				Runtime: workloadsv1alpha2.EngineRuntime{ProfileName: "ik"},
			}},
			verify: func(t *testing.T, spec *v1.PodTemplateSpec) {
				require.Len(t, spec.Spec.InitContainers, 1)
				assert.Equal(t, "ik-init", spec.Spec.InitContainers[0].Name)
				assert.Equal(t, []string{"engine", "ik-sidecar"}, containerNames(spec))
				require.Len(t, spec.Spec.Volumes, 1)
				assert.Equal(t, "ik-vol", spec.Spec.Volumes[0].Name)
			},
		},
		{
			name:    "is idempotent per name",
			podSpec: renderSpec(),
			// Overlapping fragment delivery is legal per the KEP: two
			// references at one position carrying the same profile.
			fragments: []EngineRuntimeFragment{
				{Profile: sidecarProfile("ik", nil), Runtime: workloadsv1alpha2.EngineRuntime{ProfileName: "ik"}},
				{Profile: sidecarProfile("ik", nil), Runtime: workloadsv1alpha2.EngineRuntime{ProfileName: "ik"}},
			},
			verify: func(t *testing.T, spec *v1.PodTemplateSpec) {
				require.Len(t, spec.Spec.InitContainers, 1)
				assert.Equal(t, []string{"engine", "ik-sidecar"}, containerNames(spec))
				require.Len(t, spec.Spec.Volumes, 1)
			},
		},
		{
			name: "applies the role-side Containers override to profile containers present in the spec",
			podSpec: &v1.PodTemplateSpec{Spec: v1.PodSpec{Containers: []v1.Container{
				{Name: "engine", Image: "sglang:latest"},
				// collides with the profile-declared sidecar name: the
				// append-if-missing keeps the role's own container, and the
				// override applies to it.
				{Name: "ik-sidecar", Image: "role-sidecar:latest"},
			}}},
			fragments: []EngineRuntimeFragment{{
				Profile: sidecarProfile("ik", nil),
				Runtime: workloadsv1alpha2.EngineRuntime{
					ProfileName: "ik",
					Containers: []v1.Container{{
						Name: "ik-sidecar",
						Env:  []v1.EnvVar{{Name: "IK_SITE", Value: "/opt/ik/site"}},
						Args: []string{"--ik-flag"},
					}},
				},
			}},
			verify: func(t *testing.T, spec *v1.PodTemplateSpec) {
				require.Len(t, spec.Spec.Containers, 2)
				sidecar := spec.Spec.Containers[1]
				assert.Equal(t, "role-sidecar:latest", sidecar.Image)
				require.Len(t, sidecar.Env, 1)
				assert.Equal(t, v1.EnvVar{Name: "IK_SITE", Value: "/opt/ik/site"}, sidecar.Env[0])
				assert.Equal(t, []string{"--ik-flag"}, sidecar.Args)
			},
		},
		{
			name: "applies the role-side Containers override to a profile-injected container",
			podSpec: &v1.PodTemplateSpec{Spec: v1.PodSpec{
				Containers: []v1.Container{{Name: "engine", Image: "sglang:latest"}},
			}},
			fragments: []EngineRuntimeFragment{{
				Profile: sidecarProfile("ik", nil),
				Runtime: workloadsv1alpha2.EngineRuntime{
					ProfileName: "ik",
					Containers: []v1.Container{{
						Name: "ik-sidecar",
						Env:  []v1.EnvVar{{Name: "IK_SITE", Value: "/opt/ik/site"}},
						Args: []string{"--ik-flag"},
					}},
				},
			}},
			verify: func(t *testing.T, spec *v1.PodTemplateSpec) {
				assert.Equal(t, []string{"engine", "ik-sidecar"}, containerNames(spec))
				sidecar := spec.Spec.Containers[1]
				require.Len(t, sidecar.Env, 1)
				assert.Equal(t, v1.EnvVar{Name: "IK_SITE", Value: "/opt/ik/site"}, sidecar.Env[0])
				assert.Equal(t, []string{"--ik-flag"}, sidecar.Args)
			},
		},
		{
			name:    "skips the override for names the profile does not declare",
			podSpec: renderSpec(),
			fragments: []EngineRuntimeFragment{{
				Profile: sidecarProfile("ik", nil),
				Runtime: workloadsv1alpha2.EngineRuntime{
					ProfileName: "ik",
					Containers: []v1.Container{{
						Name: "ghost",
						Env:  []v1.EnvVar{{Name: "GHOST_ENV", Value: "x"}},
						Args: []string{"--ghost"},
					}},
				},
			}},
			verify: func(t *testing.T, spec *v1.PodTemplateSpec) {
				// no ghost container appeared and no existing one gained its values
				assert.Equal(t, []string{"engine", "ik-sidecar"}, containerNames(spec))
				for _, c := range spec.Spec.Containers {
					for _, env := range c.Env {
						assert.NotEqual(t, "GHOST_ENV", env.Name,
							"container %q must not gain the skipped override env", c.Name)
					}
					for _, arg := range c.Args {
						assert.NotEqual(t, "--ghost", arg,
							"container %q must not gain the skipped override args", c.Name)
					}
				}
			},
		},
		{
			name:    "applies fragments of two profiles in reference order",
			podSpec: renderSpec(),
			fragments: []EngineRuntimeFragment{
				{Profile: sidecarProfile("a", nil), Runtime: workloadsv1alpha2.EngineRuntime{ProfileName: "a"}},
				{Profile: sidecarProfile("b", nil), Runtime: workloadsv1alpha2.EngineRuntime{ProfileName: "b"}},
			},
			verify: func(t *testing.T, spec *v1.PodTemplateSpec) {
				assert.Equal(t, []string{"engine", "a-sidecar", "b-sidecar"}, containerNames(spec))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			InjectEngineRuntimeFragments(ctx, tt.podSpec, tt.fragments)
			tt.verify(t, tt.podSpec)
		})
	}
}

func TestApplyEngineRuntimePatches(t *testing.T) {
	ctx := context.Background()

	t.Run("applies each entry's patches to its targets in entry order", func(t *testing.T) {
		spec := renderSpec()
		entries := []EngineRuntimePatch{
			{
				ProfileName: "one", Targets: []string{"engine"},
				Patches: []workloadsv1alpha2.ContainerPatch{envPatch("IK_ONE", "1")},
			},
			{
				ProfileName: "two", Targets: []string{"engine"},
				Patches: []workloadsv1alpha2.ContainerPatch{envPatch("IK_TWO", "2")},
			},
		}
		_, err := ApplyEngineRuntimePatches(ctx, spec, roleContainers("engine"), entries)
		require.NoError(t, err)
		// both profiles' env vars are present, in the plan's entry order
		env := spec.Spec.Containers[0].Env
		require.Len(t, env, 3)
		assert.Equal(t, []string{"PYTHONPATH", "IK_ONE", "IK_TWO"},
			[]string{env[0].Name, env[1].Name, env[2].Name})
	})

	t.Run("a later entry sees the mutations of earlier ones", func(t *testing.T) {
		spec := renderSpec()
		entries := []EngineRuntimePatch{
			{
				ProfileName: "one", Targets: []string{"engine"},
				Patches: []workloadsv1alpha2.ContainerPatch{envPatch("IK_A", "1")},
			},
			{
				ProfileName: "two", Targets: []string{"engine"},
				Patches: []workloadsv1alpha2.ContainerPatch{envPatch("IK_A", "2")},
			},
		}
		_, err := ApplyEngineRuntimePatches(ctx, spec, roleContainers("engine"), entries)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "add cannot join values")
		assert.Contains(t, err.Error(), `profile "two"`)
	})

	t.Run("a target absent from roleContainerNames is a render error", func(t *testing.T) {
		spec := renderSpec()
		entries := []EngineRuntimePatch{{
			ProfileName: "ik", Targets: []string{"sidecar"},
			Patches: []workloadsv1alpha2.ContainerPatch{envPatch("IK_SITE", "/opt/ik/site")},
		}}
		_, err := ApplyEngineRuntimePatches(ctx, spec, roleContainers("engine"), entries)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a role-declared")
		assert.Contains(t, err.Error(), `"sidecar"`)
	})

	t.Run("multi-profile /command/0 head-inserts stack outermost-last", func(t *testing.T) {
		// Re-homes the phase-1 multi-runtime ordering pin: two patch-carrying
		// references to different profiles at one position, both head-inserting
		// on /command/0 — the later-listed entry's insert lands outermost.
		spec := renderSpec() // engine declares command ["python"]
		entries := []EngineRuntimePatch{
			{
				ProfileName: "a", Targets: []string{"engine"},
				Patches: []workloadsv1alpha2.ContainerPatch{jp("/command/0", "wrap-a")},
			},
			{
				ProfileName: "b", Targets: []string{"engine"},
				Patches: []workloadsv1alpha2.ContainerPatch{jp("/command/0", "wrap-b")},
			},
		}
		_, err := ApplyEngineRuntimePatches(ctx, spec, roleContainers("engine"), entries)
		require.NoError(t, err)
		// list order is outermost-last: a's insert lands first, b's wraps it
		assert.Equal(t, []string{"wrap-b", "wrap-a", "python"}, spec.Spec.Containers[0].Command)
	})

	t.Run("an empty entry list is a no-op", func(t *testing.T) {
		spec := renderSpec()
		before := spec.DeepCopy()
		_, err := ApplyEngineRuntimePatches(ctx, spec, roleContainers("engine"), nil)
		require.NoError(t, err)
		assert.True(t, reflect.DeepEqual(before.Spec, spec.Spec),
			"an empty entry list must not mutate the spec")
	})
}

// TestEngineRuntimeFullRender exercises the two plan-driven passes over the
// full fragment and patch vocabulary: fragments first (they provide the
// mounted volume), then the patch entries, against the role-container names
// captured before any fragment injection.
func TestEngineRuntimeFullRender(t *testing.T) {
	ctx := context.Background()

	profile := makeProfile("ik", []workloadsv1alpha2.ContainerPatch{
		// Multi-element command prepends are written in reverse order: every
		// /command/0 head-insert lands before the previous op's element.
		jp("/command/0", "exec"),
		jp("/command/0", "--"),
		jp("/command/0", "/opt/ik/wrap"),
		jp("/args/0", "--ik-flag"),
		jp("/env/-", v1.EnvVar{Name: "IK_SITE", Value: "/opt/ik/site"}),
		jp("/volumeMounts/-", v1.VolumeMount{Name: "ik-vol", MountPath: "/opt/ik", ReadOnly: true}),
		jp("/securityContext/capabilities/add/-", "CAP_SYS_RESOURCE"),
	})
	fragments := []EngineRuntimeFragment{{
		Profile: profile,
		Runtime: workloadsv1alpha2.EngineRuntime{ProfileName: "ik"},
	}}
	entries := []EngineRuntimePatch{{
		ProfileName: "ik",
		Patches:     profile.Spec.ContainerPatches,
		Targets:     []string{"engine"},
	}}

	spec := renderSpec()
	roleContainerNames := roleContainers("engine") // captured before injection

	InjectEngineRuntimeFragments(ctx, spec, fragments)
	_, err := ApplyEngineRuntimePatches(ctx, spec, roleContainerNames, entries)
	require.NoError(t, err)

	golden := &v1.PodTemplateSpec{Spec: v1.PodSpec{
		Containers: []v1.Container{{
			Name:    "engine",
			Image:   "sglang:latest",
			Command: []string{"/opt/ik/wrap", "--", "exec", "python"},
			Args:    []string{"--ik-flag", "-m", "sglang.launch_server"},
			Env: []v1.EnvVar{
				{Name: "PYTHONPATH", Value: "/usr/lib/python"},
				{Name: "IK_SITE", Value: "/opt/ik/site"},
			},
			VolumeMounts: []v1.VolumeMount{{Name: "ik-vol", MountPath: "/opt/ik", ReadOnly: true}},
			SecurityContext: &v1.SecurityContext{Capabilities: &v1.Capabilities{
				Add: []v1.Capability{"SYS_RESOURCE"},
			}},
		}},
		// the fragment pass injected the profile volume the mount references
		Volumes: []v1.Volume{{Name: "ik-vol", VolumeSource: v1.VolumeSource{
			EmptyDir: &v1.EmptyDirVolumeSource{},
		}}},
	}}
	assert.True(t, reflect.DeepEqual(golden.Spec, spec.Spec),
		"golden mismatch:\n got: %+v\nwant: %+v", spec.Spec, golden.Spec)
}

// volumeNamesOf returns the rendered spec's volume names in order.
func volumeNamesOf(spec *v1.PodTemplateSpec) []string {
	names := make([]string, 0, len(spec.Spec.Volumes))
	for _, vol := range spec.Spec.Volumes {
		names = append(names, vol.Name)
	}
	return names
}

// renderResolvedPosition runs the PodReconciler render flow for one position:
// the role's own container, init container and volume names captured before
// any injection, then the fragment pass, then the profile-side patch pass,
// then the role-side patch pass — the KEP-531 application order, role-side
// last — over a copy of the position's role-declared template.
func renderResolvedPosition(
	t *testing.T,
	plan *EngineRuntimePlan,
	position string,
	template v1.PodTemplateSpec,
) (*v1.PodTemplateSpec, error) {
	t.Helper()
	spec := template.DeepCopy()
	roleContainerNames := make(map[string]struct{}, len(spec.Spec.Containers))
	for _, c := range spec.Spec.Containers {
		roleContainerNames[c.Name] = struct{}{}
	}
	roleInitContainerNames := make(map[string]struct{}, len(spec.Spec.InitContainers))
	for _, c := range spec.Spec.InitContainers {
		roleInitContainerNames[c.Name] = struct{}{}
	}
	roleVolumeNames := make(map[string]struct{}, len(spec.Spec.Volumes))
	for _, vol := range spec.Spec.Volumes {
		roleVolumeNames[vol.Name] = struct{}{}
	}
	ctx := context.Background()
	InjectEngineRuntimeFragments(ctx, spec, plan.FragmentApplications[position])
	profilePositionalInserts, err := ApplyEngineRuntimePatches(
		ctx, spec, roleContainerNames, plan.PatchApplications[position])
	if err != nil {
		return spec, err
	}
	_, err = ApplyRoleEngineRuntimePatches(
		ctx, spec, roleContainerNames, roleInitContainerNames, roleVolumeNames,
		plan.RolePatchApplications[position], plan.RoleName, profilePositionalInserts,
	)
	return spec, err
}

// TestEngineRuntimeCrossProfileVolumeVisibility pins the KEP test-plan row
// "cross-profile volume reference (any list position) → renders, all
// same-position fragments precede the patch pass": the earlier-listed
// reference's patch mounts a volume declared only by the later-listed
// reference's fragments. Under the same-position two-pass merge this must
// render successfully.
func TestEngineRuntimeCrossProfileVolumeVisibility(t *testing.T) {
	bProfile := makeProfile("b", nil) // declares b-vol, no patches
	aProfile := makeProfile("a", []workloadsv1alpha2.ContainerPatch{
		jp("/volumeMounts/-", v1.VolumeMount{Name: "b-vol", MountPath: "/opt/b", ReadOnly: true}),
	})
	role := leaderWorkerRole([]workloadsv1alpha2.EngineRuntime{
		// the patch-carrying reference consents at its own component
		{ProfileName: "a", Component: leaderPos(), PatchContainers: []string{"engine"}},
		{ProfileName: "b"},
	})
	plan, err, recorder := resolverHarness(
		t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{aProfile, bProfile}, role,
	)
	require.NoError(t, err)
	assert.Empty(t, collectEvents(t, recorder))

	template := *role.GetLeaderWorkerPattern().TemplateSource.Template
	spec, err := renderResolvedPosition(t, plan, leaderPos(), template)
	require.NoError(t, err, "the earlier reference's patch may mount the later reference's volume")

	// both references' fragments landed before the patch pass ran
	assert.Equal(t, []string{"a-vol", "b-vol"}, volumeNamesOf(spec))
	engine := spec.Spec.Containers[0]
	require.Len(t, engine.VolumeMounts, 1)
	assert.Equal(t, "b-vol", engine.VolumeMounts[0].Name)
	assert.Equal(t, "/opt/b", engine.VolumeMounts[0].MountPath)
}

// TestEngineRuntimePatchPassOrder pins the KEP-531 D10 pass order at the two
// points the discovery layer can observe it. With both patch passes failing,
// the error of the pass that runs first surfaces — the profile-side pass
// (stage 3) before the role-side pass (stage 4, last). And with both passes
// succeeding on the same role-declared container — the direct overlap D8's
// target vocabulary admits — the role-side mutations land last.
// renderResolvedPosition mirrors the PodReconciler pipeline's pass order, so
// the surfaced attribution and the landed order both pin that order directly.
func TestEngineRuntimePatchPassOrder(t *testing.T) {
	t.Run("with both patch passes failing, the profile-side error surfaces first", func(t *testing.T) {
		// Both sides collide with content the fragment pass already rendered:
		// the role's legacy merge appended ROLE_ENV to the role-owned engine,
		// which the profile-side patch re-adds, and the profile declares ROLE_X
		// on its sidecar, which the role-side patch re-adds.
		profile := makeProfile("ik", []workloadsv1alpha2.ContainerPatch{
			envPatch("ROLE_ENV", "profile-side"),
		})
		profile.Spec.Containers = []v1.Container{
			{Name: "runtime-sidecar", Env: []v1.EnvVar{{Name: "ROLE_X", Value: "profile"}}},
			{Name: "engine"}, // declared so the legacy merge reaches the role-owned engine
		}
		role := leaderWorkerRole([]workloadsv1alpha2.EngineRuntime{{
			ProfileName:     "ik",
			Component:       leaderPos(),
			PatchContainers: []string{"engine"},
			Containers: []v1.Container{{
				Name: "engine",
				Env:  []v1.EnvVar{{Name: "ROLE_ENV", Value: "role"}},
			}},
			ContainerPatches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("runtime-sidecar", "/env/-", v1.EnvVar{Name: "ROLE_X", Value: "role"}),
			},
		}})
		plan, err, recorder := resolverHarness(
			t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role,
		)
		require.NoError(t, err)
		assert.Empty(t, collectEvents(t, recorder))

		template := *role.GetLeaderWorkerPattern().TemplateSource.Template
		_, err = renderResolvedPosition(t, plan, leaderPos(), template)
		require.Error(t, err)
		// the profile-side pass ran first: its collision surfaces, not the
		// role-side one
		assert.Contains(t, err.Error(), `profile "ik" containerPatches[0]`)
		assert.NotContains(t, err.Error(), `role "worker" engineRuntimes`)
	})

	t.Run("both patch passes on one role-declared container: the role-side result lands last", func(t *testing.T) {
		// The direct overlap the failing-pass pin above cannot express: D8
		// lets the role-side pass target the same role-declared container the
		// profile-side pass patches, and D10 decides the outcome — the
		// role-side pass runs last, so its /command/0 insert lands ahead of
		// (wrapping) the profile's and its env append lands after it.
		profile := makeProfile("ik", []workloadsv1alpha2.ContainerPatch{
			jp("/command/0", "profile-wrap"),
			envPatch("PROF_ENV", "profile-side"),
		})
		role := leaderWorkerRole([]workloadsv1alpha2.EngineRuntime{{
			ProfileName:     "ik",
			Component:       leaderPos(),
			PatchContainers: []string{"engine"},
			ContainerPatches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("engine", "/command/0", "role-wrap"),
				roleJp("engine", "/env/-", v1.EnvVar{Name: "ROLE_ENV", Value: "role-side"}),
			},
		}})
		plan, err, recorder := resolverHarness(
			t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role,
		)
		require.NoError(t, err)
		assert.Empty(t, collectEvents(t, recorder))

		template := *role.GetLeaderWorkerPattern().TemplateSource.Template
		template.Spec.Containers[0].Command = []string{"python"}
		spec, err := renderResolvedPosition(t, plan, leaderPos(), template)
		require.NoError(t, err)

		engine := spec.Spec.Containers[0]
		// the role-side insert is outermost: it ran after the profile-side
		// insert and wrapped it
		assert.Equal(t, []string{"role-wrap", "profile-wrap", "python"}, engine.Command)
		// the role-side env append landed after the profile's
		assert.Equal(t, []string{"PROF_ENV", "ROLE_ENV"}, envNames(engine))
	})
}

// TestEngineRuntimePositionalShadowing pins the KEP-531 D10 second clause:
// a role-side positional insert landing ahead of a profile-side insert on
// the same container is reported, never failed — the role-side pass runs
// last, so its insert lands at the written index and shifts profile-side
// content at or after it. The profile-side pass records the containers its
// positional inserts touched, the role-side pass reports the subset its own
// positional inserts shadow, and the render succeeds either way — the
// shadowing must never be silent.
func TestEngineRuntimePositionalShadowing(t *testing.T) {
	ctx := context.Background()

	t.Run("a role-side /command/0 ahead of a profile-side /command/1 reports the container", func(t *testing.T) {
		spec := renderSpec() // engine declares command ["python"]
		roleContainerNames := roleContainers("engine")

		profileEntries := []EngineRuntimePatch{{
			ProfileName: "ik",
			Targets:     []string{"engine"},
			Patches:     []workloadsv1alpha2.ContainerPatch{jp("/command/1", "profile-wrap")},
		}}
		roleEntries := []EngineRuntimeRolePatch{{
			ProfileName:        "ik",
			DeclaredContainers: map[string]struct{}{},
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("engine", "/command/0", "role-wrap"),
			},
		}}

		profilePositionalInserts, err := ApplyEngineRuntimePatches(ctx, spec, roleContainerNames, profileEntries)
		require.NoError(t, err)
		assert.Contains(t, profilePositionalInserts, "engine",
			"the profile-side pass records the container its positional insert touched")

		shadowed, err := ApplyRoleEngineRuntimePatches(
			ctx, spec, roleContainerNames, nil, nil, roleEntries, "worker", profilePositionalInserts)
		require.NoError(t, err, "positional shadowing emits a render-time event, never a failure")
		assert.Contains(t, shadowed, "engine",
			"the shadow report names the container whose profile-side insert was landed ahead of")

		// the render succeeded: the role-side insert is the rendered
		// command's index 0, ahead of the profile-side insert
		assert.Equal(t, []string{"role-wrap", "python", "profile-wrap"}, spec.Spec.Containers[0].Command)
	})

	t.Run("a profile-side append records nothing, so nothing can shadow", func(t *testing.T) {
		// appends land after all existing content and shadow nothing; only
		// when both sides claim absolute positions on the same container can
		// the role-side insert land ahead of the profile-side one
		spec := renderSpec()
		roleContainerNames := roleContainers("engine")
		profileEntries := []EngineRuntimePatch{{
			ProfileName: "ik",
			Targets:     []string{"engine"},
			Patches:     []workloadsv1alpha2.ContainerPatch{jp("/command/-", "profile-wrap")},
		}}
		roleEntries := []EngineRuntimeRolePatch{{
			ProfileName:        "ik",
			DeclaredContainers: map[string]struct{}{},
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("engine", "/command/0", "role-wrap"),
			},
		}}

		profilePositionalInserts, err := ApplyEngineRuntimePatches(ctx, spec, roleContainerNames, profileEntries)
		require.NoError(t, err)
		assert.Empty(t, profilePositionalInserts, "an append is not a positional insert")

		shadowed, err := ApplyRoleEngineRuntimePatches(
			ctx, spec, roleContainerNames, nil, nil, roleEntries, "worker", profilePositionalInserts)
		require.NoError(t, err)
		assert.Empty(t, shadowed)
	})

	t.Run("a role-side append reports nothing even over a profile-side insert", func(t *testing.T) {
		spec := renderSpec()
		roleContainerNames := roleContainers("engine")
		profileEntries := []EngineRuntimePatch{{
			ProfileName: "ik",
			Targets:     []string{"engine"},
			Patches:     []workloadsv1alpha2.ContainerPatch{jp("/command/1", "profile-wrap")},
		}}
		roleEntries := []EngineRuntimeRolePatch{{
			ProfileName:        "ik",
			DeclaredContainers: map[string]struct{}{},
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("engine", "/command/-", "role-wrap"),
			},
		}}

		profilePositionalInserts, err := ApplyEngineRuntimePatches(ctx, spec, roleContainerNames, profileEntries)
		require.NoError(t, err)
		assert.Contains(t, profilePositionalInserts, "engine")

		shadowed, err := ApplyRoleEngineRuntimePatches(
			ctx, spec, roleContainerNames, nil, nil, roleEntries, "worker", profilePositionalInserts)
		require.NoError(t, err)
		assert.Empty(t, shadowed, "a role-side append lands after everything, ahead of nothing")
	})

	t.Run("positional inserts on different containers report nothing", func(t *testing.T) {
		spec := renderSpec()
		spec.Spec.Containers = append(spec.Spec.Containers, v1.Container{
			Name: "engine2", Image: "sglang:latest", Command: []string{"python"},
		})
		roleContainerNames := roleContainers("engine", "engine2")

		profileEntries := []EngineRuntimePatch{{
			ProfileName: "ik",
			Targets:     []string{"engine"},
			Patches:     []workloadsv1alpha2.ContainerPatch{jp("/command/1", "profile-wrap")},
		}}
		roleEntries := []EngineRuntimeRolePatch{{
			ProfileName:        "ik",
			DeclaredContainers: map[string]struct{}{},
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("engine2", "/command/0", "role-wrap"),
			},
		}}

		profilePositionalInserts, err := ApplyEngineRuntimePatches(ctx, spec, roleContainerNames, profileEntries)
		require.NoError(t, err)
		shadowed, err := ApplyRoleEngineRuntimePatches(
			ctx, spec, roleContainerNames, nil, nil, roleEntries, "worker", profilePositionalInserts)
		require.NoError(t, err)
		assert.Empty(t, shadowed,
			"the role-side insert landed on a container the profile-side pass never touched positionally")
	})
}

// TestEngineRuntimeSplitDeliveryDanglingMount pins the KEP test-plan row
// "split delivery: a /volumeMounts patch qualified to a position whose
// fragments (and volume) go elsewhere → dangling-mount render error". The
// reworked reference cannot split its own fragments and patches across
// positions — component scopes both — so the split rides two references:
// the patch-carrying reference fires on the leader while the volume-declaring
// profile's fragments go to the worker, and the leader's patch mount
// references a volume the leader never received.
func TestEngineRuntimeSplitDeliveryDanglingMount(t *testing.T) {
	aProfile := makeProfile("a", []workloadsv1alpha2.ContainerPatch{
		jp("/volumeMounts/-", v1.VolumeMount{Name: "b-vol", MountPath: "/opt/b", ReadOnly: true}),
	})
	bProfile := makeProfile("b", nil) // declares b-vol
	role := leaderWorkerRole([]workloadsv1alpha2.EngineRuntime{
		{ProfileName: "a", Component: leaderPos(), PatchContainers: []string{"engine"}},
		{ProfileName: "b", Component: workerPos()},
	})
	plan, err, recorder := resolverHarness(
		t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{aProfile, bProfile}, role,
	)
	require.NoError(t, err)
	assert.Empty(t, collectEvents(t, recorder))

	// the worker received the volume and carries no patch: it renders clean
	template := *role.GetLeaderWorkerPattern().TemplateSource.Template
	workerSpec, err := renderResolvedPosition(t, plan, workerPos(), template)
	require.NoError(t, err)
	assert.Equal(t, []string{"b-vol"}, volumeNamesOf(workerSpec))
	assert.Empty(t, plan.PatchApplications[workerPos()])

	// the leader carries the patch but not the volume: dangling-mount error
	_, err = renderResolvedPosition(t, plan, leaderPos(), template)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "b-vol")
	assert.Contains(t, err.Error(), "declared by neither the rendered spec nor the profile")
}

// heterogeneousComponentsRole builds a customComponents role whose
// engine-comp template declares an engine container and whose aux-comp
// template does not — the shape the "target presence scoped to addressed
// positions" test-plan row requires.
func heterogeneousComponentsRole(runtimes []workloadsv1alpha2.EngineRuntime) *workloadsv1alpha2.RoleSpec {
	return &workloadsv1alpha2.RoleSpec{
		Name: "worker",
		Pattern: workloadsv1alpha2.Pattern{
			CustomComponentsPattern: &workloadsv1alpha2.CustomComponentsPattern{
				Components: []workloadsv1alpha2.InstanceComponent{
					{
						Name: "engine-comp", Size: ptr.To(int32(1)),
						Template: v1.PodTemplateSpec{Spec: v1.PodSpec{
							Containers: []v1.Container{{Name: "engine", Image: "sglang:latest"}},
						}},
					},
					{
						Name: "aux-comp", Size: ptr.To(int32(1)),
						Template: v1.PodTemplateSpec{Spec: v1.PodSpec{
							Containers: []v1.Container{{Name: "aux", Image: "aux:latest"}},
						}},
					},
				},
			},
		},
		EngineRuntimes: runtimes,
	}
}

// TestEngineRuntimeTargetPresenceScopedToAddressedPositions pins the KEP
// test-plan row "target presence scoped to addressed positions" — the
// phase-2 fix for the KEP's customComponents defect 2, reworked for the
// named-component reference: only the component a reference addresses sees
// its patch applications, and a component lacking the named container only
// errors when the reference addresses it.
func TestEngineRuntimeTargetPresenceScopedToAddressedPositions(t *testing.T) {
	profile := makeProfile("ik", []workloadsv1alpha2.ContainerPatch{envPatch("IK_SITE", "/opt/ik/site")})

	t.Run("a reference addressing the component lacking the container is a render error", func(t *testing.T) {
		role := heterogeneousComponentsRole([]workloadsv1alpha2.EngineRuntime{
			{ProfileName: "ik", Component: "aux-comp", PatchContainers: []string{"engine"}},
		})
		plan, err, recorder := resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.NoError(t, err)
		assert.Empty(t, collectEvents(t, recorder))
		// the patch routed to the addressed component only
		require.Len(t, plan.PatchApplications["aux-comp"], 1)
		assert.NotContains(t, plan.PatchApplications, "engine-comp")

		components := role.GetCustomComponentsPattern().Components

		// the addressed component that lacks the container is a render error
		_, err = renderResolvedPosition(t, plan, "aux-comp", components[1].Template)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"engine"`)
		assert.Contains(t, err.Error(), "not a role-declared spec.containers member")
	})

	t.Run("a reference addressing only the component that has the container renders with no error", func(t *testing.T) {
		role := heterogeneousComponentsRole([]workloadsv1alpha2.EngineRuntime{
			{ProfileName: "ik", Component: "engine-comp", PatchContainers: []string{"engine"}},
		})
		plan, err, recorder := resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.NoError(t, err)
		assert.Empty(t, collectEvents(t, recorder))
		// the patch routes to the addressed component only
		require.Len(t, plan.PatchApplications, 1)
		require.Len(t, plan.PatchApplications["engine-comp"], 1)
		assert.NotContains(t, plan.PatchApplications, "aux-comp")

		components := role.GetCustomComponentsPattern().Components

		// the unaddressed component renders with no error (narrowed scope)
		spec, err := renderResolvedPosition(t, plan, "aux-comp", components[1].Template)
		require.NoError(t, err)
		assert.Equal(t, []string{"aux"}, containerNames(spec))
		assert.Empty(t, spec.Spec.Containers[0].Env)

		// the addressed one renders the patch
		spec, err = renderResolvedPosition(t, plan, "engine-comp", components[0].Template)
		require.NoError(t, err)
		require.Len(t, spec.Spec.Containers[0].Env, 1)
		assert.Equal(t, "IK_SITE", spec.Spec.Containers[0].Env[0].Name)
	})
}

// TestEngineRuntimeTargetingBoundary re-homes the phase-1 cross-profile
// targeting pins: profile-injected containers are never addressable, and a
// profile-declared name colliding with a role-declared container resolves to
// the role's own container.
func TestEngineRuntimeTargetingBoundary(t *testing.T) {
	ctx := context.Background()

	t.Run("a patch aimed at a profile-injected container is a render error", func(t *testing.T) {
		spec := renderSpec()
		roleContainerNames := roleContainers("engine") // captured before injection
		InjectEngineRuntimeFragments(ctx, spec, []EngineRuntimeFragment{{
			Profile: sidecarProfile("ik", nil),
			Runtime: workloadsv1alpha2.EngineRuntime{ProfileName: "ik"},
		}})
		// the sidecar the profile itself injected is present in the rendered spec...
		assert.Equal(t, []string{"engine", "ik-sidecar"}, containerNames(spec))

		// ...but is not addressable: the patch pass rejects it
		entries := []EngineRuntimePatch{{
			ProfileName: "ik", Targets: []string{"ik-sidecar"},
			Patches: []workloadsv1alpha2.ContainerPatch{envPatch("IK_SITE", "/opt/ik/site")},
		}}
		_, err := ApplyEngineRuntimePatches(ctx, spec, roleContainerNames, entries)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"ik-sidecar"`)
		assert.Contains(t, err.Error(), "not a role-declared spec.containers member")
		// the injected sidecar gained nothing
		assert.Empty(t, spec.Spec.Containers[1].Env)
	})

	t.Run("a profile-declared name colliding with a role-declared container resolves to the role's own", func(t *testing.T) {
		spec := &v1.PodTemplateSpec{Spec: v1.PodSpec{Containers: []v1.Container{
			{Name: "engine", Image: "sglang:latest"},
			// the role declares its own ik-sidecar, colliding with the
			// profile-declared sidecar name
			{Name: "ik-sidecar", Image: "role-sidecar:latest"},
		}}}
		roleContainerNames := roleContainers("engine", "ik-sidecar") // captured before injection
		InjectEngineRuntimeFragments(ctx, spec, []EngineRuntimeFragment{{
			Profile: sidecarProfile("ik", nil),
			Runtime: workloadsv1alpha2.EngineRuntime{ProfileName: "ik"},
		}})
		// append-if-missing kept the role's own container
		require.Len(t, spec.Spec.Containers, 2)
		assert.Equal(t, "role-sidecar:latest", spec.Spec.Containers[1].Image)

		// the address resolves to the role's own container and applies legally
		entries := []EngineRuntimePatch{{
			ProfileName: "ik", Targets: []string{"ik-sidecar"},
			Patches: []workloadsv1alpha2.ContainerPatch{envPatch("IK_SITE", "/opt/ik/site")},
		}}
		_, err := ApplyEngineRuntimePatches(ctx, spec, roleContainerNames, entries)
		require.NoError(t, err)
		sidecar := spec.Spec.Containers[1]
		assert.Equal(t, "role-sidecar:latest", sidecar.Image)
		require.Len(t, sidecar.Env, 1)
		assert.Equal(t, "IK_SITE", sidecar.Env[0].Name)
	})
}

// roleJp builds one role-side containerPatches entry — a named target plus
// an add-op jsonPatch — from a path and a Go value.
func roleJp(container, path string, value any) workloadsv1alpha2.EngineRuntimeContainerPatch {
	patch := jp(path, value)
	return workloadsv1alpha2.EngineRuntimeContainerPatch{Container: container, JSONPatch: patch.JSONPatch}
}

// declaredContainersOf mirrors the resolver's capture of one profile's
// declared container names (init containers and sidecars) — the
// profile-declared half of a role-side patch entry's target vocabulary.
func declaredContainersOf(profile *workloadsv1alpha2.ClusterEngineRuntimeProfile) map[string]struct{} {
	declared := make(map[string]struct{},
		len(profile.Spec.InitContainers)+len(profile.Spec.Containers))
	for _, c := range profile.Spec.InitContainers {
		declared[c.Name] = struct{}{}
	}
	for _, c := range profile.Spec.Containers {
		declared[c.Name] = struct{}{}
	}
	return declared
}

// envNames returns a container's env var names, in list order.
func envNames(c v1.Container) []string {
	names := make([]string, 0, len(c.Env))
	for _, e := range c.Env {
		names = append(names, e.Name)
	}
	return names
}

// TestApplyRoleEngineRuntimePatches pins the KEP-531 role-side patch pass:
// its place in the application order (after the profile-side pass, last) and
// the D8 target resolution — a target may be role-declared (a spec.containers
// or spec.initContainers member of the role's own template, no consent
// involved) or declared by this reference's own profile, and a name the
// role's own template also declares resolves to the role-owned container —
// the same precedence injection applies. Errors attribute to the role's
// reference, never the profile.
func TestApplyRoleEngineRuntimePatches(t *testing.T) {
	ctx := context.Background()

	// renderProfile declares the container vocabulary role-side patches
	// address: an init container runtime-init (args declared so /args inserts
	// are legal) and a sidecar runtime-sidecar.
	renderProfile := func(sidecarEnv ...v1.EnvVar) *workloadsv1alpha2.ClusterEngineRuntimeProfile {
		profile := declaringProfile("ik")
		profile.Spec.InitContainers[0].Args = []string{"--base"}
		profile.Spec.Containers[0].Env = sidecarEnv
		return profile
	}

	t.Run("role-side patches fire on profile-declared sidecars and init containers", func(t *testing.T) {
		spec := renderSpec()
		roleContainerNames := roleContainers("engine") // captured before injection
		profile := renderProfile()
		InjectEngineRuntimeFragments(ctx, spec, []EngineRuntimeFragment{{
			Profile: profile,
			Runtime: workloadsv1alpha2.EngineRuntime{ProfileName: "ik"},
		}})

		_, err := ApplyRoleEngineRuntimePatches(ctx, spec, roleContainerNames, nil, nil, []EngineRuntimeRolePatch{{
			ProfileName:        "ik",
			DeclaredContainers: declaredContainersOf(profile),
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("runtime-sidecar", "/env/-", v1.EnvVar{Name: "A", Value: "b"}),
				roleJp("runtime-init", "/args/-", "--flag"),
			},
		}}, "worker", nil)
		require.NoError(t, err)

		// the sidecar gains env A
		sidecar := findRenderedContainer(spec, "runtime-sidecar")
		require.NotNil(t, sidecar)
		require.Len(t, sidecar.Env, 1)
		assert.Equal(t, v1.EnvVar{Name: "A", Value: "b"}, sidecar.Env[0])
		// the init container gains the arg
		require.Len(t, spec.Spec.InitContainers, 1)
		assert.Equal(t, []string{"--base", "--flag"}, spec.Spec.InitContainers[0].Args)
	})

	t.Run("a role-declared container target applies with no consent involved", func(t *testing.T) {
		// D8: the reference is the single toggle for all profile-coupled
		// modification, so its patches may couple to the role's own
		// containers; patchContainers gates the profile's rules only.
		spec := renderSpec()
		roleContainerNames := roleContainers("engine") // captured before injection

		_, err := ApplyRoleEngineRuntimePatches(ctx, spec, roleContainerNames, nil, nil, []EngineRuntimeRolePatch{{
			ProfileName:        "ik",
			DeclaredContainers: map[string]struct{}{}, // the profile declares nothing
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("engine", "/env/-", v1.EnvVar{Name: "ROLE_X", Value: "b"}),
			},
		}}, "worker", nil)
		require.NoError(t, err)

		require.Len(t, spec.Spec.Containers, 1)
		assert.Equal(t, []string{"PYTHONPATH", "ROLE_X"}, envNames(spec.Spec.Containers[0]))
	})

	t.Run("a role-declared init container target applies", func(t *testing.T) {
		// the legacy containers merge cannot reach init containers — the
		// patch grammar closes that gap
		spec := renderSpec()
		spec.Spec.InitContainers = []v1.Container{{
			Name: "role-init", Image: "role-init:latest", Args: []string{"--base"},
		}}
		roleContainerNames := roleContainers("engine")
		roleInitContainerNames := roleContainers("role-init")

		_, err := ApplyRoleEngineRuntimePatches(ctx, spec, roleContainerNames, roleInitContainerNames, nil, []EngineRuntimeRolePatch{{
			ProfileName:        "ik",
			DeclaredContainers: map[string]struct{}{},
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("role-init", "/args/-", "--flag"),
			},
		}}, "worker", nil)
		require.NoError(t, err)

		require.Len(t, spec.Spec.InitContainers, 1)
		assert.Equal(t, []string{"--base", "--flag"}, spec.Spec.InitContainers[0].Args)
	})

	t.Run("a name declared by both the role template and the profile resolves to the role-owned container", func(t *testing.T) {
		profile := renderProfile()
		// the profile also declares a sidecar colliding with the role's engine
		profile.Spec.Containers = append(profile.Spec.Containers, v1.Container{Name: "engine", Image: "profile-engine:latest"})
		spec := renderSpec()
		roleContainerNames := roleContainers("engine") // captured before injection
		InjectEngineRuntimeFragments(ctx, spec, []EngineRuntimeFragment{{
			Profile: profile,
			Runtime: workloadsv1alpha2.EngineRuntime{ProfileName: "ik"},
		}})
		// append-if-missing kept the role's own engine: the rendered
		// same-named container is role-owned
		require.Len(t, spec.Spec.Containers, 2)
		assert.Equal(t, "sglang:latest", spec.Spec.Containers[0].Image)

		_, err := ApplyRoleEngineRuntimePatches(ctx, spec, roleContainerNames, nil, nil, []EngineRuntimeRolePatch{{
			ProfileName:        "ik",
			DeclaredContainers: declaredContainersOf(profile),
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("engine", "/env/-", v1.EnvVar{Name: "A", Value: "b"}),
			},
		}}, "worker", nil)
		require.NoError(t, err)

		// the patch landed on the role-owned engine, not a profile-declared
		// same-named container
		engine := spec.Spec.Containers[0]
		assert.Equal(t, "sglang:latest", engine.Image)
		assert.Equal(t, []string{"PYTHONPATH", "A"}, envNames(engine))
	})

	t.Run("an init container name declared by both the role template and the profile resolves to the role-owned init container", func(t *testing.T) {
		profile := renderProfile()
		spec := renderSpec()
		// the role's own template declares an init container colliding with
		// the profile's runtime-init (declaring args so the patch would be
		// legal on either owner)
		spec.Spec.InitContainers = []v1.Container{{
			Name: "runtime-init", Image: "role-init:latest", Args: []string{"--base"},
		}}
		roleContainerNames := roleContainers("engine") // captured before injection
		roleInitContainerNames := roleContainers("runtime-init")
		InjectEngineRuntimeFragments(ctx, spec, []EngineRuntimeFragment{{
			Profile: profile,
			Runtime: workloadsv1alpha2.EngineRuntime{ProfileName: "ik"},
		}})
		// append-if-missing kept the role's own runtime-init: the rendered
		// same-named init container is role-owned
		require.Len(t, spec.Spec.InitContainers, 1)
		assert.Equal(t, "role-init:latest", spec.Spec.InitContainers[0].Image)

		_, err := ApplyRoleEngineRuntimePatches(ctx, spec, roleContainerNames, roleInitContainerNames, nil, []EngineRuntimeRolePatch{{
			ProfileName:        "ik",
			DeclaredContainers: declaredContainersOf(profile),
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("runtime-init", "/args/-", "--flag"),
			},
		}}, "worker", nil)
		require.NoError(t, err)

		// the patch landed on the role-owned init container
		assert.Equal(t, "role-init:latest", spec.Spec.InitContainers[0].Image)
		assert.Equal(t, []string{"--base", "--flag"}, spec.Spec.InitContainers[0].Args)
	})

	t.Run("an env name the legacy merge appended conflicts with a role-side patch", func(t *testing.T) {
		spec := renderSpec()
		// the profile sidecar declares env X and the role's legacy containers
		// entry appends a same-named X — both land before the patch passes,
		// so the §5.4 refusal rule must catch the collision.
		profile := renderProfile(v1.EnvVar{Name: "X", Value: "profile"})
		InjectEngineRuntimeFragments(ctx, spec, []EngineRuntimeFragment{{
			Profile: profile,
			Runtime: workloadsv1alpha2.EngineRuntime{
				ProfileName: "ik",
				Containers: []v1.Container{{
					Name: "runtime-sidecar",
					Env:  []v1.EnvVar{{Name: "X", Value: "role"}},
				}},
			},
		}})
		require.Len(t, spec.Spec.Containers[1].Env, 2) // the legacy merge landed

		_, err := ApplyRoleEngineRuntimePatches(ctx, spec, roleContainers("engine"), nil, nil, []EngineRuntimeRolePatch{{
			ProfileName:        "ik",
			DeclaredContainers: declaredContainersOf(profile),
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("runtime-sidecar", "/env/-", v1.EnvVar{Name: "X", Value: "patch"}),
			},
		}}, "worker", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `role "worker" engineRuntimes["ik"].containerPatches[0]`)
		assert.Contains(t, err.Error(), "add cannot join values")
	})

	t.Run("a profile-declared target absent from the rendered spec is a render error", func(t *testing.T) {
		// the name is inside this reference's declared vocabulary, but the
		// position never rendered it (here: no fragment pass ran)
		spec := renderSpec()
		_, err := ApplyRoleEngineRuntimePatches(ctx, spec, roleContainers("engine"), nil, nil, []EngineRuntimeRolePatch{{
			ProfileName:        "ik",
			DeclaredContainers: declaredContainersOf(renderProfile()),
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("runtime-sidecar", "/env/-", v1.EnvVar{Name: "A", Value: "b"}),
			},
		}}, "worker", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not present in the rendered spec")
		assert.Contains(t, err.Error(), `role "worker" engineRuntimes["ik"].containerPatches[0]`)
	})

	t.Run("a target declared only by a different reference's profile is a render error", func(t *testing.T) {
		// a name injected only by a different reference or profile is
		// outside this reference's target vocabulary
		spec := renderSpec()
		profile := renderProfile()
		InjectEngineRuntimeFragments(ctx, spec, []EngineRuntimeFragment{{
			Profile: profile,
			Runtime: workloadsv1alpha2.EngineRuntime{ProfileName: "ik"},
		}})
		other := makeProfile("other", nil)
		other.Spec.Containers = []v1.Container{{Name: "other-sidecar", Image: "other:latest"}}
		InjectEngineRuntimeFragments(ctx, spec, []EngineRuntimeFragment{{
			Profile: other,
			Runtime: workloadsv1alpha2.EngineRuntime{ProfileName: "other"},
		}})
		// the other profile's sidecar is rendered into the position...
		require.NotNil(t, findRenderedContainer(spec, "other-sidecar"))

		// ...but is not addressable by this reference's patches
		_, err := ApplyRoleEngineRuntimePatches(ctx, spec, roleContainers("engine"), nil, nil, []EngineRuntimeRolePatch{{
			ProfileName:        "ik",
			DeclaredContainers: declaredContainersOf(profile),
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("other-sidecar", "/env/-", v1.EnvVar{Name: "A", Value: "b"}),
			},
		}}, "worker", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(),
			`role "worker" engineRuntimes["ik"].containerPatches[0] targets container "other-sidecar", `+
				`which is neither role-declared nor declared by the profile`)
		// the other profile's sidecar gained nothing
		assert.Empty(t, findRenderedContainer(spec, "other-sidecar").Env)
	})

	t.Run("profile-side and role-side patches both fire in application order", func(t *testing.T) {
		// KEP-531 D10: the profile-side pass applies before the role-side
		// pass — what the role writes lands where it is written. The profile
		// declares a sidecar colliding with the role's own engine container,
		// so the role's legacy containers entry merges its env into the
		// role-owned engine ahead of the profile-side patch. The
		// profile-declared sidecar carries the profile's command and env, and
		// the role-side pass — last — renders its /command/0 insert as the
		// rendered command's index 0, ahead of anything the profile
		// inserted, and appends env entries that may $(VAR)-reference
		// profile-appended ones (the forward direction only).
		profile := renderProfile(v1.EnvVar{Name: "PROF_X", Value: "profile"})
		profile.Spec.Containers[0].Command = []string{"--sidecar-base"}
		profile.Spec.Containers = append(profile.Spec.Containers, v1.Container{Name: "engine"})
		fragment := EngineRuntimeFragment{
			Profile: profile,
			Runtime: workloadsv1alpha2.EngineRuntime{
				ProfileName: "ik",
				Containers: []v1.Container{{
					Name: "engine",
					Env:  []v1.EnvVar{{Name: "ROLE_ENV", Value: "role"}},
				}},
			},
		}
		profileEntries := []EngineRuntimePatch{{
			ProfileName: "ik",
			Targets:     []string{"engine"},
			Patches: []workloadsv1alpha2.ContainerPatch{
				jp("/command/0", "profile-wrap"),
				envPatch("PROF_ENV", "profile-side"),
			},
		}}
		roleEntries := []EngineRuntimeRolePatch{{
			ProfileName:        "ik",
			DeclaredContainers: declaredContainersOf(profile),
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("runtime-sidecar", "/command/0", "role-wrap"),
				roleJp("runtime-sidecar", "/env/-", v1.EnvVar{Name: "ROLE_X", Value: "$(PROF_X)"}),
			},
		}}

		spec := renderSpec()
		roleContainerNames := roleContainers("engine") // captured before injection
		InjectEngineRuntimeFragments(ctx, spec, []EngineRuntimeFragment{fragment})
		profileInserts, err := ApplyEngineRuntimePatches(ctx, spec, roleContainerNames, profileEntries)
		require.NoError(t, err)
		shadowed, err := ApplyRoleEngineRuntimePatches(
			ctx, spec, roleContainerNames, nil, nil, roleEntries, "worker", profileInserts)
		require.NoError(t, err)
		// the two passes inserted positionally on different containers, so
		// nothing shadowed
		assert.Empty(t, shadowed)

		// the profile-side pass fired on the role-declared engine: its
		// command insert landed and its env appends land behind the env the
		// legacy merge (role-side authored) appended
		engine := spec.Spec.Containers[0]
		assert.Equal(t, []string{"profile-wrap", "python"}, engine.Command)
		assert.Equal(t, []string{"PYTHONPATH", "ROLE_ENV", "PROF_ENV"}, envNames(engine))

		// the role-side pass fired on the profile-declared sidecar, last:
		// its /command/0 insert is the rendered command's index 0, ahead of
		// anything the profile inserted, and its env appends may
		// $(VAR)-reference profile-appended entries — the forward direction
		sidecar := spec.Spec.Containers[1]
		assert.Equal(t, []string{"role-wrap", "--sidecar-base"}, sidecar.Command)
		require.Len(t, sidecar.Env, 2)
		assert.Equal(t, []string{"PROF_X", "ROLE_X"}, envNames(sidecar))
		assert.Equal(t, "$(PROF_X)", sidecar.Env[1].Value)

		// a profile-side patch re-adding the role-side-merged env name
		// errors with profile attribution
		_, err = ApplyEngineRuntimePatches(ctx, spec, roleContainerNames, []EngineRuntimePatch{{
			ProfileName: "ik",
			Targets:     []string{"engine"},
			Patches:     []workloadsv1alpha2.ContainerPatch{envPatch("ROLE_ENV", "again")},
		}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), `profile "ik" containerPatches[0]`)
		assert.Contains(t, err.Error(), "add cannot join values")
	})
}

// TestApplyRoleEngineRuntimePatchesOneWay pins the KEP-531 D9 one-way
// content flow: a role-side patch may not mount a role-declared volume
// into, or inject a consuming-namespace secretKeyRef/configMapKeyRef env
// value into, a profile-declared container — render error. Role-declared
// containers may consume both sides' content, and fieldRef,
// resourceFieldRef and plain env values breach no one-way bound.
func TestApplyRoleEngineRuntimePatchesOneWay(t *testing.T) {
	ctx := context.Background()

	// roleVolumeNames snapshots the role-declared pod volumes the way the
	// reconciler does, before ANY injection — the built-in injector chain
	// included, so controller-injected volumes are not role content.
	roleVolumeNames := map[string]struct{}{"role-vol": {}}
	roleVol := v1.Volume{
		Name:         "role-vol",
		VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}},
	}
	secretRefEnv := v1.EnvVar{Name: "TOKEN", ValueFrom: &v1.EnvVarSource{
		SecretKeyRef: &v1.SecretKeySelector{
			LocalObjectReference: v1.LocalObjectReference{Name: "role-secret"}, Key: "token",
		},
	}}
	configMapRefEnv := v1.EnvVar{Name: "SITE", ValueFrom: &v1.EnvVarSource{
		ConfigMapKeyRef: &v1.ConfigMapKeySelector{
			LocalObjectReference: v1.LocalObjectReference{Name: "role-cm"}, Key: "site",
		},
	}}

	// renderedWithProfile returns a fresh spec carrying the role-declared
	// volume plus the ik profile's declared init container and sidecar.
	renderedWithProfile := func() *v1.PodTemplateSpec {
		spec := renderSpec()
		spec.Spec.Volumes = []v1.Volume{roleVol}
		profile := declaringProfile("ik")
		InjectEngineRuntimeFragments(ctx, spec, []EngineRuntimeFragment{{
			Profile: profile,
			Runtime: workloadsv1alpha2.EngineRuntime{ProfileName: "ik"},
		}})
		return spec
	}

	t.Run("a role-declared volume mounted into a profile-declared container is a render error", func(t *testing.T) {
		spec := renderedWithProfile()
		_, err := ApplyRoleEngineRuntimePatches(ctx, spec, roleContainers("engine"), nil, roleVolumeNames, []EngineRuntimeRolePatch{{
			ProfileName:        "ik",
			DeclaredContainers: declaredContainersOf(declaringProfile("ik")),
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("runtime-sidecar", "/volumeMounts/-", v1.VolumeMount{Name: "role-vol", MountPath: "/data"}),
			},
		}}, "worker", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `role "worker" engineRuntimes["ik"].containerPatches[0]`)
		assert.Contains(t, err.Error(), "violates the one-way principle")
		assert.Contains(t, err.Error(), `"role-vol"`)
		// the profile-declared sidecar gained nothing
		sidecar := findRenderedContainer(spec, "runtime-sidecar")
		require.NotNil(t, sidecar)
		assert.Empty(t, sidecar.VolumeMounts)
	})

	t.Run("a secretKeyRef env value into a profile-declared container is a render error", func(t *testing.T) {
		spec := renderedWithProfile()
		_, err := ApplyRoleEngineRuntimePatches(ctx, spec, roleContainers("engine"), nil, roleVolumeNames, []EngineRuntimeRolePatch{{
			ProfileName:        "ik",
			DeclaredContainers: declaredContainersOf(declaringProfile("ik")),
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("runtime-sidecar", "/env/-", secretRefEnv),
			},
		}}, "worker", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `role "worker" engineRuntimes["ik"].containerPatches[0]`)
		assert.Contains(t, err.Error(), "violates the one-way principle")
		assert.Contains(t, err.Error(), `"TOKEN"`)
		// the profile-declared sidecar gained nothing
		sidecar := findRenderedContainer(spec, "runtime-sidecar")
		require.NotNil(t, sidecar)
		assert.Empty(t, sidecar.Env)
	})

	t.Run("a configMapKeyRef env value into a profile-declared container is a render error", func(t *testing.T) {
		spec := renderedWithProfile()
		_, err := ApplyRoleEngineRuntimePatches(ctx, spec, roleContainers("engine"), nil, roleVolumeNames, []EngineRuntimeRolePatch{{
			ProfileName:        "ik",
			DeclaredContainers: declaredContainersOf(declaringProfile("ik")),
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("runtime-sidecar", "/env/-", configMapRefEnv),
			},
		}}, "worker", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `role "worker" engineRuntimes["ik"].containerPatches[0]`)
		assert.Contains(t, err.Error(), "violates the one-way principle")
		// the profile-declared sidecar gained nothing
		sidecar := findRenderedContainer(spec, "runtime-sidecar")
		require.NotNil(t, sidecar)
		assert.Empty(t, sidecar.Env)
	})

	t.Run("fieldRef, resourceFieldRef and plain env values into a profile-declared container stay legal", func(t *testing.T) {
		// only secret/configMap references are refused: a fieldRef reads the
		// pod's own metadata, a resourceFieldRef the container's own
		// resources, and a plain value nothing at all
		spec := renderedWithProfile()
		_, err := ApplyRoleEngineRuntimePatches(ctx, spec, roleContainers("engine"), nil, roleVolumeNames, []EngineRuntimeRolePatch{{
			ProfileName:        "ik",
			DeclaredContainers: declaredContainersOf(declaringProfile("ik")),
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("runtime-sidecar", "/env/-", v1.EnvVar{Name: "POD_NAME", ValueFrom: &v1.EnvVarSource{
					FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.name"},
				}}),
				roleJp("runtime-sidecar", "/env/-", v1.EnvVar{Name: "CPU_REQ", ValueFrom: &v1.EnvVarSource{
					ResourceFieldRef: &v1.ResourceFieldSelector{Resource: "requests.cpu"},
				}}),
				roleJp("runtime-sidecar", "/env/-", v1.EnvVar{Name: "PLAIN", Value: "x"}),
			},
		}}, "worker", nil)
		require.NoError(t, err)
		sidecar := findRenderedContainer(spec, "runtime-sidecar")
		require.NotNil(t, sidecar)
		assert.Equal(t, []string{"POD_NAME", "CPU_REQ", "PLAIN"}, envNames(*sidecar))
	})

	t.Run("a role-declared volume mounted into a profile-declared init container is a render error", func(t *testing.T) {
		// D8 resolves profile-declared targets across both lists — init
		// containers included — so D9 must refuse there too: a role volume
		// may not land in the publisher-controlled init container any more
		// than in a sidecar.
		spec := renderedWithProfile()
		_, err := ApplyRoleEngineRuntimePatches(ctx, spec, roleContainers("engine"), nil, roleVolumeNames, []EngineRuntimeRolePatch{{
			ProfileName:        "ik",
			DeclaredContainers: declaredContainersOf(declaringProfile("ik")),
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("runtime-init", "/volumeMounts/-", v1.VolumeMount{Name: "role-vol", MountPath: "/data"}),
			},
		}}, "worker", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `role "worker" engineRuntimes["ik"].containerPatches[0]`)
		assert.Contains(t, err.Error(), "violates the one-way principle")
		assert.Contains(t, err.Error(), `"role-vol"`)
		// the profile-declared init container gained nothing
		require.Len(t, spec.Spec.InitContainers, 1)
		assert.Empty(t, spec.Spec.InitContainers[0].VolumeMounts)
	})

	t.Run("the controller-injected rbg-cluster-config volume mounted into a profile-declared container is legal", func(t *testing.T) {
		// The controller's own injection is not role content — the same
		// stance the KEP takes for the built-in RBG_* env vars. The
		// roleVolumeNames snapshot is captured before the built-in injector
		// chain runs, so the config volume InjectConfig appends after the
		// snapshot is not in it, and a role-side patch mounting it into a
		// profile-declared container breaches no one-way bound.
		spec := renderedWithProfile()
		spec.Spec.Volumes = append(spec.Spec.Volumes, v1.Volume{
			Name: "rbg-cluster-config",
			VolumeSource: v1.VolumeSource{ConfigMap: &v1.ConfigMapVolumeSource{
				LocalObjectReference: v1.LocalObjectReference{Name: "resolver-rbg"},
			}},
		})
		_, err := ApplyRoleEngineRuntimePatches(ctx, spec, roleContainers("engine"), nil, roleVolumeNames, []EngineRuntimeRolePatch{{
			ProfileName:        "ik",
			DeclaredContainers: declaredContainersOf(declaringProfile("ik")),
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("runtime-sidecar", "/volumeMounts/-", v1.VolumeMount{
					Name: "rbg-cluster-config", MountPath: "/etc/rbg", ReadOnly: true,
				}),
			},
		}}, "worker", nil)
		require.NoError(t, err)
		sidecar := findRenderedContainer(spec, "runtime-sidecar")
		require.NotNil(t, sidecar)
		require.Len(t, sidecar.VolumeMounts, 1)
		assert.Equal(t, "rbg-cluster-config", sidecar.VolumeMounts[0].Name)
	})

	t.Run("the same volume mount and secret env into a role-declared container is legal", func(t *testing.T) {
		// D9's converse: role-declared containers may consume both sides'
		// content — the role's own volumes and consuming-namespace secrets
		// included
		spec := renderedWithProfile()
		_, err := ApplyRoleEngineRuntimePatches(ctx, spec, roleContainers("engine"), nil, roleVolumeNames, []EngineRuntimeRolePatch{{
			ProfileName:        "ik",
			DeclaredContainers: map[string]struct{}{}, // the target is role-declared
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("engine", "/volumeMounts/-", v1.VolumeMount{Name: "role-vol", MountPath: "/data"}),
				roleJp("engine", "/env/-", secretRefEnv),
			},
		}}, "worker", nil)
		require.NoError(t, err)
		engine := spec.Spec.Containers[0]
		require.Len(t, engine.VolumeMounts, 1)
		assert.Equal(t, "role-vol", engine.VolumeMounts[0].Name)
		assert.Equal(t, []string{"PYTHONPATH", "TOKEN"}, envNames(engine))
	})
}
