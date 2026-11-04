# Engine Runtime Profile

ClusterEngineRuntimeProfile is a cluster-scoped CRD that defines reusable sidecar, init-container, and volume configurations, plus an additive container patch that can modify existing role containers. These profiles can be injected into role pods, enabling consistent runtime dependencies across multiple RoleBasedGroups.

## Overview

Engine runtime profiles are useful for:
- **GPU Driver Injection**: Automatically inject GPU driver init containers
- **Monitoring Sidecars**: Add Prometheus exporters or logging agents
- **Shared Dependencies**: Common runtime libraries across all inference pods
- **Consistent Configuration**: Same sidecar configuration for all roles

## ClusterEngineRuntimeProfile CRD

```yaml
apiVersion: workloads.x-k8s.io/v1alpha2
kind: ClusterEngineRuntimeProfile
metadata:
  name: nvidia-runtime-profile
spec:
  # Update strategy: NoUpdate (default) or RollingUpdate
  # NoUpdate: profile changes don't trigger pod updates
  # RollingUpdate: profile changes trigger rolling update of associated pods
  updateStrategy: RollingUpdate
  initContainers:
    - name: gpu-driver-init
      image: nvidia/gpu-driver:init
      command: ["/bin/sh", "-c", "echo 'GPU driver initialized'"]
      volumeMounts:
        - name: gpu-lib
          mountPath: /usr/local/lib/gpu
  containers:
    - name: gpu-monitor
      image: nvidia/gpu-monitor:latest
      ports:
        - containerPort: 9090
          name: metrics
      resources:
        requests:
          cpu: "100m"
          memory: "128Mi"
  volumes:
    - name: gpu-lib
      emptyDir: {}
```

### Key Fields

| Field | Description |
|-------|-------------|
| `updateStrategy` | How profile changes affect existing pods |
| `initContainers` | Init containers to inject |
| `containers` | Sidecar containers to inject |
| `volumes` | Volumes to inject |
| `containerPatches` | Ordered per-container modifications; each element sets exactly one patch field — today `jsonPatch`, standard RFC 6902 `add`-only ops — applied to the role containers named by each referencing engineRuntime's `patchContainers` within the reference's named `component` |

### Update Strategies

| Strategy | Behavior |
|----------|----------|
| `NoUpdate` | Profile changes don't trigger pod recreation. Pods need manual update. |
| `RollingUpdate` | Profile changes trigger rolling update of pods using this profile. |

### Container Patches

`containerPatches` is a list of per-container modifications applied at render time to existing role containers. Each element sets exactly one patch field; `jsonPatch` is the only one today, and a future patch format is added as another sibling field. The `jsonPatch` field carries RFC 6902 JSONPatch, additive only:

- `op` must be `add` (schema-enumerated; `replace` is a reserved value, not implemented).
- `path` is an RFC 6901 pointer relative to the target container, restricted to element pointers under five allowed prefixes: `/command/`, `/args/`, `/env/`, `/volumeMounts/`, `/securityContext/capabilities/add/`.
- `value` is the JSON value to insert — a string for argv and capability elements, an `EnvVar` object for `/env`, a `VolumeMount` object for `/volumeMounts`.

```yaml
spec:
  containerPatches:
    - jsonPatch:
        op: add
        path: /env/-
        value: {name: PYTHONPATH, value: /opt/inference-kit/site}
    - jsonPatch:
        op: add
        path: /volumeMounts/-
        value: {name: inference-kit, mountPath: /opt/inference-kit, readOnly: true}
    - jsonPatch:
        op: add
        path: /securityContext/capabilities/add/-
        value: SYS_RESOURCE
```

Multiple `/command/0` head-inserts stack: the last rule ends up first in the argv, so multi-element prefixes (wrapper argv) are written in reverse order:

```yaml
spec:
  containerPatches:
    # final argv: [/opt/ik/wrap, --, exec, <original command...>]
    - jsonPatch: {op: add, path: /command/0, value: exec}
    - jsonPatch: {op: add, path: /command/0, value: --}
    - jsonPatch: {op: add, path: /command/0, value: /opt/ik/wrap}
```

Patches fire only where a role reference consents. Consent has two parts, both required: the reference names a `component`, and it lists the target containers in `patchContainers` — a CEL rule on the role rejects either patch field without `component` at write time. Each `patchContainers` entry is a bare container name addressing exactly one `spec.containers` member of the named component's rendered template; the profile's patches apply at the consented addresses in the profile's list order. If the reference does not consent — `patchContainers` absent — the patches do not fire and the render succeeds; if the profile declares `containerPatches`, an `UnaimedContainerPatch` warning event is emitted, so profile evolution cannot break pre-existing referencing workloads. A standalonePattern role cannot name a component (an explicit `component` there is a render error), so it has no path to the patches at all.

`injectContainers` is inert: the field predates this consent model, nothing reads it, its values are accepted and ignored, and it is removed at the next API version boundary.

## Using Engine Runtime Profiles

In RoleBasedGroup, reference the profile via `engineRuntimes` field. A reference that sets only `profileName` delivers the fragments to every component of the role's pattern; a reference that also names a `component` narrows the delivery to that component and may carry patch consent:

```yaml
apiVersion: workloads.x-k8s.io/v1alpha2
kind: RoleBasedGroup
metadata:
  name: gpu-inference
spec:
  roles:
    - name: prefill
      replicas: 2
      # component absent: fragments inject into every component
      engineRuntimes:
        - profileName: nvidia-runtime-profile
      leaderWorkerPattern:
        size: 2
        template:
          spec:
            containers:
              - name: prefill-main
                image: inference-engine:latest
                resources:
                  limits:
                    nvidia.com/gpu: "1"

    - name: decode
      replicas: 4
      # component + patchContainers: fragments deliver to the named
      # component, and the profile's containerPatches fire at the
      # consented containers
      engineRuntimes:
        - profileName: nvidia-runtime-profile
          component: worker
          patchContainers:
            - decode-main
      leaderWorkerPattern:
        size: 2
        template:
          spec:
            containers:
              - name: decode-main
                image: inference-engine:latest
                resources:
                  limits:
                    nvidia.com/gpu: "1"
```

### engineRuntimes Configuration

| Field | Description |
|-------|-------------|
| `profileName` | Name of the ClusterEngineRuntimeProfile to use (required) |
| `component` | Component the reference scopes to: `leader` or `worker` under leaderWorkerPattern, a declared component name under customComponentsPattern. Absent, the fragments (and the legacy `containers` merge) deliver to every component of the pattern. Required whenever `patchContainers` or `containerPatches` is set |
| `patchContainers` | Bare container names consenting to the profile's `containerPatches`, within the named component |
| `containerPatches` | The role's patches into the profile-declared containers, within the named component; each entry sets exactly one patch field — today `jsonPatch` |
| `containers` | Legacy: role-provided env/args appended to profile-declared containers; superseded by `containerPatches` |
| `injectContainers` | Inert: accepted and ignored, nothing reads it; removed at the next API version boundary |

## Injection Behavior

When a role references an engine runtime profile, delivery follows the reference's scope: the named `component`, or every component of the pattern when `component` is absent.

1. **Init Containers**: Init containers from the profile are added to the pod spec
2. **Sidecar Containers**: Containers from the profile are added to the pod spec
3. **Volumes**: Volumes from the profile are added to the pod spec
4. **Container Patches**: The profile's `containerPatches` fire at the containers a reference consents to — `component` plus `patchContainers` (a CEL rule rejects either patch field without `component`). Without consent the patches do not fire; if the profile declares `containerPatches`, an `UnaimedContainerPatch` warning event is emitted and the render succeeds

## Example: GPU Driver + Monitoring Profile

```yaml
# Step 1: Create the profile
apiVersion: workloads.x-k8s.io/v1alpha2
kind: ClusterEngineRuntimeProfile
metadata:
  name: nvidia-runtime-profile
spec:
  updateStrategy: RollingUpdate
  initContainers:
    - name: gpu-driver-init
      image: nvidia/driver-init:latest
      command: ["/install-driver.sh"]
      volumeMounts:
        - name: nvidia-lib
          mountPath: /usr/local/nvidia
  containers:
    - name: dcgm-exporter
      image: nvidia/dcgm-exporter:latest
      ports:
        - containerPort: 9400
          name: metrics
      resources:
        requests:
          cpu: "100m"
          memory: "128Mi"
  volumes:
    - name: nvidia-lib
      emptyDir: {}
  containerPatches:
    - jsonPatch:
        op: add
        path: /volumeMounts/-
        value:
          name: nvidia-lib
          mountPath: /usr/local/nvidia
          readOnly: true

---
# Step 2: Use in RoleBasedGroup
apiVersion: workloads.x-k8s.io/v1alpha2
kind: RoleBasedGroup
metadata:
  name: inference-with-monitoring
spec:
  roles:
    - name: inference
      replicas: 4
      engineRuntimes:
        - profileName: nvidia-runtime-profile
          # component + patchContainers: fragments deliver to the worker
          # component, and the profile's containerPatches fire at the
          # consented containers
          component: worker
          patchContainers:
            - inference-engine
      leaderWorkerPattern:
        size: 2
        template:
          spec:
            containers:
              - name: inference-engine
                image: sglang:latest
                resources:
                  limits:
                    nvidia.com/gpu: "1"
```

This results in worker pods with:
- GPU driver init container (with the NVIDIA library volume mounted)
- DCGM exporter sidecar for GPU metrics
- The `inference-engine` container mounting `nvidia-lib` through the consented profile patch

The leader component renders from its own template unchanged: the reference's `component` scope keeps both the fragments and the patches out of it.

## Profile Updates

When a ClusterEngineRuntimeProfile is updated:

- With `NoUpdate`: Existing pods retain old configuration until manually updated
- With `RollingUpdate`: Controller triggers rolling update of all affected pods

Rolling update respects each role's `rolloutStrategy` configuration.

## Use Cases

- **GPU Infrastructure**: Inject NVIDIA drivers, DCGM exporters, GPU utilities
- **Observability**: Add Prometheus exporters, logging agents, tracing sidecars
- **Security**: Inject security scanners, certificate managers
- **Common Dependencies**: Shared libraries across multiple inference deployments

## Examples

- [Engine Runtime Profile Example](../../examples/basic/engine-runtime/engine-runtime-profile.yaml)