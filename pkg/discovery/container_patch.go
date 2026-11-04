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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	workloadsv1alpha2 "sigs.k8s.io/rbgs/api/workloads/v1alpha2"
)

// Warning-event reasons emitted by the engine runtime resolver and the
// render passes on the referencing RBG.
const (
	// ReasonUnaimedContainerPatch: a patch-carrying profile is referenced but
	// the role lists no patchContainers — the patch does not fire.
	ReasonUnaimedContainerPatch = "UnaimedContainerPatch"
	// ReasonEngineRuntimeRenderFailed: emitted alongside a render error for
	// timeline observability.
	ReasonEngineRuntimeRenderFailed = "EngineRuntimeRenderFailed"
)

// patchSource names the author of a patch list so render errors attribute
// to the side that wrote the rules.
type patchSource struct {
	kind string // "profile" or "role"
	name string // profile name; role name on the role side
	ref  string // the reference's profileName, role side only
}

func profilePatchSource(profileName string) patchSource {
	return patchSource{kind: "profile", name: profileName}
}

func rolePatchSource(roleName, profileName string) patchSource {
	return patchSource{kind: "role", name: roleName, ref: profileName}
}

func (s patchSource) describe() string {
	if s.kind == "role" {
		return fmt.Sprintf("role %q engineRuntimes[%q].containerPatches", s.name, s.ref)
	}
	return fmt.Sprintf("profile %q containerPatches", s.name)
}

// renderedVolumeNames collects the volume names visible at patch time (the
// rendered spec's volumes already include the position's fragment volumes).
func renderedVolumeNames(podSpec *v1.PodTemplateSpec) map[string]struct{} {
	volumeNames := make(map[string]struct{}, len(podSpec.Spec.Volumes))
	for _, vol := range podSpec.Spec.Volumes {
		volumeNames[vol.Name] = struct{}{}
	}
	return volumeNames
}

// ApplyContainerPatch applies a profile's containerPatches to the target
// containers of a rendered pod spec, in list order. Each element sets
// exactly one patch field; today only `jsonPatch` exists, and an element
// with no patch field set is a render error attributed to the profile
// (the CEL exactly-one rule normally rejects it at write time).
// roleContainerNames holds the spec.containers names the role's own template
// declared, captured by the caller before any profile fragment injection —
// profile-injected containers and role-declared initContainers are
// therefore never addressable.
//
// The write-time path allowlist is CEL; this pass re-checks the full grammar
// (an element pointer under a finite root set) so a path that slips the CEL
// approximation fails loudly here, attributed to the profile. Two deviations
// from strict RFC 6902 are deliberate, because a nil field is the most common
// container shape: a missing target list counts as empty where insertion is
// allowed (env, volumeMounts, capabilities), and intermediate structs along
// the allowlisted path are created when absent (securityContext.capabilities).
// Element insertion into command or args requires the container to declare
// the list: creating it would silently discard the image ENTRYPOINT/CMD.
func ApplyContainerPatch(
	ctx context.Context,
	podSpec *v1.PodTemplateSpec,
	roleContainerNames map[string]struct{},
	targets []string,
	patches []workloadsv1alpha2.ContainerPatch,
	profileName string,
) error {
	if len(patches) == 0 || len(targets) == 0 {
		return nil
	}
	logger := log.FromContext(ctx)
	source := profilePatchSource(profileName)

	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if _, dup := seen[target]; dup {
			return fmt.Errorf("profile %q lists target container %q twice in patchContainers", profileName, target)
		}
		seen[target] = struct{}{}
		if _, ok := roleContainerNames[target]; !ok {
			return fmt.Errorf(
				"profile %q targets container %q, which is not a role-declared spec.containers member; "+
					"profile-injected containers and role-declared initContainers are not addressable",
				profileName, target,
			)
		}
	}

	volumeNames := renderedVolumeNames(podSpec)

	// Presence validation is element-scoped, so it runs once per list, not
	// once per target — attribution must not depend on target list order.
	for i, rule := range patches {
		if rule.JSONPatch == nil {
			return fmt.Errorf(
				"%s[%d]: no patch field is set; exactly one must be",
				source.describe(), i,
			)
		}
	}

	for _, target := range targets {
		container := findRenderedContainer(podSpec, target)
		if container == nil {
			return fmt.Errorf(
				"profile %q targets container %q, which is not present in the rendered spec", profileName, target,
			)
		}
		for i, rule := range patches {
			if err := applyJSONPatch(logger, container, *rule.JSONPatch, volumeNames, source, target, i); err != nil {
				return err
			}
		}
	}
	return nil
}

// ApplyEngineRuntimePatches is the render-time patch pass for one position's
// template: it feeds each per-reference entry to ApplyContainerPatch in
// role-list order, so a later reference's rules see the mutations of earlier
// ones — standard application order, preserved from the fragment-builder era
// by keeping the reference sequence the outer loop.
func ApplyEngineRuntimePatches(
	ctx context.Context,
	podSpec *v1.PodTemplateSpec,
	roleContainerNames map[string]struct{},
	entries []EngineRuntimePatch,
) error {
	for _, entry := range entries {
		if err := ApplyContainerPatch(
			ctx, podSpec, roleContainerNames, entry.Targets, entry.Patches, entry.ProfileName,
		); err != nil {
			return err
		}
	}
	return nil
}

// ApplyRoleEngineRuntimePatches applies the role-side containerPatches of
// one position's references, in reference order then list order — the
// KEP-531 application order, after the fragment/legacy-merge pass, the
// built-in injector chain and the profile-side pass: role-side patches land
// last. Each entry's target resolves per D8 against the role-declared names
// captured by the caller before any profile fragment injection plus the
// container names this reference's own profile declares; anything else — a
// name injected only by a different reference or profile — is a render
// error. roleContainerNames and roleInitContainerNames hold the names the
// role's own template declared (spec.containers and spec.initContainers).
func ApplyRoleEngineRuntimePatches(
	ctx context.Context,
	podSpec *v1.PodTemplateSpec,
	roleContainerNames map[string]struct{},
	roleInitContainerNames map[string]struct{},
	entries []EngineRuntimeRolePatch,
	roleName string,
) error {
	if len(entries) == 0 {
		return nil
	}
	logger := log.FromContext(ctx)
	volumeNames := renderedVolumeNames(podSpec)
	for _, entry := range entries {
		source := rolePatchSource(roleName, entry.ProfileName)
		for i, patch := range entry.Patches {
			container, err := resolveRolePatchTarget(
				podSpec, roleContainerNames, roleInitContainerNames, entry.DeclaredContainers,
				source, i, patch.Container,
			)
			if err != nil {
				return err
			}
			if err := applyJSONPatch(logger, container, *patch.JSONPatch, volumeNames, source, patch.Container, i); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveRolePatchTarget resolves one role-side patch target to its rendered
// container, per the KEP-531 D8 precedence: (1) a name the role's own
// template declares in spec.containers — the role-owned member; (2) a name
// it declares in spec.initContainers — the role-owned init container (the
// legacy containers merge cannot reach init containers; the patch grammar
// closes that gap); (3) a name this reference's own profile declares,
// located by name in the rendered pod — init containers included,
// profile-declared targets span both lists; (4) anything else is a render
// error. A name the role's own template also declares resolves to the
// role-owned container — the same precedence injection applies.
func resolveRolePatchTarget(
	podSpec *v1.PodTemplateSpec,
	roleContainerNames map[string]struct{},
	roleInitContainerNames map[string]struct{},
	declaredContainers map[string]struct{},
	source patchSource,
	idx int,
	name string,
) (*v1.Container, error) {
	if _, ok := roleContainerNames[name]; ok {
		if container := findRenderedContainer(podSpec, name); container != nil {
			return container, nil
		}
	}
	if _, ok := roleInitContainerNames[name]; ok {
		if container := findRenderedInitContainer(podSpec, name); container != nil {
			return container, nil
		}
	}
	if _, ok := declaredContainers[name]; ok {
		container := findRenderedProfileContainer(podSpec, name)
		if container == nil {
			return nil, fmt.Errorf(
				"%s[%d] targets container %q, which is not present in the rendered spec",
				source.describe(), idx, name)
		}
		return container, nil
	}
	return nil, fmt.Errorf(
		"%s[%d] targets container %q, which is neither role-declared nor declared by the profile",
		source.describe(), idx, name)
}

// findRenderedInitContainer finds an init container by name in the rendered
// spec's spec.initContainers — the locator for a role-declared init
// container target (role-owned under the append-if-missing merge).
func findRenderedInitContainer(podSpec *v1.PodTemplateSpec, name string) *v1.Container {
	for i := range podSpec.Spec.InitContainers {
		if podSpec.Spec.InitContainers[i].Name == name {
			return &podSpec.Spec.InitContainers[i]
		}
	}
	return nil
}

// findRenderedProfileContainer finds a container by name in the rendered
// spec — init containers first, then sidecars — because profile-declared
// targets include init containers and sidecars alike.
func findRenderedProfileContainer(podSpec *v1.PodTemplateSpec, name string) *v1.Container {
	for i := range podSpec.Spec.InitContainers {
		if podSpec.Spec.InitContainers[i].Name == name {
			return &podSpec.Spec.InitContainers[i]
		}
	}
	return findRenderedContainer(podSpec, name)
}

// applyJSONPatch applies one ContainerJSONPatch to one container. Value
// checks run against the container's current state, which by construction
// contains only the mutations of earlier rules — so a violation fires only
// when the patch introduces it, never for a duplicate the role itself
// declared.
func applyJSONPatch(
	logger logr.Logger,
	container *v1.Container,
	rule workloadsv1alpha2.ContainerJSONPatch,
	volumeNames map[string]struct{},
	source patchSource,
	containerName string,
	idx int,
) error {
	if rule.Op != "add" {
		return fmt.Errorf(
			"%s[%d]: op %q is not supported; only add is implemented",
			source.describe(), idx, rule.Op,
		)
	}
	if len(rule.Value.Raw) == 0 {
		return fmt.Errorf("%s[%d]: op add requires a value", source.describe(), idx)
	}

	pos, seg, err := parsePatchPath(rule.Path)
	if err != nil {
		return fmt.Errorf("%s[%d]: %w", source.describe(), idx, err)
	}

	switch pos {
	case "command":
		value, err := decodePatchString(rule.Value.Raw)
		if err != nil {
			return patchValueError(source, idx, rule.Path, err)
		}
		if len(container.Command) == 0 {
			return fmt.Errorf(
				"%s[%d] path %q targets container %q, which declares no command; "+
					"inserting would create one and silently discard the image ENTRYPOINT — declare the command to opt in",
				source.describe(), idx, rule.Path, containerName,
			)
		}
		list, err := insertElement(container.Command, seg, value)
		if err != nil {
			return patchIndexError(source, idx, rule.Path, err)
		}
		container.Command = list
	case "args":
		value, err := decodePatchString(rule.Value.Raw)
		if err != nil {
			return patchValueError(source, idx, rule.Path, err)
		}
		if len(container.Args) == 0 {
			return fmt.Errorf(
				"%s[%d] path %q targets container %q, which declares no args; "+
					"inserting would create them and silently discard the image CMD — declare the args to opt in",
				source.describe(), idx, rule.Path, containerName,
			)
		}
		list, err := insertElement(container.Args, seg, value)
		if err != nil {
			return patchIndexError(source, idx, rule.Path, err)
		}
		container.Args = list
	case "env":
		var env v1.EnvVar
		if err := decodePatchStruct(rule.Value.Raw, &env); err != nil {
			return patchValueError(source, idx, rule.Path, err)
		}
		if env.Name == "" {
			return fmt.Errorf(
				"%s[%d] path %q: an env var requires a name", source.describe(), idx, rule.Path,
			)
		}
		for _, existing := range container.Env {
			if existing.Name == env.Name {
				// add has no read ability, so it cannot compose with an
				// existing value (join semantics); the collision is an error
				// rather than a silent last-wins clobber.
				return fmt.Errorf(
					"%s[%d] path %q adds env var %q to container %q, which already declares it; "+
						"add cannot join values — a same-named entry must come from the role template or the patch, never both",
					source.describe(), idx, rule.Path, env.Name, containerName,
				)
			}
		}
		list, err := insertElement(container.Env, seg, env)
		if err != nil {
			return patchIndexError(source, idx, rule.Path, err)
		}
		container.Env = list
	case "volumeMounts":
		var mount v1.VolumeMount
		if err := decodePatchStruct(rule.Value.Raw, &mount); err != nil {
			return patchValueError(source, idx, rule.Path, err)
		}
		if mount.MountPath == "" {
			return fmt.Errorf(
				"%s[%d] path %q: a volumeMount requires a mountPath", source.describe(), idx, rule.Path,
			)
		}
		if err := checkVolumeMount(container, mount, volumeNames, source, containerName, idx, rule.Path); err != nil {
			return err
		}
		if isDuplicateMount(container.VolumeMounts, mount) {
			return nil
		}
		list, err := insertElement(container.VolumeMounts, seg, mount)
		if err != nil {
			return patchIndexError(source, idx, rule.Path, err)
		}
		container.VolumeMounts = list
	case "capabilities":
		capability, err := decodePatchString(rule.Value.Raw)
		if err != nil {
			return patchValueError(source, idx, rule.Path, err)
		}
		normalized := normalizeCapability(v1.Capability(capability))
		if !canonicalLinuxCapabilities[normalized] {
			logger.Info(fmt.Sprintf(
				"%s[%d] path %q: unknown capability name %q on container %q, dropping",
				source.describe(), idx, rule.Path, capability, containerName,
			))
			return nil
		}
		if container.SecurityContext == nil {
			container.SecurityContext = &v1.SecurityContext{}
		}
		if container.SecurityContext.Capabilities == nil {
			container.SecurityContext.Capabilities = &v1.Capabilities{}
		}
		if hasCapability(container.SecurityContext.Capabilities.Add, normalized) {
			return nil
		}
		list, err := insertElement(container.SecurityContext.Capabilities.Add, seg, v1.Capability(normalized))
		if err != nil {
			return patchIndexError(source, idx, rule.Path, err)
		}
		container.SecurityContext.Capabilities.Add = list
	default:
		// Unreachable: parsePatchPath rejects other roots.
		return fmt.Errorf("%s[%d] path %q: unsupported target", source.describe(), idx, rule.Path)
	}
	return nil
}

// parsePatchPath validates the container-relative RFC 6901 pointer grammar
// against the patch vocabulary: an element pointer (index or "-") under one
// of the finite roots. It returns the target identifier and the element
// segment. Whole-list roots ("/command", "/volumeMounts") are rejected —
// RFC 6902 add onto an existing location replaces it, which would overwrite
// user values.
func parsePatchPath(path string) (target, seg string, err error) {
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	switch {
	case len(segments) == 2 && segments[0] == "command",
		len(segments) == 2 && segments[0] == "args",
		len(segments) == 2 && segments[0] == "env",
		len(segments) == 2 && segments[0] == "volumeMounts":
		target, seg = segments[0], segments[1]
	case len(segments) == 4 && segments[0] == "securityContext" &&
		segments[1] == "capabilities" && segments[2] == "add":
		target, seg = "capabilities", segments[3]
	default:
		return "", "", fmt.Errorf("path %q is not an element pointer under /command, /args, /env, /volumeMounts or /securityContext/capabilities/add", path)
	}
	if seg != "-" {
		for _, r := range seg {
			if r < '0' || r > '9' {
				return "", "", fmt.Errorf("path %q: element segment %q is neither an index nor \"-\"", path, seg)
			}
		}
	}
	return target, seg, nil
}

// insertElement performs the RFC 6902 add insertion for one list element:
// "-" appends (index len); an explicit index must not exceed the list
// length (RFC 6902: adding beyond the array end does not extend it).
func insertElement[T any](list []T, seg string, value T) ([]T, error) {
	index, err := elementIndex(seg, len(list))
	if err != nil {
		return nil, err
	}
	list = append(list, value)
	copy(list[index+1:], list[index:])
	list[index] = value
	return list, nil
}

func elementIndex(seg string, length int) (int, error) {
	if seg == "-" {
		return length, nil
	}
	// strconv rejects non-decimal input and indices beyond int range; a
	// manual digit accumulation would silently wrap negative, bypass the
	// length check below, and panic in insertElement's copy.
	index, err := strconv.Atoi(seg)
	if err != nil {
		return 0, fmt.Errorf("invalid index %q: %w", seg, err)
	}
	if index > length {
		return 0, fmt.Errorf("index %s exceeds the list length %d", seg, length)
	}
	return index, nil
}

// isDuplicateMount reports whether the exact (name, mountPath) pair is
// already mounted; re-adding an identical mount is idempotent.
func isDuplicateMount(mounts []v1.VolumeMount, mount v1.VolumeMount) bool {
	for _, existing := range mounts {
		if existing.Name == mount.Name && existing.MountPath == mount.MountPath {
			return true
		}
	}
	return false
}

func hasCapability(add []v1.Capability, normalized string) bool {
	for _, existing := range add {
		if normalizeCapability(existing) == normalized {
			return true
		}
	}
	return false
}

// checkVolumeMount enforces the same merge invariants the additive verb did:
// every mount is validated against the volumes visible at patch time (the
// rendered spec volumes already include the position's fragment volumes,
// applied by the fragment pass before this one), a dangling reference fails
// fast, and a mountPath already used by a different volume is a render error —
// the kubelet resolves overlapping mount paths last-wins, a silent shadowing
// the merge refuses to create. Re-adding an identical (name, mountPath) is
// idempotent and callers skip it before inserting.
func checkVolumeMount(
	container *v1.Container, mount v1.VolumeMount,
	volumeNames map[string]struct{},
	source patchSource, containerName string, idx int, path string,
) error {
	if _, ok := volumeNames[mount.Name]; !ok {
		return fmt.Errorf(
			"%s[%d] path %q references volume %q (container %q), which is declared by "+
				"neither the rendered spec nor the profile",
			source.describe(), idx, path, mount.Name, containerName,
		)
	}
	for _, existing := range container.VolumeMounts {
		if existing.MountPath == mount.MountPath && existing.Name != mount.Name {
			return fmt.Errorf(
				"%s[%d] path %q mounts volume %q at path %q (container %q), already used by "+
					"a different volume; the kubelet resolves overlapping mount paths last-wins",
				source.describe(), idx, path, mount.Name, mount.MountPath, containerName,
			)
		}
	}
	return nil
}

// canonicalLinuxCapabilities is the canonical Linux capability name set used
// to normalize capability patch values. Unknown names are dropped and
// logged at render time: the apiserver does not validate capability names
// either, and the canonical set drifts across kernel versions, so render
// defers name validity to the runtime rather than erroring on a name it
// cannot judge (KEP-531 Patch Grammar).
var canonicalLinuxCapabilities = map[string]bool{
	// Modern-kernel names (Linux 5.8+); included so profiles targeting recent
	// hosts are not silently stripped.
	"BPF": true, "CHECKPOINT_RESTORE": true, "PERFMON": true,
	"AUDIT_CONTROL": true, "AUDIT_READ": true, "AUDIT_WRITE": true, "BLOCK_SUSPEND": true,
	"CHOWN": true, "DAC_OVERRIDE": true, "DAC_READ_SEARCH": true, "FOWNER": true, "FSETID": true,
	"IPC_LOCK": true, "IPC_OWNER": true, "KILL": true, "LEASE": true, "LINUX_IMMUTABLE": true,
	"MAC_ADMIN": true, "MAC_OVERRIDE": true, "MKNOD": true,
	"NET_ADMIN": true, "NET_BIND_SERVICE": true, "NET_BROADCAST": true, "NET_RAW": true,
	"SETFCAP": true, "SETGID": true, "SETPCAP": true, "SETUID": true, "SYSLOG": true,
	"SYS_ADMIN": true, "SYS_BOOT": true, "SYS_CHROOT": true, "SYS_MODULE": true, "SYS_NICE": true,
	"SYS_PACCT": true, "SYS_PTRACE": true, "SYS_RAWIO": true, "SYS_RESOURCE": true,
	"SYS_TIME": true, "SYS_TTY_CONFIG": true, "WAKE_ALARM": true,
}

func normalizeCapability(name v1.Capability) string {
	return strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(string(name))), "CAP_")
}

// decodePatchString unmarshals a scalar patch value. RawExtension.Raw holds
// the submitted JSON bytes verbatim (preserve-unknown-fields), so the decode
// sees exactly what the author wrote.
func decodePatchString(raw []byte) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("value must be a JSON string: %w", err)
	}
	return value, nil
}

// decodePatchStruct unmarshals an object patch value strictly: a mistyped
// field name fails here, attributed to the profile, instead of silently
// producing a half-initialized struct.
func decodePatchStruct(raw []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("value must match the target struct: %w", err)
	}
	return nil
}

func patchValueError(source patchSource, idx int, path string, err error) error {
	return fmt.Errorf("%s[%d] path %q: %w", source.describe(), idx, path, err)
}

func patchIndexError(source patchSource, idx int, path string, err error) error {
	return fmt.Errorf("%s[%d] path %q: %w", source.describe(), idx, path, err)
}

func findRenderedContainer(podSpec *v1.PodTemplateSpec, name string) *v1.Container {
	for i := range podSpec.Spec.Containers {
		if podSpec.Spec.Containers[i].Name == name {
			return &podSpec.Spec.Containers[i]
		}
	}
	return nil
}
