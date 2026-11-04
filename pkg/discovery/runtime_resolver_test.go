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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/rbgs/api/workloads/constants"
	workloadsv1alpha2 "sigs.k8s.io/rbgs/api/workloads/v1alpha2"
)

func resolveScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = workloadsv1alpha2.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	return scheme
}

func makeProfile(name string, patches []workloadsv1alpha2.ContainerPatch) *workloadsv1alpha2.ClusterEngineRuntimeProfile {
	return &workloadsv1alpha2.ClusterEngineRuntimeProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: workloadsv1alpha2.ClusterEngineRuntimeProfileSpec{
			Volumes: []corev1.Volume{{
				Name:         name + "-vol",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			}},
			ContainerPatches: patches,
		},
	}
}

func envPatch(name, value string) workloadsv1alpha2.ContainerPatch {
	return workloadsv1alpha2.ContainerPatch{
		JSONPatch: &workloadsv1alpha2.ContainerJSONPatch{
			Op: "add", Path: "/env/-",
			Value: runtime.RawExtension{Raw: []byte(fmt.Sprintf(`{"name":%q,"value":%q}`, name, value))},
		},
	}
}

func leaderWorkerRole(runtimes []workloadsv1alpha2.EngineRuntime) *workloadsv1alpha2.RoleSpec {
	tmpl := corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "engine"}}}}
	return &workloadsv1alpha2.RoleSpec{
		Name: "worker",
		Pattern: workloadsv1alpha2.Pattern{
			LeaderWorkerPattern: &workloadsv1alpha2.LeaderWorkerPattern{
				Size:           ptr.To(int32(2)),
				TemplateSource: workloadsv1alpha2.TemplateSource{Template: &tmpl},
			},
		},
		EngineRuntimes: runtimes,
	}
}

func standaloneRole(runtimes []workloadsv1alpha2.EngineRuntime) *workloadsv1alpha2.RoleSpec {
	tmpl := corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "engine"}}}}
	return &workloadsv1alpha2.RoleSpec{
		Name: "standalone",
		Pattern: workloadsv1alpha2.Pattern{
			StandalonePattern: &workloadsv1alpha2.StandalonePattern{
				TemplateSource: workloadsv1alpha2.TemplateSource{Template: &tmpl},
			},
		},
		EngineRuntimes: runtimes,
	}
}

func componentsRole(runtimes []workloadsv1alpha2.EngineRuntime) *workloadsv1alpha2.RoleSpec {
	component := func(name string) workloadsv1alpha2.InstanceComponent {
		return workloadsv1alpha2.InstanceComponent{
			Name: name,
			Size: ptr.To(int32(1)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "engine"}}},
			},
		}
	}
	return &workloadsv1alpha2.RoleSpec{
		Name: "worker",
		Pattern: workloadsv1alpha2.Pattern{
			CustomComponentsPattern: &workloadsv1alpha2.CustomComponentsPattern{
				Components: []workloadsv1alpha2.InstanceComponent{
					component("engine-comp"),
					component("aux-comp"),
				},
			},
		},
		EngineRuntimes: runtimes,
	}
}

// declaringProfile builds a profile that declares one init container and one
// sidecar — the container vocabulary role-side containerPatches may target.
func declaringProfile(name string) *workloadsv1alpha2.ClusterEngineRuntimeProfile {
	profile := makeProfile(name, nil)
	profile.Spec.InitContainers = []corev1.Container{{Name: "runtime-init"}}
	profile.Spec.Containers = []corev1.Container{{Name: "runtime-sidecar"}}
	return profile
}

func basicRBG() *workloadsv1alpha2.RoleBasedGroup {
	return &workloadsv1alpha2.RoleBasedGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "resolver-rbg", Namespace: "default"},
	}
}

func collectEvents(t *testing.T, recorder *record.FakeRecorder) []string {
	t.Helper()
	var events []string
	for {
		select {
		case ev := <-recorder.Events:
			events = append(events, ev)
		default:
			return events
		}
	}
}

func hasEventContaining(t *testing.T, recorder *record.FakeRecorder, reason string) bool {
	t.Helper()
	for _, ev := range collectEvents(t, recorder) {
		if strings.Contains(ev, reason) {
			return true
		}
	}
	return false
}

func countEventsContaining(t *testing.T, recorder *record.FakeRecorder, reason string) int {
	t.Helper()
	count := 0
	for _, ev := range collectEvents(t, recorder) {
		if strings.Contains(ev, reason) {
			count++
		}
	}
	return count
}

// resolverHarness builds a fake client seeded with profiles plus a recorder,
// then runs the resolver once against one RBG role. The context carries no
// logger so warning logs stay discarded and the test output stays pristine.
func resolverHarness(
	t *testing.T,
	profiles []*workloadsv1alpha2.ClusterEngineRuntimeProfile,
	role *workloadsv1alpha2.RoleSpec,
) (*EngineRuntimePlan, error, *record.FakeRecorder) {
	t.Helper()
	objects := make([]client.Object, 0, len(profiles))
	for _, profile := range profiles {
		objects = append(objects, profile)
	}
	recorder := record.NewFakeRecorder(16)
	cli := fake.NewClientBuilder().WithScheme(resolveScheme()).WithObjects(objects...).Build()
	plan, err := ResolveEngineRuntimes(context.Background(), cli, recorder, basicRBG(), role)
	return plan, err, recorder
}

// TestResolveEngineRuntimes pins the KEP-531 resolver semantics: the D2
// pattern default (leader-only under leaderWorkerPattern), single-component
// scoping, bare-name consent
// (patchContainers), role-side expression (containerPatches), the D6
// scope-ownership rule with its compatibility exemption, and the D7 render
// re-check — each warning event firing exactly once per reference per
// resolve.
func TestResolveEngineRuntimes(t *testing.T) {
	leader := string(constants.LeaderComponentType)
	worker := string(constants.WorkerComponentType)

	t.Run("no engine runtimes yields an empty plan", func(t *testing.T) {
		plan, err, recorder := resolverHarness(t, nil, leaderWorkerRole(nil))
		require.NoError(t, err)
		require.NotNil(t, plan)
		require.NotNil(t, plan.FragmentApplications)
		require.NotNil(t, plan.PatchApplications)
		require.NotNil(t, plan.RolePatchApplications)
		assert.Empty(t, plan.FragmentApplications)
		assert.Empty(t, plan.PatchApplications)
		assert.Empty(t, plan.RolePatchApplications)
		assert.Empty(t, plan.RoleName)
		assert.Empty(t, collectEvents(t, recorder))
	})

	t.Run("missing profile is a render error with an event", func(t *testing.T) {
		role := leaderWorkerRole([]workloadsv1alpha2.EngineRuntime{{ProfileName: "ghost"}})
		plan, err, recorder := resolverHarness(t, nil, role)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ghost")
		assert.NotNil(t, plan)
		assert.Equal(t, 1, countEventsContaining(t, recorder, ReasonEngineRuntimeRenderFailed))
	})

	t.Run("D2 default: absent component expands to the pattern default set — leader only under leaderWorkerPattern", func(t *testing.T) {
		profile := makeProfile("p", nil)
		role := leaderWorkerRole([]workloadsv1alpha2.EngineRuntime{{ProfileName: "p"}})
		plan, err, recorder := resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.NoError(t, err)
		assert.Empty(t, collectEvents(t, recorder))

		require.Len(t, plan.FragmentApplications[leader], 1)
		assert.Equal(t, "p", plan.FragmentApplications[leader][0].Profile.Name)
		// main never runs the sidecar stage for the worker template, so the
		// compatibility default must not deliver there
		assert.NotContains(t, plan.FragmentApplications, worker)
		assert.Equal(t, []string{leader, worker}, plan.Vocabulary)
	})

	t.Run("D2 capability: component worker reaches the worker position", func(t *testing.T) {
		profile := makeProfile("p", nil)
		role := leaderWorkerRole([]workloadsv1alpha2.EngineRuntime{{ProfileName: "p", Component: worker}})
		plan, err, recorder := resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.NoError(t, err)
		assert.Empty(t, collectEvents(t, recorder))

		require.Len(t, plan.FragmentApplications[worker], 1)
		assert.Equal(t, "p", plan.FragmentApplications[worker][0].Profile.Name)
		assert.NotContains(t, plan.FragmentApplications, leader)
	})

	t.Run("explicit component narrows the scope", func(t *testing.T) {
		profile := makeProfile("p", nil)
		role := leaderWorkerRole([]workloadsv1alpha2.EngineRuntime{{ProfileName: "p", Component: worker}})
		plan, err, recorder := resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.NoError(t, err)
		assert.Empty(t, collectEvents(t, recorder))

		require.Len(t, plan.FragmentApplications[worker], 1)
		assert.NotContains(t, plan.FragmentApplications, leader)
	})

	t.Run("customComponents: absent component expands to every declared component; explicit narrows", func(t *testing.T) {
		profile := makeProfile("p", nil)

		role := componentsRole([]workloadsv1alpha2.EngineRuntime{{ProfileName: "p"}})
		plan, err, recorder := resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.NoError(t, err)
		assert.Empty(t, collectEvents(t, recorder))
		require.Len(t, plan.FragmentApplications["engine-comp"], 1)
		require.Len(t, plan.FragmentApplications["aux-comp"], 1)

		role = componentsRole([]workloadsv1alpha2.EngineRuntime{{ProfileName: "p", Component: "engine-comp"}})
		plan, err, recorder = resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.NoError(t, err)
		assert.Empty(t, collectEvents(t, recorder))
		require.Len(t, plan.FragmentApplications["engine-comp"], 1)
		assert.NotContains(t, plan.FragmentApplications, "aux-comp")
	})

	t.Run("unknown component is a render error", func(t *testing.T) {
		profile := makeProfile("p", nil)
		plan, err, recorder := resolverHarness(t,
			[]*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile},
			leaderWorkerRole([]workloadsv1alpha2.EngineRuntime{{ProfileName: "p", Component: "bogus"}}),
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `names component "bogus", which the role's pattern does not define`)
		assert.Nil(t, plan.FragmentApplications[leader])
		assert.True(t, hasEventContaining(t, recorder, ReasonEngineRuntimeRenderFailed))
	})

	t.Run("standalone explicit component is a render error", func(t *testing.T) {
		profile := makeProfile("p", nil)
		role := standaloneRole([]workloadsv1alpha2.EngineRuntime{{ProfileName: "p", Component: leader}})
		plan, err, recorder := resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "standalonePattern role has no component vocabulary")
		assert.Nil(t, plan.FragmentApplications[StandalonePositionName])
		assert.True(t, hasEventContaining(t, recorder, ReasonEngineRuntimeRenderFailed))
	})

	t.Run("patchContainers consent routes the profile's patches to the named component", func(t *testing.T) {
		profile := makeProfile("p", []workloadsv1alpha2.ContainerPatch{envPatch("IK_SITE", "/opt/ik/site")})
		role := leaderWorkerRole([]workloadsv1alpha2.EngineRuntime{{
			ProfileName:     "p",
			Component:       leader,
			PatchContainers: []string{"engine"},
		}})
		plan, err, recorder := resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.NoError(t, err)
		assert.Empty(t, collectEvents(t, recorder))

		require.Len(t, plan.PatchApplications[leader], 1)
		applied := plan.PatchApplications[leader][0]
		assert.Equal(t, "p", applied.ProfileName)
		assert.Equal(t, []string{"engine"}, applied.Targets)
		assert.Equal(t, profile.Spec.ContainerPatches, applied.Patches)
		assert.NotContains(t, plan.PatchApplications, worker)
	})

	t.Run("unaimed patch warns once per reference without consent", func(t *testing.T) {
		profile := makeProfile("p", []workloadsv1alpha2.ContainerPatch{envPatch("IK_SITE", "/opt/ik/site")})

		// scenario 2: a named component without patchContainers
		role := leaderWorkerRole([]workloadsv1alpha2.EngineRuntime{{ProfileName: "p", Component: leader}})
		plan, err, recorder := resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.NoError(t, err)
		assert.Equal(t, 1, countEventsContaining(t, recorder, ReasonUnaimedContainerPatch))
		for _, ev := range collectEvents(t, recorder) {
			assert.Contains(t, ev, "patchContainers")
		}
		assert.Empty(t, plan.PatchApplications)
		// fragments still routed within the named scope
		require.Len(t, plan.FragmentApplications[leader], 1)

		// scenario 1: the pure compatibility shape warns too — absent
		// consent is a valid state, so profile evolution warns pre-existing
		// references, component or not
		role = leaderWorkerRole([]workloadsv1alpha2.EngineRuntime{{ProfileName: "p"}})
		_, err, recorder = resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.NoError(t, err)
		assert.Equal(t, 1, countEventsContaining(t, recorder, ReasonUnaimedContainerPatch))
	})

	t.Run("duplicate patchContainers name is a render error", func(t *testing.T) {
		profile := makeProfile("p", []workloadsv1alpha2.ContainerPatch{envPatch("IK_SITE", "/opt/ik/site")})
		role := leaderWorkerRole([]workloadsv1alpha2.EngineRuntime{{
			ProfileName:     "p",
			Component:       leader,
			PatchContainers: []string{"engine", "engine"},
		}})
		plan, err, recorder := resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `twice in patchContainers`)
		assert.Nil(t, plan.PatchApplications[leader])
		assert.True(t, hasEventContaining(t, recorder, ReasonEngineRuntimeRenderFailed))
	})

	t.Run("D6: same profile twice naming the same component is a render error; pure compat duplicates are exempt", func(t *testing.T) {
		profile := makeProfile("p", nil)

		// two component-scoped references owning the same component collide
		role := leaderWorkerRole([]workloadsv1alpha2.EngineRuntime{
			{ProfileName: "p", Component: leader},
			{ProfileName: "p", Component: leader},
		})
		plan, err, recorder := resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `the same component "leader"`)
		assert.True(t, hasEventContaining(t, recorder, ReasonEngineRuntimeRenderFailed))

		// two references of the pure compatibility shape keep main's
		// semantics verbatim: no ownership check, fragments append in
		// role-list order under the pattern's default set (leader only)
		role = leaderWorkerRole([]workloadsv1alpha2.EngineRuntime{
			{ProfileName: "p"},
			{ProfileName: "p"},
		})
		plan, err, recorder = resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.NoError(t, err)
		assert.Empty(t, collectEvents(t, recorder))
		require.Len(t, plan.FragmentApplications[leader], 2)
		assert.NotContains(t, plan.FragmentApplications, worker)
		assert.Equal(t, "p", plan.FragmentApplications[leader][0].Profile.Name)
		assert.Equal(t, "p", plan.FragmentApplications[leader][1].Profile.Name)
	})

	t.Run("role-side containerPatches route to the named component with the declared vocabulary", func(t *testing.T) {
		profile := declaringProfile("p")
		role := leaderWorkerRole([]workloadsv1alpha2.EngineRuntime{{
			ProfileName: "p",
			Component:   leader,
			ContainerPatches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				{
					Container: "runtime-init",
					JSONPatch: &workloadsv1alpha2.ContainerJSONPatch{
						Op: "add", Path: "/command/-",
						Value: runtime.RawExtension{Raw: []byte(`"--probe"`)},
					},
				},
				{
					Container: "runtime-sidecar",
					JSONPatch: &workloadsv1alpha2.ContainerJSONPatch{
						Op: "add", Path: "/env/-",
						Value: runtime.RawExtension{Raw: []byte(`{"name":"A","value":"b"}`)},
					},
				},
			},
		}})
		plan, err, recorder := resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.NoError(t, err)
		assert.Empty(t, collectEvents(t, recorder))

		require.Len(t, plan.RolePatchApplications[leader], 1)
		routed := plan.RolePatchApplications[leader][0]
		assert.Equal(t, "p", routed.ProfileName)
		require.Len(t, routed.Patches, 2)
		assert.Equal(t, "runtime-init", routed.Patches[0].Container)
		assert.Equal(t, "runtime-sidecar", routed.Patches[1].Container)
		assert.Contains(t, routed.DeclaredContainers, "runtime-init")
		assert.Contains(t, routed.DeclaredContainers, "runtime-sidecar")
		assert.NotContains(t, plan.RolePatchApplications, worker)
		assert.Equal(t, "worker", plan.RoleName)
	})

	t.Run("role-side unknown target is a render error", func(t *testing.T) {
		profile := declaringProfile("p")
		role := leaderWorkerRole([]workloadsv1alpha2.EngineRuntime{{
			ProfileName: "p",
			Component:   leader,
			ContainerPatches: []workloadsv1alpha2.EngineRuntimeContainerPatch{{
				Container: "nope",
				JSONPatch: &workloadsv1alpha2.ContainerJSONPatch{
					Op: "add", Path: "/env/-",
					Value: runtime.RawExtension{Raw: []byte(`{"name":"A","value":"b"}`)},
				},
			}},
		}})
		plan, err, recorder := resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `targets container "nope", which the profile does not declare`)
		assert.Nil(t, plan.RolePatchApplications[leader])
		assert.True(t, hasEventContaining(t, recorder, ReasonEngineRuntimeRenderFailed))
	})

	t.Run("role-side entry without a patch field is a render error", func(t *testing.T) {
		profile := declaringProfile("p")
		role := leaderWorkerRole([]workloadsv1alpha2.EngineRuntime{{
			ProfileName: "p",
			Component:   leader,
			ContainerPatches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
				{Container: "runtime-sidecar"}, // no jsonPatch set
			},
		}})
		plan, err, recorder := resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exactly one must be")
		assert.Nil(t, plan.RolePatchApplications[leader])
		assert.True(t, hasEventContaining(t, recorder, ReasonEngineRuntimeRenderFailed))
	})

	t.Run("D7 defense in depth: patch fields without component are a render error", func(t *testing.T) {
		// unreachable through the API (the CEL rule rejects it at write
		// time), but render is the authoritative gate and re-checks
		profile := makeProfile("p", []workloadsv1alpha2.ContainerPatch{envPatch("IK_SITE", "/opt/ik/site")})
		role := leaderWorkerRole([]workloadsv1alpha2.EngineRuntime{{
			ProfileName:     "p",
			PatchContainers: []string{"engine"},
		}})
		plan, err, recorder := resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "require component")
		assert.Nil(t, plan.PatchApplications[leader])
		assert.True(t, hasEventContaining(t, recorder, ReasonEngineRuntimeRenderFailed))
	})
}

// TestResolveEngineRuntimesVocabulary pins the render vocabulary the resolver
// records on the plan: the ordered positions the role's pattern defines,
// which are the only positions the plan's applications may key by. The
// render pass uses it to fail loudly when a construct path asks for a
// position the pattern does not define (a pattern/workload-type mismatch).
func TestResolveEngineRuntimesVocabulary(t *testing.T) {
	profile := makeProfile("ik", nil)

	t.Run("a role without engine runtimes records no vocabulary and accepts any position", func(t *testing.T) {
		plan, err, _ := resolverHarness(t, nil, leaderWorkerRole(nil))
		require.NoError(t, err)
		assert.Empty(t, plan.Vocabulary)
		// nothing is routed, so every position is legal — the render guard
		// must not fire for contentless plans
		assert.True(t, plan.HasPosition(string(constants.LeaderComponentType)))
		assert.True(t, plan.HasPosition(StandalonePositionName))
		assert.True(t, plan.HasPosition("anything-else"))
	})

	t.Run("leaderWorker vocabulary is leader and worker", func(t *testing.T) {
		role := leaderWorkerRole([]workloadsv1alpha2.EngineRuntime{{ProfileName: "ik"}})
		plan, err, _ := resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.NoError(t, err)
		assert.Equal(t, []string{
			string(constants.LeaderComponentType), string(constants.WorkerComponentType),
		}, plan.Vocabulary)
		assert.True(t, plan.HasPosition(string(constants.LeaderComponentType)))
		assert.True(t, plan.HasPosition(string(constants.WorkerComponentType)))
		assert.False(t, plan.HasPosition(StandalonePositionName))
	})

	t.Run("customComponents vocabulary is the component names in authored order", func(t *testing.T) {
		role := componentsRole([]workloadsv1alpha2.EngineRuntime{{ProfileName: "ik"}})
		plan, err, _ := resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.NoError(t, err)
		assert.Equal(t, []string{"engine-comp", "aux-comp"}, plan.Vocabulary)
		assert.True(t, plan.HasPosition("engine-comp"))
		assert.True(t, plan.HasPosition("aux-comp"))
		assert.False(t, plan.HasPosition(StandalonePositionName))
	})

	t.Run("standalone vocabulary is the sentinel", func(t *testing.T) {
		role := standaloneRole([]workloadsv1alpha2.EngineRuntime{{ProfileName: "ik"}})
		plan, err, _ := resolverHarness(t, []*workloadsv1alpha2.ClusterEngineRuntimeProfile{profile}, role)
		require.NoError(t, err)
		assert.Equal(t, []string{StandalonePositionName}, plan.Vocabulary)
		assert.True(t, plan.HasPosition(StandalonePositionName))
		assert.False(t, plan.HasPosition(string(constants.LeaderComponentType)))
	})
}
