/*
Copyright 2025 The RBG Authors.

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

package v1alpha2

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	NoUpdateStrategy      = "NoUpdate"
	RollingUpdateStrategy = "RollingUpdate"
)

// ClusterEngineRuntimeProfileSpec defines the desired state of ClusterEngineRuntimeProfile.
type ClusterEngineRuntimeProfileSpec struct {
	// +optional
	InitContainers []v1.Container `json:"initContainers,omitempty"`
	// +optional
	Containers []v1.Container `json:"containers,omitempty"`
	// +optional
	Volumes []v1.Volume `json:"volumes,omitempty"`
	// +kubebuilder:validation:Enum=NoUpdate;RollingUpdate
	// +kubebuilder:default=NoUpdate
	UpdateStrategy string `json:"updateStrategy"`

	// ContainerPatches applies ordered per-container modifications to the
	// role containers named by each referencing EngineRuntime's
	// patchContainers at render time, in list order. Each element sets
	// exactly one of its optional sibling fields — the same
	// variant-selection idiom as Pattern in RoleSpec and TemplateSource —
	// each field carrying one patch
	// format; `jsonPatch` carries the RFC 6902 `add`-only rules below.
	// A future patch format joins as another sibling field without
	// touching `jsonPatch`, and the single ordered list preserves
	// cross-format application order. Absent, no container is modified.
	//
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	ContainerPatches []ContainerPatch `json:"containerPatches,omitempty"`
}

// ContainerPatch is one ordered per-container modification. Exactly
// one patch field is set, selected by optional sibling fields — the
// same variant-selection idiom as Pattern in RoleSpec and
// TemplateSource.
// +kubebuilder:validation:XValidation:rule="has(self.jsonPatch)",message="exactly one patch field must be set"
type ContainerPatch struct {
	// JSONPatch carries the RFC 6902 format: `add`-only operations
	// with container-relative paths. See ContainerJSONPatch for its
	// grammar.
	// +optional
	JSONPatch *ContainerJSONPatch `json:"jsonPatch,omitempty"`
}

// ContainerJSONPatch is a single JSON Patch (RFC 6902) operation applied to
// a role container at render time.
//
// Two deliberate deviations from strict RFC 6902, because a nil
// field is the most common container shape: a missing target list
// is treated as empty where insertion is allowed (`/env/0`,
// `/volumeMounts/-`), and intermediate struct members along the
// allowlisted path are created when absent
// (`/securityContext/capabilities/add/-` on a container without a
// securityContext). Element insertion into `/command` or `/args`
// requires the container to declare that list: creating it would
// silently discard the image ENTRYPOINT/CMD, so render errors
// instead.
// +kubebuilder:validation:XValidation:rule="self.path.startsWith('/command/') || self.path.startsWith('/args/') || self.path.startsWith('/env/') || self.path.startsWith('/volumeMounts/') || self.path.startsWith('/securityContext/capabilities/add/')",message="path must be an element pointer under /command, /args, /env, /volumeMounts or /securityContext/capabilities/add"
type ContainerJSONPatch struct {
	// Op is the patch operation. Only `add` is accepted today;
	// `replace` is reserved as a future enum value and rejected.
	// +kubebuilder:validation:Enum=add
	// +kubebuilder:validation:Required
	Op string `json:"op"`

	// Path is an RFC 6901 pointer relative to the rendered target
	// container: `/command/0` prepends an argv element (head insert,
	// so multi-element prepends are written in reverse order),
	// `/env/0` inserts an env var at the head, `/volumeMounts/-`
	// appends a mount, `/securityContext/capabilities/add/-` appends
	// a capability. Whole-list paths are forbidden: `add` onto an
	// existing location replaces it, which would overwrite user
	// values.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Required
	Path string `json:"path"`

	// Value is the JSON value the operation adds, in the shape of
	// the path's element: a string for /command, /args and
	// /securityContext/capabilities/add elements; a corev1.EnvVar
	// object for /env; a corev1.VolumeMount object for
	// /volumeMounts. Accepted as arbitrary JSON at write time and
	// unmarshaled into the element's concrete corev1 type at render
	// time; a mismatched shape is a render error attributed to this
	// profile.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Required
	Value runtime.RawExtension `json:"value"`
}

// ClusterEngineRuntimeProfileStatus defines the observed state of ClusterEngineRuntimeProfile.
type ClusterEngineRuntimeProfileStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:storageversion

// ClusterEngineRuntimeProfile is the Schema for the clusterengineruntimeprofiles API.
type ClusterEngineRuntimeProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterEngineRuntimeProfileSpec   `json:"spec,omitempty"`
	Status ClusterEngineRuntimeProfileStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterEngineRuntimeProfileList contains a list of ClusterEngineRuntimeProfile.
type ClusterEngineRuntimeProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterEngineRuntimeProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterEngineRuntimeProfile{}, &ClusterEngineRuntimeProfileList{})
}
