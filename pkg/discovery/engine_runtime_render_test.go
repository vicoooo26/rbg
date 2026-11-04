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
		require.NoError(t, ApplyEngineRuntimePatches(ctx, spec, roleContainers("engine"), entries))
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
		err := ApplyEngineRuntimePatches(ctx, spec, roleContainers("engine"), entries)
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
		err := ApplyEngineRuntimePatches(ctx, spec, roleContainers("engine"), entries)
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
		require.NoError(t, ApplyEngineRuntimePatches(ctx, spec, roleContainers("engine"), entries))
		// list order is outermost-last: a's insert lands first, b's wraps it
		assert.Equal(t, []string{"wrap-b", "wrap-a", "python"}, spec.Spec.Containers[0].Command)
	})

	t.Run("an empty entry list is a no-op", func(t *testing.T) {
		spec := renderSpec()
		before := spec.DeepCopy()
		require.NoError(t, ApplyEngineRuntimePatches(ctx, spec, roleContainers("engine"), nil))
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
	require.NoError(t, ApplyEngineRuntimePatches(ctx, spec, roleContainerNames, entries))

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
// the role's own container and init container names captured before any
// injection, then the fragment pass, then the role-side patch pass, then
// the profile-side patch pass — the KEP-531 application order — over a copy
// of the position's role-declared template.
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
	ctx := context.Background()
	InjectEngineRuntimeFragments(ctx, spec, plan.FragmentApplications[position])
	if err := ApplyRoleEngineRuntimePatches(
		ctx, spec, roleContainerNames, roleInitContainerNames,
		plan.RolePatchApplications[position], plan.RoleName,
	); err != nil {
		return spec, err
	}
	err := ApplyEngineRuntimePatches(
		ctx, spec, roleContainerNames, plan.PatchApplications[position])
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
		err := ApplyEngineRuntimePatches(ctx, spec, roleContainerNames, entries)
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
		require.NoError(t, ApplyEngineRuntimePatches(ctx, spec, roleContainerNames, entries))
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

// envNames returns a container's env var names, in list order.
func envNames(c v1.Container) []string {
	names := make([]string, 0, len(c.Env))
	for _, e := range c.Env {
		names = append(names, e.Name)
	}
	return names
}

// TestApplyRoleEngineRuntimePatches pins the KEP-531 application order — the
// role-side containerPatches pass over the profile-declared init containers
// and sidecars the fragment/legacy-merge pass rendered into the position,
// before the profile-side pass. Errors attribute to the role's reference,
// never the profile; a target the role's own template declares — a
// spec.containers or spec.initContainers member — is a render error even
// though the profile declares the name too, because the rendered
// same-named container is role-owned.
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
		InjectEngineRuntimeFragments(ctx, spec, []EngineRuntimeFragment{{
			Profile: renderProfile(),
			Runtime: workloadsv1alpha2.EngineRuntime{ProfileName: "ik"},
		}})

		require.NoError(t, ApplyRoleEngineRuntimePatches(ctx, spec, roleContainerNames, nil, []EngineRuntimeRolePatch{{
			ProfileName: "ik",
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("runtime-sidecar", "/env/-", v1.EnvVar{Name: "A", Value: "b"}),
				roleJp("runtime-init", "/args/-", "--flag"),
			},
		}}, "worker"))

		// the sidecar gains env A
		sidecar := findRenderedContainer(spec, "runtime-sidecar")
		require.NotNil(t, sidecar)
		require.Len(t, sidecar.Env, 1)
		assert.Equal(t, v1.EnvVar{Name: "A", Value: "b"}, sidecar.Env[0])
		// the init container gains the arg
		require.Len(t, spec.Spec.InitContainers, 1)
		assert.Equal(t, []string{"--base", "--flag"}, spec.Spec.InitContainers[0].Args)
	})

	t.Run("an env name the legacy merge appended conflicts with a role-side patch", func(t *testing.T) {
		spec := renderSpec()
		// the profile sidecar declares env X and the role's legacy containers
		// entry appends a same-named X — both land before the patch passes,
		// so the §5.4 refusal rule must catch the collision.
		InjectEngineRuntimeFragments(ctx, spec, []EngineRuntimeFragment{{
			Profile: renderProfile(v1.EnvVar{Name: "X", Value: "profile"}),
			Runtime: workloadsv1alpha2.EngineRuntime{
				ProfileName: "ik",
				Containers: []v1.Container{{
					Name: "runtime-sidecar",
					Env:  []v1.EnvVar{{Name: "X", Value: "role"}},
				}},
			},
		}})
		require.Len(t, spec.Spec.Containers[1].Env, 2) // the legacy merge landed

		err := ApplyRoleEngineRuntimePatches(ctx, spec, roleContainers("engine"), nil, []EngineRuntimeRolePatch{{
			ProfileName: "ik",
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("runtime-sidecar", "/env/-", v1.EnvVar{Name: "X", Value: "patch"}),
			},
		}}, "worker")
		require.Error(t, err)
		assert.Contains(t, err.Error(), `role "worker" engineRuntimes["ik"].containerPatches[0]`)
		assert.Contains(t, err.Error(), "add cannot join values")
	})

	t.Run("a target absent from the rendered spec is a render error", func(t *testing.T) {
		spec := renderSpec()
		InjectEngineRuntimeFragments(ctx, spec, []EngineRuntimeFragment{{
			Profile: renderProfile(),
			Runtime: workloadsv1alpha2.EngineRuntime{ProfileName: "ik"},
		}})
		err := ApplyRoleEngineRuntimePatches(ctx, spec, roleContainers("engine"), nil, []EngineRuntimeRolePatch{{
			ProfileName: "ik",
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("ghost", "/env/-", v1.EnvVar{Name: "A", Value: "b"}),
			},
		}}, "worker")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not present in the rendered spec")
		assert.Contains(t, err.Error(), `role "worker" engineRuntimes["ik"].containerPatches[0]`)
	})

	t.Run("a target the role's own template declares is a render error even though the profile declares the name", func(t *testing.T) {
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

		err := ApplyRoleEngineRuntimePatches(ctx, spec, roleContainerNames, nil, []EngineRuntimeRolePatch{{
			ProfileName: "ik",
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("engine", "/env/-", v1.EnvVar{Name: "A", Value: "b"}),
			},
		}}, "worker")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "which the role's own template declares")
		assert.Contains(t, err.Error(), `role "worker" engineRuntimes["ik"].containerPatches[0]`)
	})

	t.Run("an init container target the role's own template declares is a render error even though the profile declares the name", func(t *testing.T) {
		profile := renderProfile()
		spec := renderSpec()
		// the role's own template declares an init container colliding with
		// the profile's runtime-init (declaring args so the patch would be
		// legal if it fired)
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

		err := ApplyRoleEngineRuntimePatches(ctx, spec, roleContainerNames, roleInitContainerNames, []EngineRuntimeRolePatch{{
			ProfileName: "ik",
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("runtime-init", "/args/-", "--flag"),
			},
		}}, "worker")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "which the role's own template declares")
		assert.Contains(t, err.Error(), `role "worker" engineRuntimes["ik"].containerPatches[0]`)
	})

	t.Run("role-side and profile-side patches both fire in application order", func(t *testing.T) {
		// The profile declares a sidecar colliding with the role's own
		// engine container, so the role's legacy containers entry merges its
		// env into the role-owned engine — exercising the §6.3 shared
		// env-name conflict check across the legacy merge and the
		// profile-side patch pass.
		profile := renderProfile()
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
		roleEntries := []EngineRuntimeRolePatch{{
			ProfileName: "ik",
			Patches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				roleJp("runtime-sidecar", "/env/-", v1.EnvVar{Name: "A", Value: "role-side"}),
			},
		}}

		spec := renderSpec()
		roleContainerNames := roleContainers("engine") // captured before injection
		InjectEngineRuntimeFragments(ctx, spec, []EngineRuntimeFragment{fragment})
		require.NoError(t, ApplyRoleEngineRuntimePatches(ctx, spec, roleContainerNames, nil, roleEntries, "worker"))
		require.NoError(t, ApplyEngineRuntimePatches(ctx, spec, roleContainerNames, []EngineRuntimePatch{{
			ProfileName: "ik",
			Targets:     []string{"engine"},
			Patches:     []workloadsv1alpha2.ContainerPatch{envPatch("PROF_ENV", "profile-side")},
		}}))

		// the role-side pass fired on the profile-declared sidecar...
		sidecar := spec.Spec.Containers[1]
		require.Len(t, sidecar.Env, 1)
		assert.Equal(t, "A", sidecar.Env[0].Name)
		// ...and the profile-side pass fired on the role-declared engine,
		// seeing the env the legacy merge (role-side authored) appended
		engine := spec.Spec.Containers[0]
		assert.Equal(t, []string{"PYTHONPATH", "ROLE_ENV", "PROF_ENV"}, envNames(engine))

		// a profile-side patch re-adding the role-side-merged env name
		// errors with profile attribution
		err := ApplyEngineRuntimePatches(ctx, spec, roleContainerNames, []EngineRuntimePatch{{
			ProfileName: "ik",
			Targets:     []string{"engine"},
			Patches:     []workloadsv1alpha2.ContainerPatch{envPatch("ROLE_ENV", "again")},
		}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), `profile "ik" containerPatches[0]`)
		assert.Contains(t, err.Error(), "add cannot join values")
	})
}
