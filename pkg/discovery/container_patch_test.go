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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/log"
	workloadsv1alpha2 "sigs.k8s.io/rbgs/api/workloads/v1alpha2"
)

func basePodSpec() *v1.PodTemplateSpec {
	return &v1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Name: "tpl"},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{Name: "engine", Image: "sglang:latest", Command: []string{"python"}, Args: []string{"-m", "sglang.launch_server"}},
			},
			Volumes: []v1.Volume{{Name: "role-vol", VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}}},
		},
	}
}

func roleContainers(names ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return m
}

// jp builds an `add` jsonPatch arm from a path and a Go value, mirroring how
// the apiserver delivers the authored YAML JSON inside RawExtension.
func jp(path string, value any) workloadsv1alpha2.ContainerPatch {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return workloadsv1alpha2.ContainerPatch{JSONPatch: &workloadsv1alpha2.ContainerJSONPatch{
		Op: "add", Path: path, Value: runtime.RawExtension{Raw: raw},
	}}
}

func apply(ctx context.Context, spec *v1.PodTemplateSpec, targets []string,
	rules []workloadsv1alpha2.ContainerPatch, profile string,
) error {
	return ApplyContainerPatch(ctx, spec, roleContainers("engine"), targets, rules, profile)
}

func TestApplyContainerPatchTargeting(t *testing.T) {
	ctx := log.IntoContext(context.Background(), klog.NewKlogr())
	rules := []workloadsv1alpha2.ContainerPatch{jp("/env/-", v1.EnvVar{Name: "IK_SITE", Value: "/opt/p"})}

	t.Run("target not a role-declared container errors", func(t *testing.T) {
		spec := basePodSpec()
		err := apply(ctx, spec, []string{"ghost"}, rules, "p")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a role-declared spec.containers member")
	})

	t.Run("target listed twice errors", func(t *testing.T) {
		spec := basePodSpec()
		err := apply(ctx, spec, []string{"engine", "engine"}, rules, "p")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "twice in patchContainers")
	})

	t.Run("empty rules or targets is a no-op", func(t *testing.T) {
		spec := basePodSpec()
		require.NoError(t, apply(ctx, spec, []string{"engine"}, nil, "p"))
		require.NoError(t, apply(ctx, spec, nil, rules, "p"))
		assert.Equal(t, basePodSpec().Spec, spec.Spec)
	})

	t.Run("element with no patch field set errors", func(t *testing.T) {
		spec := basePodSpec()
		err := apply(ctx, spec, []string{"engine"}, []workloadsv1alpha2.ContainerPatch{{}}, "p")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no patch field is set")
		assert.Contains(t, err.Error(), `profile "p"`)
		assert.Contains(t, err.Error(), "containerPatches[0]")
	})
}

func TestApplyContainerPatchGrammar(t *testing.T) {
	ctx := log.IntoContext(context.Background(), klog.NewKlogr())

	tests := []struct {
		name    string
		rule    workloadsv1alpha2.ContainerPatch
		wantErr string
	}{
		{name: "unsupported op", rule: workloadsv1alpha2.ContainerPatch{JSONPatch: &workloadsv1alpha2.ContainerJSONPatch{
			Op: "replace", Path: "/command/0", Value: runtime.RawExtension{Raw: []byte(`"x"`)}}}, wantErr: `op "replace" is not supported`},
		{name: "missing value", rule: workloadsv1alpha2.ContainerPatch{JSONPatch: &workloadsv1alpha2.ContainerJSONPatch{
			Op: "add", Path: "/command/0"}}, wantErr: "requires a value"},
		{name: "whole-list path", rule: jp("/command", "x"), wantErr: "not an element pointer"},
		{name: "path outside the allowlist", rule: jp("/image", "x"), wantErr: "not an element pointer"},
		{name: "deep pointer below the root", rule: jp("/env/0/value", "x"), wantErr: "not an element pointer"},
		{name: "segment is neither index nor dash", rule: jp("/command/x", "x"), wantErr: `neither an index nor "-"`},
		{name: "index beyond the list length", rule: jp("/command/5", "x"), wantErr: "exceeds the list length"},
		{name: "index beyond int range", rule: jp("/command/9223372036854775808", "x"), wantErr: "invalid index"},
		{name: "string value for an env slot", rule: jp("/env/-", "not-an-object"), wantErr: "value must match the target struct"},
		{name: "object value for a command slot", rule: jp("/command/0", map[string]string{"a": "b"}), wantErr: "value must be a JSON string"},
		{name: "unknown field in an object value", rule: jp("/env/-", map[string]any{"name": "X", "valuee": "typo"}),
			wantErr: "value must match the target struct"},
		{name: "env var without a name", rule: jp("/env/-", v1.EnvVar{Value: "x"}), wantErr: "requires a name"},
		{name: "volumeMount without a mountPath", rule: jp("/volumeMounts/-", v1.VolumeMount{Name: "role-vol"}), wantErr: "requires a mountPath"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := basePodSpec()
			err := apply(ctx, spec, []string{"engine"}, []workloadsv1alpha2.ContainerPatch{tc.rule}, "p")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Contains(t, err.Error(), `profile "p"`)
		})
	}
}

func TestApplyContainerPatchRFCAdd(t *testing.T) {
	ctx := log.IntoContext(context.Background(), klog.NewKlogr())

	t.Run("head insert: multi-element prepend is written in reverse order", func(t *testing.T) {
		spec := basePodSpec()
		require.NoError(t, apply(ctx, spec, []string{"engine"}, []workloadsv1alpha2.ContainerPatch{
			jp("/command/0", "exec"), // [exec, python]
			jp("/command/0", "--"),   // [--, exec, python]
			jp("/command/0", "/opt/p/wrap"),
		}, "p"))
		assert.Equal(t, []string{"/opt/p/wrap", "--", "exec", "python"}, spec.Spec.Containers[0].Command)
	})

	t.Run("tail insert appends", func(t *testing.T) {
		spec := basePodSpec()
		require.NoError(t, apply(ctx, spec, []string{"engine"}, []workloadsv1alpha2.ContainerPatch{
			jp("/args/-", "--extra"),
		}, "p"))
		assert.Equal(t, []string{"-m", "sglang.launch_server", "--extra"}, spec.Spec.Containers[0].Args)
	})

	t.Run("index == len behaves as append", func(t *testing.T) {
		spec := basePodSpec()
		require.NoError(t, apply(ctx, spec, []string{"engine"}, []workloadsv1alpha2.ContainerPatch{
			jp("/args/2", "--extra"),
		}, "p"))
		assert.Equal(t, []string{"-m", "sglang.launch_server", "--extra"}, spec.Spec.Containers[0].Args)
	})

	t.Run("middle index inserts in place", func(t *testing.T) {
		spec := basePodSpec()
		require.NoError(t, apply(ctx, spec, []string{"engine"}, []workloadsv1alpha2.ContainerPatch{
			jp("/args/1", "--extra"),
		}, "p"))
		assert.Equal(t, []string{"-m", "--extra", "sglang.launch_server"}, spec.Spec.Containers[0].Args)
	})

	t.Run("inserting into a command-less container errors (ENTRYPOINT hazard)", func(t *testing.T) {
		spec := basePodSpec()
		spec.Spec.Containers[0].Command = nil
		err := apply(ctx, spec, []string{"engine"}, []workloadsv1alpha2.ContainerPatch{jp("/command/0", "x")}, "p")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "declares no command")
		assert.Contains(t, err.Error(), "ENTRYPOINT")
	})

	t.Run("inserting into an args-less container errors (CMD hazard)", func(t *testing.T) {
		spec := basePodSpec()
		spec.Spec.Containers[0].Args = nil
		err := apply(ctx, spec, []string{"engine"}, []workloadsv1alpha2.ContainerPatch{jp("/args/0", "x")}, "p")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "declares no args")
		assert.Contains(t, err.Error(), "CMD")
	})

	t.Run("later ops in the same list see earlier ops' mutations", func(t *testing.T) {
		spec := basePodSpec()
		require.NoError(t, apply(ctx, spec, []string{"engine"}, []workloadsv1alpha2.ContainerPatch{
			jp("/env/-", v1.EnvVar{Name: "FIRST", Value: "1"}),
			jp("/env/0", v1.EnvVar{Name: "SECOND", Value: "2"}), // head-inserts before FIRST
		}, "p"))
		env := spec.Spec.Containers[0].Env
		require.Len(t, env, 2)
		assert.Equal(t, []string{"SECOND", "FIRST"}, []string{env[0].Name, env[1].Name})
	})
}

func TestApplyContainerPatchEnv(t *testing.T) {
	ctx := log.IntoContext(context.Background(), klog.NewKlogr())

	t.Run("adds a fresh var and leaves existing ones untouched", func(t *testing.T) {
		spec := basePodSpec()
		spec.Spec.Containers[0].Env = []v1.EnvVar{{Name: "PYTHONPATH", Value: "/usr/lib/python"}}
		require.NoError(t, apply(ctx, spec, []string{"engine"}, []workloadsv1alpha2.ContainerPatch{
			jp("/env/-", v1.EnvVar{Name: "IK_SITE", Value: "/opt/p"}),
		}, "p"))
		env := spec.Spec.Containers[0].Env
		require.Len(t, env, 2)
		assert.Equal(t, v1.EnvVar{Name: "PYTHONPATH", Value: "/usr/lib/python"}, env[0])
		assert.Equal(t, v1.EnvVar{Name: "IK_SITE", Value: "/opt/p"}, env[1])
	})

	t.Run("re-adding a name the role already declares errors — add cannot join", func(t *testing.T) {
		spec := basePodSpec()
		spec.Spec.Containers[0].Env = []v1.EnvVar{{Name: "PYTHONPATH", Value: "/usr/lib/python"}}
		err := apply(ctx, spec, []string{"engine"}, []workloadsv1alpha2.ContainerPatch{
			jp("/env/-", v1.EnvVar{Name: "PYTHONPATH", Value: "/opt/p"}),
		}, "p")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "add cannot join values")
	})

	t.Run("re-adding a name an earlier op introduced errors too", func(t *testing.T) {
		spec := basePodSpec()
		err := apply(ctx, spec, []string{"engine"}, []workloadsv1alpha2.ContainerPatch{
			jp("/env/-", v1.EnvVar{Name: "IK_SITE", Value: "/opt/p"}),
			jp("/env/-", v1.EnvVar{Name: "IK_SITE", Value: "/other"}),
		}, "p")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "add cannot join values")
	})

	t.Run("a duplicate the role itself declared is not the patch's business", func(t *testing.T) {
		spec := basePodSpec()
		spec.Spec.Containers[0].Env = []v1.EnvVar{
			{Name: "TWO", Value: "first"}, {Name: "TWO", Value: "second"},
		}
		require.NoError(t, apply(ctx, spec, []string{"engine"}, []workloadsv1alpha2.ContainerPatch{
			jp("/env/-", v1.EnvVar{Name: "IK_SITE", Value: "/opt/p"}),
		}, "p"))
		assert.Len(t, spec.Spec.Containers[0].Env, 3)
	})
}

func TestApplyContainerPatchVolumeMounts(t *testing.T) {
	ctx := log.IntoContext(context.Background(), klog.NewKlogr())
	ikMount := v1.VolumeMount{Name: "role-vol", MountPath: "/opt/p", ReadOnly: true}

	mount := func(spec *v1.PodTemplateSpec, m v1.VolumeMount, path string) error {
		return apply(ctx, spec, []string{"engine"}, []workloadsv1alpha2.ContainerPatch{jp(path, m)}, "p")
	}

	t.Run("appends a mount referencing a role volume", func(t *testing.T) {
		spec := basePodSpec()
		require.NoError(t, mount(spec, ikMount, "/volumeMounts/-"))
		require.Len(t, spec.Spec.Containers[0].VolumeMounts, 1)
		assert.Equal(t, ikMount, spec.Spec.Containers[0].VolumeMounts[0])
	})

	t.Run("exact (name, mountPath) pair is idempotent", func(t *testing.T) {
		spec := basePodSpec()
		spec.Spec.Containers[0].VolumeMounts = []v1.VolumeMount{ikMount}
		require.NoError(t, mount(spec, ikMount, "/volumeMounts/-"))
		assert.Len(t, spec.Spec.Containers[0].VolumeMounts, 1)
	})

	t.Run("same volume at a second path appends", func(t *testing.T) {
		spec := basePodSpec()
		spec.Spec.Containers[0].VolumeMounts = []v1.VolumeMount{{Name: "role-vol", MountPath: "/first"}}
		require.NoError(t, mount(spec, v1.VolumeMount{Name: "role-vol", MountPath: "/second"}, "/volumeMounts/-"))
		assert.Len(t, spec.Spec.Containers[0].VolumeMounts, 2)
	})

	t.Run("mount path already used by a different volume errors", func(t *testing.T) {
		spec := basePodSpec()
		// The conflicting volume must exist in the spec, otherwise the dangling
		// reference check fires before the mountPath conflict check.
		spec.Spec.Volumes = append(spec.Spec.Volumes,
			v1.Volume{Name: "other-vol", VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}})
		spec.Spec.Containers[0].VolumeMounts = []v1.VolumeMount{{Name: "role-vol", MountPath: "/opt/p"}}
		err := mount(spec, v1.VolumeMount{Name: "other-vol", MountPath: "/opt/p"}, "/volumeMounts/-")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "/opt/p")
		assert.Contains(t, err.Error(), "last-wins")
	})

	t.Run("mount path appended by an earlier op is also protected", func(t *testing.T) {
		spec := basePodSpec()
		spec.Spec.Volumes = append(spec.Spec.Volumes,
			v1.Volume{Name: "other-vol", VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}})
		err := apply(ctx, spec, []string{"engine"}, []workloadsv1alpha2.ContainerPatch{
			jp("/volumeMounts/-", v1.VolumeMount{Name: "other-vol", MountPath: "/a"}),
			jp("/volumeMounts/-", v1.VolumeMount{Name: "role-vol", MountPath: "/a"}),
		}, "p")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already used by a different volume")
	})

	t.Run("dangling volume reference errors naming profile and volume", func(t *testing.T) {
		spec := basePodSpec()
		err := mount(spec, v1.VolumeMount{Name: "no-such-vol", MountPath: "/opt/p"}, "/volumeMounts/-")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "profile \"p\"")
		assert.Contains(t, err.Error(), "no-such-vol")
	})

	t.Run("head insert is available for mounts too", func(t *testing.T) {
		spec := basePodSpec()
		spec.Spec.Containers[0].VolumeMounts = []v1.VolumeMount{ikMount}
		require.NoError(t, mount(spec, v1.VolumeMount{Name: "role-vol", MountPath: "/first"}, "/volumeMounts/0"))
		assert.Equal(t, "/first", spec.Spec.Containers[0].VolumeMounts[0].MountPath)
	})
}

func TestApplyContainerPatchCapabilities(t *testing.T) {
	ctx := log.IntoContext(context.Background(), klog.NewKlogr())

	cap := func(spec *v1.PodTemplateSpec, name string) error {
		return apply(ctx, spec, []string{"engine"},
			[]workloadsv1alpha2.ContainerPatch{jp("/securityContext/capabilities/add/-", name)}, "p")
	}

	t.Run("creates securityContext.capabilities when absent", func(t *testing.T) {
		spec := basePodSpec()
		require.NoError(t, cap(spec, "SYS_RESOURCE"))
		require.NotNil(t, spec.Spec.Containers[0].SecurityContext)
		require.Equal(t, []v1.Capability{"SYS_RESOURCE"}, spec.Spec.Containers[0].SecurityContext.Capabilities.Add)
	})

	t.Run("appends to an existing add list", func(t *testing.T) {
		spec := basePodSpec()
		spec.Spec.Containers[0].SecurityContext = &v1.SecurityContext{Capabilities: &v1.Capabilities{
			Add: []v1.Capability{"SYS_PTRACE"},
		}}
		require.NoError(t, cap(spec, "SYS_RESOURCE"))
		assert.Equal(t, []v1.Capability{"SYS_PTRACE", "SYS_RESOURCE"},
			spec.Spec.Containers[0].SecurityContext.Capabilities.Add)
	})

	t.Run("CAP_ prefix is normalized away", func(t *testing.T) {
		spec := basePodSpec()
		require.NoError(t, cap(spec, "CAP_SYS_RESOURCE"))
		assert.Equal(t, []v1.Capability{"SYS_RESOURCE"}, spec.Spec.Containers[0].SecurityContext.Capabilities.Add)
	})

	t.Run("duplicate capability is skipped, unknown dropped with a log", func(t *testing.T) {
		spec := basePodSpec()
		require.NoError(t, cap(spec, "SYS_RESOURCE"))
		require.NoError(t, cap(spec, "SYS_RESOURCE"))
		require.NoError(t, cap(spec, "NOT_A_CAPABILITY"))
		assert.Equal(t, []v1.Capability{"SYS_RESOURCE"}, spec.Spec.Containers[0].SecurityContext.Capabilities.Add)
	})

	t.Run("the drop list is untouched when a capability is added", func(t *testing.T) {
		spec := basePodSpec()
		spec.Spec.Containers[0].SecurityContext = &v1.SecurityContext{Capabilities: &v1.Capabilities{
			Drop: []v1.Capability{"ALL"},
		}}
		require.NoError(t, cap(spec, "SYS_RESOURCE"))
		assert.Equal(t, []v1.Capability{"ALL"}, spec.Spec.Containers[0].SecurityContext.Capabilities.Drop)
	})

	t.Run("non-string capability value errors", func(t *testing.T) {
		spec := basePodSpec()
		err := apply(ctx, spec, []string{"engine"},
			[]workloadsv1alpha2.ContainerPatch{jp("/securityContext/capabilities/add/-", map[string]string{"a": "b"})}, "p")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "value must be a JSON string")
	})
}
