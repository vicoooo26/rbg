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

package rbg

import (
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/rbgs/api/workloads/constants"
	workloadsv1alpha2 "sigs.k8s.io/rbgs/api/workloads/v1alpha2"
	"sigs.k8s.io/rbgs/pkg/discovery"
	"sigs.k8s.io/rbgs/test/envtest/testutil"
	wrappersv2 "sigs.k8s.io/rbgs/test/wrappers/v1alpha2"
)

// The KEP-531 targeting semantics (keps/531-engine-runtime-profile-injection/README.md):
// an absent component expands to the pattern's default component set —
// main's published behavior, which under leaderWorkerPattern is the leader
// ONLY: the worker template's injector chain omits the sidecar stage
// outright, so a profileName-only reference never injects into the worker
// (D2, scenario 1); a named component scopes fragments and legacy merge to
// that one component — component: worker is the new capability that reaches
// a component main's default never touches (scenario 2); patchContainers
// consents the profile's patches and containerPatches carries the role's
// patches, both within the named component (scenario 3).
var _ = Describe("RoleBasedGroup engine runtime targeting", func() {
	const (
		targetingTimeout  = time.Second * 60
		targetingInterval = time.Millisecond * 500
	)

	var testNs string

	BeforeEach(func() {
		testNs = fmt.Sprintf("test-targeting-%d", time.Now().UnixNano())
		testutil.CreateNamespace(testNs)
	})
	AfterEach(func() {
		testutil.DeleteNamespace(testNs)
	})

	// addOp builds an `add` jsonPatch arm, mirroring how the apiserver
	// delivers the authored YAML JSON inside RawExtension.
	addOp := func(path string, value any) workloadsv1alpha2.ContainerPatch {
		raw, err := json.Marshal(value)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return workloadsv1alpha2.ContainerPatch{
			JSONPatch: &workloadsv1alpha2.ContainerJSONPatch{
				Op: "add", Path: path, Value: runtime.RawExtension{Raw: raw},
			},
		}
	}

	// roleAddOp builds one role-side containerPatches entry: the same
	// `add` jsonPatch arm as addOp, wrapped in the role-side shape that
	// names its own target container.
	roleAddOp := func(container, path string, value any) workloadsv1alpha2.EngineRuntimeContainerPatch {
		raw, err := json.Marshal(value)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return workloadsv1alpha2.EngineRuntimeContainerPatch{
			Container: container,
			JSONPatch: &workloadsv1alpha2.ContainerJSONPatch{
				Op: "add", Path: path, Value: runtime.RawExtension{Raw: raw},
			},
		}
	}

	// makeTargetingProfile builds a profile carrying one emptyDir volume
	// (a pod-level fragment) plus the given container patches. The
	// updateStrategy carries no omitempty: leaving the zero value would
	// marshal spec.updateStrategy as "" and fail the apiserver enum
	// validation, so the fixture sets it.
	makeTargetingProfile := func(name string, patches ...workloadsv1alpha2.ContainerPatch) *workloadsv1alpha2.ClusterEngineRuntimeProfile {
		return &workloadsv1alpha2.ClusterEngineRuntimeProfile{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: workloadsv1alpha2.ClusterEngineRuntimeProfileSpec{
				UpdateStrategy: "NoUpdate",
				Volumes: []corev1.Volume{{
					Name: name + "-vol",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				}},
				ContainerPatches: patches,
			},
		}
	}

	// withSidecar declares one profile-injected sidecar container — the
	// fragment container a role-side containerPatches entry addresses.
	withSidecar := func(
		profile *workloadsv1alpha2.ClusterEngineRuntimeProfile,
		name string, env ...corev1.EnvVar,
	) *workloadsv1alpha2.ClusterEngineRuntimeProfile {
		profile.Spec.Containers = append(profile.Spec.Containers, corev1.Container{
			Name: name, Image: name + ":latest", Env: env,
		})
		return profile
	}

	// withInitContainer declares one profile-injected init container with
	// the given args — role-side patches address profile init containers
	// too, and an /args add requires the target to declare its args.
	withInitContainer := func(
		profile *workloadsv1alpha2.ClusterEngineRuntimeProfile,
		name string, args ...string,
	) *workloadsv1alpha2.ClusterEngineRuntimeProfile {
		profile.Spec.InitContainers = append(profile.Spec.InitContainers, corev1.Container{
			Name: name, Image: name + ":latest", Args: args,
		})
		return profile
	}

	// leaderWorkerRole builds the single leaderWorkerPattern role the
	// targeting fixtures render (the RoleInstanceSet path) referencing the
	// given engine runtimes. BuildLeaderWorkerRole declares an `nginx`
	// container in the shared base template; the targeting semantics
	// address it as `engine`, so the fixture renames it and gives it a
	// PYTHONPATH env the profile must not collide with. Returned by value
	// so callers can still mutate the shared base template through the
	// embedded pointers (e.g. to declare a role volume for the D9 cases).
	leaderWorkerRole := func(runtimes ...workloadsv1alpha2.EngineRuntime) workloadsv1alpha2.RoleSpec {
		role := wrappersv2.BuildLeaderWorkerRole("worker").Obj()
		base := role.Pattern.LeaderWorkerPattern.TemplateSource.Template
		base.Spec.Containers[0].Name = "engine"
		base.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "PYTHONPATH", Value: "/usr/lib/python"}}
		role.EngineRuntimes = runtimes
		return role
	}

	// createLeaderWorkerRBG creates a v1alpha2 RBG carrying one
	// already-built leaderWorkerPattern role — the entrypoint for fixtures
	// that need to shape the role beyond the runtimes list.
	createLeaderWorkerRBG := func(name string, role workloadsv1alpha2.RoleSpec) *workloadsv1alpha2.RoleBasedGroup {
		rbg := wrappersv2.BuildBasicRoleBasedGroup(name, testNs).
			WithRoles([]workloadsv1alpha2.RoleSpec{role}).
			Obj()
		ExpectWithOffset(1, testutil.K8sClient.Create(testutil.Ctx, rbg)).To(Succeed())

		created := &workloadsv1alpha2.RoleBasedGroup{}
		ExpectWithOffset(1, testutil.K8sClient.Get(
			testutil.Ctx, types.NamespacedName{Name: name, Namespace: testNs}, created,
		)).To(Succeed())
		return created
	}

	// leaderWorkerRBG creates a v1alpha2 RBG whose single role is a
	// leaderWorkerPattern role referencing the given engine runtimes.
	leaderWorkerRBG := func(name string, runtimes ...workloadsv1alpha2.EngineRuntime) *workloadsv1alpha2.RoleBasedGroup {
		return createLeaderWorkerRBG(name, leaderWorkerRole(runtimes...))
	}

	// standaloneRBG creates a v1alpha2 RBG whose worker role is a Deployment
	// workload. The storage version is v1alpha2 and this suite wires no
	// conversion webhooks, so a v1alpha1 RBG would lose its role pattern in
	// the apiserver's structural version conversion; the role-workload-type
	// annotation below is exactly the one the v1alpha1 conversion writes, so
	// both entrypoints dispatch to the same Deployment reconciler.
	standaloneRBG := func(name string, runtime workloadsv1alpha2.EngineRuntime) *workloadsv1alpha2.RoleBasedGroup {
		return &workloadsv1alpha2.RoleBasedGroup{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNs},
			Spec: workloadsv1alpha2.RoleBasedGroupSpec{
				Roles: []workloadsv1alpha2.RoleSpec{{
					Name:     "worker",
					Replicas: ptr.To(int32(1)),
					Annotations: map[string]string{
						constants.RoleWorkloadTypeAnnotationKey: constants.DeploymentWorkloadType,
					},
					Pattern: workloadsv1alpha2.Pattern{
						StandalonePattern: &workloadsv1alpha2.StandalonePattern{
							TemplateSource: workloadsv1alpha2.TemplateSource{
								Template: ptr.To(corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
									Name:    "engine",
									Image:   "nginx:latest",
									Command: []string{"nginx"},
									Env:     []corev1.EnvVar{{Name: "PYTHONPATH", Value: "/usr/lib/python"}},
								}}}}),
							},
						},
					},
					EngineRuntimes: []workloadsv1alpha2.EngineRuntime{runtime},
				}},
			},
		}
	}

	// rolePods waits until every pod of the role exists and indexes them by
	// component name, so the leader and worker components are addressable
	// separately.
	rolePods := func(rbgName, roleName string, count int) map[string]corev1.Pod {
		pods := map[string]corev1.Pod{}
		EventuallyWithOffset(1, func() int {
			podList := &corev1.PodList{}
			if err := testutil.K8sClient.List(testutil.Ctx, podList,
				client.InNamespace(testNs),
				client.MatchingLabels{
					constants.GroupNameLabelKey: rbgName,
					constants.RoleNameLabelKey:  roleName,
				},
			); err != nil {
				return 0
			}
			pods = map[string]corev1.Pod{}
			for i := range podList.Items {
				pod := podList.Items[i]
				if pod.DeletionTimestamp != nil {
					continue
				}
				pods[pod.Labels[constants.ComponentNameLabelKey]] = pod
			}
			return len(pods)
		}, targetingTimeout, targetingInterval).Should(Equal(count))
		return pods
	}

	// listDeployments lists the Deployments of the test namespace — the
	// child workload shape a standalonePattern role renders.
	listDeployments := func() []appsv1.Deployment {
		list := &appsv1.DeploymentList{}
		ExpectWithOffset(1, testutil.K8sClient.List(testutil.Ctx, list, client.InNamespace(testNs))).To(Succeed())
		return list.Items
	}

	// hasEvent reports whether a warning event with the given reason was
	// recorded in the test namespace. Assertions match on the reason only —
	// the message wording is not contract, and reconcile loops legitimately
	// repeat events, so no count is ever asserted.
	hasEvent := func(reason string) bool {
		events := &corev1.EventList{}
		ExpectWithOffset(1, testutil.K8sClient.List(testutil.Ctx, events, client.InNamespace(testNs))).To(Succeed())
		for _, ev := range events.Items {
			if ev.Reason == reason {
				return true
			}
		}
		return false
	}

	// noEngineRuntimeEvents reports whether no engine-runtime event of any
	// known reason fired — the byte-for-byte guarantee of scenario 1.
	noEngineRuntimeEvents := func() bool {
		return !hasEvent(discovery.ReasonUnaimedContainerPatch) &&
			!hasEvent(discovery.ReasonEngineRuntimeRenderFailed) &&
			!hasEvent(discovery.ReasonEngineRuntimePositionalShadowing)
	}

	hasMountName := func(mounts []corev1.VolumeMount, name string) bool {
		for _, m := range mounts {
			if m.Name == name {
				return true
			}
		}
		return false
	}

	// noRoleInstanceSet pins that the role's child workload was never
	// created — the render error must leave no partial child behind.
	noRoleInstanceSet := func(rbg *workloadsv1alpha2.RoleBasedGroup) {
		risName := rbg.GetWorkloadName(&rbg.Spec.Roles[0])
		ConsistentlyWithOffset(1, func(g Gomega) {
			err := testutil.K8sClient.Get(testutil.Ctx, types.NamespacedName{
				Name: risName, Namespace: testNs,
			}, &workloadsv1alpha2.RoleInstanceSet{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "RoleInstanceSet %s must not exist", risName)
		}, 5*time.Second, targetingInterval).Should(Succeed())
	}

	Context("scenario 1: profileName-only references stay byte-for-byte compatible", func() {
		It("delivers the fragments to the leader only — the worker sidecar stage is skipped", func() {
			// D2: absent component expands to the pattern's default
			// component set, which under leaderWorkerPattern is the leader
			// ONLY — main's published behavior. The worker template's
			// injector chain omits the sidecar stage outright, so no
			// fragment and no legacy merge may reach it.
			profile := makeTargetingProfile("ik-compat")
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-compat", workloadsv1alpha2.EngineRuntime{
				ProfileName: "ik-compat",
			})

			pods := rolePods(rbg.Name, rbg.Spec.Roles[0].Name, 2)
			leader := findContainer(pods[string(constants.LeaderComponentType)].Spec.Containers, "engine")
			Expect(leader).NotTo(BeNil(), "leader: engine container not found")
			Expect(hasVolume(pods[string(constants.LeaderComponentType)].Spec.Volumes, "ik-compat-vol")).
				To(BeTrue(), "leader: expected the profile volume in the rendered pod spec")
			// No patch dimension is expressed by this reference, so no
			// patch env var may appear on the engine.
			_, patched := envValue(leader, "IK_SITE")
			Expect(patched).To(BeFalse(), "leader: no patch env may fire on a profileName-only reference")

			By("verifying the worker template skips the sidecar stage entirely")
			worker := findContainer(pods[string(constants.WorkerComponentType)].Spec.Containers, "engine")
			Expect(worker).NotTo(BeNil(), "worker: the role's own engine container still renders")
			Expect(hasVolume(pods[string(constants.WorkerComponentType)].Spec.Volumes, "ik-compat-vol")).
				To(BeFalse(), "worker: the absent-component default is leader-only — no fragments may reach the worker template")

			By("verifying no engine-runtime event of any reason fires")
			Consistently(func(g Gomega) {
				g.Expect(noEngineRuntimeEvents()).To(BeTrue())
			}, 3*time.Second, targetingInterval).Should(Succeed())
		})

		It("withholds the patches of a patch-carrying profile and warns", func() {
			// A profile that later gains containerPatches must not change a
			// pre-existing reference's rendered output: the fragments still
			// land in the default component set (the leader only under
			// leaderWorkerPattern), the patches do not fire, and the
			// unaimed-patch warning event is emitted (asserted by presence —
			// reconcile loops legitimately repeat it).
			profile := makeTargetingProfile("ik-compat-patches",
				addOp("/env/-", corev1.EnvVar{Name: "IK_SITE", Value: "/opt/ik/site"}),
				addOp("/volumeMounts/-", corev1.VolumeMount{
					Name: "ik-compat-patches-vol", MountPath: "/opt/ik", ReadOnly: true,
				}),
			)
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-compat-patches", workloadsv1alpha2.EngineRuntime{
				ProfileName: "ik-compat-patches",
			})

			pods := rolePods(rbg.Name, rbg.Spec.Roles[0].Name, 2)
			leader := findContainer(pods[string(constants.LeaderComponentType)].Spec.Containers, "engine")
			Expect(leader).NotTo(BeNil(), "leader: engine container not found")
			Expect(hasVolume(pods[string(constants.LeaderComponentType)].Spec.Volumes, "ik-compat-patches-vol")).
				To(BeTrue(), "leader: the fragments must keep landing in the default component set")
			_, patched := envValue(leader, "IK_SITE")
			Expect(patched).To(BeFalse(), "leader: the patches must not fire without patchContainers consent")

			By("verifying the worker template skips the sidecar stage entirely")
			worker := findContainer(pods[string(constants.WorkerComponentType)].Spec.Containers, "engine")
			Expect(worker).NotTo(BeNil(), "worker: the role's own engine container still renders")
			Expect(hasVolume(pods[string(constants.WorkerComponentType)].Spec.Volumes, "ik-compat-patches-vol")).
				To(BeFalse(), "worker: the absent-component default is leader-only — no fragments may reach the worker template")

			By("verifying the unaimed-patch warning event fires")
			Eventually(func(g Gomega) {
				g.Expect(hasEvent(discovery.ReasonUnaimedContainerPatch)).To(BeTrue())
			}, targetingTimeout, targetingInterval).Should(Succeed())
		})
	})

	Context("scenario 2: a named component narrows the fragments", func() {
		It("delivers the fragments to the worker only", func() {
			profile := makeTargetingProfile("ik-narrow")
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-narrow", workloadsv1alpha2.EngineRuntime{
				ProfileName: "ik-narrow",
				Component:   string(constants.WorkerComponentType),
			})

			pods := rolePods(rbg.Name, rbg.Spec.Roles[0].Name, 2)
			Expect(hasVolume(pods[string(constants.WorkerComponentType)].Spec.Volumes, "ik-narrow-vol")).
				To(BeTrue(), "worker: expected the profile volume in the rendered pod spec")
			Expect(hasVolume(pods[string(constants.LeaderComponentType)].Spec.Volumes, "ik-narrow-vol")).
				To(BeFalse(), "leader: the fragments must not deliver outside the named component")

			By("verifying no engine-runtime event fires")
			Expect(noEngineRuntimeEvents()).To(BeTrue())
		})

		It("component: worker delivers pod-level fragments to the worker template (new capability)", func() {
			// D2: naming component: worker reaches a component main's
			// default never touches — main's worker injector chain omits the
			// sidecar stage outright, so this is scope EXTENSION, the first
			// surface to make worker-side pod-level injection reachable.
			// Every fragment kind lands: init container, sidecar and volume.
			profile := withInitContainer(
				withSidecar(makeTargetingProfile("ik-worker-cap"), "ik-sidecar"), "ik-init",
			)
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-worker-cap", workloadsv1alpha2.EngineRuntime{
				ProfileName: "ik-worker-cap",
				Component:   string(constants.WorkerComponentType),
			})

			pods := rolePods(rbg.Name, rbg.Spec.Roles[0].Name, 2)
			workerSpec := pods[string(constants.WorkerComponentType)].Spec
			Expect(findContainer(workerSpec.Containers, "ik-sidecar")).
				NotTo(BeNil(), "worker: the profile sidecar must render")
			Expect(findInitContainer(workerSpec.InitContainers, "ik-init")).
				NotTo(BeNil(), "worker: the profile init container must render")
			Expect(hasVolume(workerSpec.Volumes, "ik-worker-cap-vol")).
				To(BeTrue(), "worker: the profile volume must render")

			By("verifying the leader template stays untouched")
			leaderSpec := pods[string(constants.LeaderComponentType)].Spec
			Expect(findContainer(leaderSpec.Containers, "ik-sidecar")).
				To(BeNil(), "leader: the fragments must not deliver outside the named component")
			Expect(findInitContainer(leaderSpec.InitContainers, "ik-init")).
				To(BeNil(), "leader: the fragments must not deliver outside the named component")
			Expect(hasVolume(leaderSpec.Volumes, "ik-worker-cap-vol")).
				To(BeFalse(), "leader: the fragments must not deliver outside the named component")

			By("verifying no engine-runtime event fires")
			Expect(noEngineRuntimeEvents()).To(BeTrue())
		})
	})

	Context("scenario 3: consented patches within the named component", func() {
		It("profile-side: patchContainers fires the profile's patches on the named role container", func() {
			profile := makeTargetingProfile("ik-consent",
				addOp("/env/-", corev1.EnvVar{Name: "IK_SITE", Value: "/opt/ik/site"}),
				addOp("/volumeMounts/-", corev1.VolumeMount{
					Name: "ik-consent-vol", MountPath: "/opt/ik", ReadOnly: true,
				}),
			)
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-consent", workloadsv1alpha2.EngineRuntime{
				ProfileName:     "ik-consent",
				Component:       string(constants.WorkerComponentType),
				PatchContainers: []string{"engine"},
			})

			pods := rolePods(rbg.Name, rbg.Spec.Roles[0].Name, 2)
			worker := findContainer(pods[string(constants.WorkerComponentType)].Spec.Containers, "engine")
			leader := findContainer(pods[string(constants.LeaderComponentType)].Spec.Containers, "engine")
			Expect(worker).NotTo(BeNil())
			Expect(leader).NotTo(BeNil())

			By("verifying the worker engine gains both the env var and the mount")
			val, ok := envValue(worker, "IK_SITE")
			Expect(ok).To(BeTrue(), "worker: IK_SITE env var not found")
			Expect(val).To(Equal("/opt/ik/site"))
			Expect(worker.VolumeMounts).To(ContainElement(corev1.VolumeMount{
				Name: "ik-consent-vol", MountPath: "/opt/ik", ReadOnly: true,
			}))
			Expect(hasVolume(pods[string(constants.WorkerComponentType)].Spec.Volumes, "ik-consent-vol")).
				To(BeTrue(), "worker: the fragments render within the named component")

			By("verifying the leader engine gains neither")
			_, ok = envValue(leader, "IK_SITE")
			Expect(ok).To(BeFalse(), "leader: the patches must not fire outside the named component")
			Expect(hasMountName(leader.VolumeMounts, "ik-consent-vol")).
				To(BeFalse(), "leader: the profile volume must not be mounted")

			By("verifying no warning event fires — the reference consented")
			Expect(hasEvent(discovery.ReasonUnaimedContainerPatch)).To(BeFalse())
			Expect(hasEvent(discovery.ReasonEngineRuntimeRenderFailed)).To(BeFalse())
		})

		It("role-side: containerPatches fire the role's patches into the profile's sidecar", func() {
			profile := withSidecar(makeTargetingProfile("ik-rolepatch"), "ik-sidecar")
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-rolepatch", workloadsv1alpha2.EngineRuntime{
				ProfileName: "ik-rolepatch",
				Component:   string(constants.WorkerComponentType),
				ContainerPatches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
					roleAddOp("ik-sidecar", "/env/-", corev1.EnvVar{Name: "ROLE_FLAG", Value: "on"}),
				},
			})

			pods := rolePods(rbg.Name, rbg.Spec.Roles[0].Name, 2)
			workerSidecar := findContainer(pods[string(constants.WorkerComponentType)].Spec.Containers, "ik-sidecar")
			Expect(workerSidecar).NotTo(BeNil(), "worker: the profile sidecar must render in the named component")
			val, ok := envValue(workerSidecar, "ROLE_FLAG")
			Expect(ok).To(BeTrue(), "worker: ROLE_FLAG env var not found on the sidecar")
			Expect(val).To(Equal("on"))

			By("verifying the sidecar renders in the worker only")
			Expect(findContainer(pods[string(constants.LeaderComponentType)].Spec.Containers, "ik-sidecar")).
				To(BeNil(), "leader: the fragments must not deliver outside the named component")

			By("verifying no warning event fires — writing the rules is the consent")
			Expect(hasEvent(discovery.ReasonUnaimedContainerPatch)).To(BeFalse())
			Expect(hasEvent(discovery.ReasonEngineRuntimeRenderFailed)).To(BeFalse())
		})

		It("role-side: containerPatches address profile init containers too", func() {
			profile := withInitContainer(makeTargetingProfile("ik-init-patch"), "ik-init", "--seed")
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-init", workloadsv1alpha2.EngineRuntime{
				ProfileName: "ik-init-patch",
				Component:   string(constants.WorkerComponentType),
				ContainerPatches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
					roleAddOp("ik-init", "/args/-", "--ik-warm"),
				},
			})

			pods := rolePods(rbg.Name, rbg.Spec.Roles[0].Name, 2)
			workerInit := findInitContainer(pods[string(constants.WorkerComponentType)].Spec.InitContainers, "ik-init")
			Expect(workerInit).NotTo(BeNil(), "worker: the profile init container must render in the named component")
			Expect(workerInit.Args).To(Equal([]string{"--seed", "--ik-warm"}),
				"worker: the role-side patch must append its arg to the profile init container")

			By("verifying the init container renders in the worker only")
			Expect(findInitContainer(pods[string(constants.LeaderComponentType)].Spec.InitContainers, "ik-init")).
				To(BeNil(), "leader: the fragments must not deliver outside the named component")

			By("verifying no warning event fires")
			Expect(hasEvent(discovery.ReasonUnaimedContainerPatch)).To(BeFalse())
			Expect(hasEvent(discovery.ReasonEngineRuntimeRenderFailed)).To(BeFalse())
		})
	})

	Context("render errors", func() {
		It("D6: two same-profile references naming one component create no child", func() {
			profile := makeTargetingProfile("ik-d6")
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-d6",
				workloadsv1alpha2.EngineRuntime{ProfileName: "ik-d6", Component: string(constants.LeaderComponentType)},
				workloadsv1alpha2.EngineRuntime{ProfileName: "ik-d6", Component: string(constants.LeaderComponentType)},
			)

			By("verifying no RoleInstanceSet is created for the role")
			noRoleInstanceSet(rbg)

			By("verifying the render-failed warning event fires")
			Eventually(func(g Gomega) {
				g.Expect(hasEvent(discovery.ReasonEngineRuntimeRenderFailed)).To(BeTrue())
			}, targetingTimeout, targetingInterval).Should(Succeed())
		})

		It("a reference naming an unknown component creates no child", func() {
			profile := makeTargetingProfile("ik-bogus")
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-bogus", workloadsv1alpha2.EngineRuntime{
				ProfileName: "ik-bogus",
				Component:   "bogus", // no such component under leaderWorkerPattern
			})

			By("verifying no RoleInstanceSet is created for the role")
			noRoleInstanceSet(rbg)

			By("verifying the render-failed warning event fires")
			Eventually(func(g Gomega) {
				g.Expect(hasEvent(discovery.ReasonEngineRuntimeRenderFailed)).To(BeTrue())
			}, targetingTimeout, targetingInterval).Should(Succeed())
		})

		It("an explicit component on a standalone role creates no child", func() {
			profile := makeTargetingProfile("ik-standalone-pods")
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			Expect(testutil.K8sClient.Create(testutil.Ctx, standaloneRBG("targeting-standalone", workloadsv1alpha2.EngineRuntime{
				ProfileName: "ik-standalone-pods",
				Component:   string(constants.LeaderComponentType), // a standalonePattern role has no component vocabulary
			}))).To(Succeed())

			By("verifying no Deployment is created")
			Consistently(func(g Gomega) {
				g.Expect(listDeployments()).To(BeEmpty())
			}, 5*time.Second, targetingInterval).Should(Succeed())

			By("verifying the render-failed warning event fires")
			Eventually(func(g Gomega) {
				g.Expect(hasEvent(discovery.ReasonEngineRuntimeRenderFailed)).To(BeTrue())
			}, targetingTimeout, targetingInterval).Should(Succeed())
		})

		It("a patchContainers entry naming a profile-injected container creates no child", func() {
			// The profile's patches may not fire into a container the
			// profile itself injected — only into containers the role's
			// own template declares.
			profile := withSidecar(makeTargetingProfile("ik-injected",
				addOp("/env/-", corev1.EnvVar{Name: "IK_SITE", Value: "/opt/ik/site"}),
			), "ik-sidecar")
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-injected", workloadsv1alpha2.EngineRuntime{
				ProfileName:     "ik-injected",
				Component:       string(constants.WorkerComponentType),
				PatchContainers: []string{"ik-sidecar"}, // profile-declared, not role-declared
			})

			By("verifying no RoleInstanceSet is created for the role")
			noRoleInstanceSet(rbg)

			By("verifying the render-failed warning event fires")
			Eventually(func(g Gomega) {
				g.Expect(hasEvent(discovery.ReasonEngineRuntimeRenderFailed)).To(BeTrue())
			}, targetingTimeout, targetingInterval).Should(Succeed())
		})

		It("a role-side patch redeclaring the sidecar's env creates no child", func() {
			// Coexistence conflict: the profile sidecar declares FROM_PROFILE,
			// and the role-side patch adds an env of the same name — add
			// cannot join values, so the render refuses.
			profile := withSidecar(makeTargetingProfile("ik-conflict"), "ik-sidecar",
				corev1.EnvVar{Name: "FROM_PROFILE", Value: "profile"},
			)
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-conflict", workloadsv1alpha2.EngineRuntime{
				ProfileName: "ik-conflict",
				Component:   string(constants.WorkerComponentType),
				ContainerPatches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
					roleAddOp("ik-sidecar", "/env/-", corev1.EnvVar{Name: "FROM_PROFILE", Value: "role"}),
				},
			})

			By("verifying no RoleInstanceSet is created for the role")
			noRoleInstanceSet(rbg)

			By("verifying the render-failed warning event fires")
			Eventually(func(g Gomega) {
				g.Expect(hasEvent(discovery.ReasonEngineRuntimeRenderFailed)).To(BeTrue())
			}, targetingTimeout, targetingInterval).Should(Succeed())
		})

		It("D9: a role-side patch mounting a role-declared volume into a profile container creates no child", func() {
			// One-way content flow: profile-declared containers consume only
			// profile-declared content. The role template declares role-vol
			// and the role-side patch aims it at the profile's sidecar —
			// refused at render time, so no role volume ever lands in a
			// publisher-controlled container.
			profile := withSidecar(makeTargetingProfile("ik-d9-vol"), "ik-sidecar")
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			role := leaderWorkerRole(workloadsv1alpha2.EngineRuntime{
				ProfileName: "ik-d9-vol",
				Component:   string(constants.WorkerComponentType),
				ContainerPatches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
					roleAddOp("ik-sidecar", "/volumeMounts/-", corev1.VolumeMount{
						Name: "role-vol", MountPath: "/mnt/role",
					}),
				},
			})
			base := role.Pattern.LeaderWorkerPattern.TemplateSource.Template
			base.Spec.Volumes = append(base.Spec.Volumes, corev1.Volume{
				Name:         "role-vol",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			})
			rbg := createLeaderWorkerRBG("targeting-d9-vol", role)

			By("verifying no RoleInstanceSet is created for the role")
			noRoleInstanceSet(rbg)

			By("verifying the render-failed warning event fires")
			Eventually(func(g Gomega) {
				g.Expect(hasEvent(discovery.ReasonEngineRuntimeRenderFailed)).To(BeTrue())
			}, targetingTimeout, targetingInterval).Should(Succeed())
		})

		It("D9: a role-side patch injecting a secretKeyRef env into a profile container creates no child", func() {
			// The other D9 refusal: a consuming-namespace secret reference
			// injected into a profile-declared container would steer a role
			// secret into a publisher-controlled image — a render error.
			profile := withSidecar(makeTargetingProfile("ik-d9-secret"), "ik-sidecar")
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-d9-secret", workloadsv1alpha2.EngineRuntime{
				ProfileName: "ik-d9-secret",
				Component:   string(constants.WorkerComponentType),
				ContainerPatches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
					roleAddOp("ik-sidecar", "/env/-", corev1.EnvVar{
						Name: "ROLE_SECRET",
						ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "role-secret"},
							Key:                  "token",
						}},
					}),
				},
			})

			By("verifying no RoleInstanceSet is created for the role")
			noRoleInstanceSet(rbg)

			By("verifying the render-failed warning event fires")
			Eventually(func(g Gomega) {
				g.Expect(hasEvent(discovery.ReasonEngineRuntimeRenderFailed)).To(BeTrue())
			}, targetingTimeout, targetingInterval).Should(Succeed())
		})

		It("a legacy containers override redeclaring the profile's env creates no child", func() {
			// The legacy containers merge joins the shared env-name conflict
			// check: the profile sidecar already declares FROM_PROFILE, and
			// the role-side override appends the same name — the merge
			// cannot join values, so the render refuses instead of writing
			// a silent second entry.
			profile := withSidecar(makeTargetingProfile("ik-legacy-conflict"), "ik-sidecar",
				corev1.EnvVar{Name: "FROM_PROFILE", Value: "profile"},
			)
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-legacy-conflict", workloadsv1alpha2.EngineRuntime{
				ProfileName: "ik-legacy-conflict",
				Component:   string(constants.WorkerComponentType),
				Containers: []corev1.Container{{
					Name: "ik-sidecar",
					Env:  []corev1.EnvVar{{Name: "FROM_PROFILE", Value: "role"}},
				}},
			})

			By("verifying no RoleInstanceSet is created for the role")
			noRoleInstanceSet(rbg)

			By("verifying the render-failed warning event fires")
			Eventually(func(g Gomega) {
				g.Expect(hasEvent(discovery.ReasonEngineRuntimeRenderFailed)).To(BeTrue())
			}, targetingTimeout, targetingInterval).Should(Succeed())
		})

		It("duplicate references appending the same legacy env name conflict", func() {
			// The conflict check spans earlier references too: the first
			// compatibility-shape reference's override appends SHARED, and
			// the second's finds the name already declared — a render
			// error, never two entries of one name.
			profile := withSidecar(makeTargetingProfile("ik-legacy-dup"), "ik-sidecar")
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			override := workloadsv1alpha2.EngineRuntime{
				ProfileName: "ik-legacy-dup",
				Containers: []corev1.Container{{
					Name: "ik-sidecar",
					Env:  []corev1.EnvVar{{Name: "SHARED", Value: "v"}},
				}},
			}
			rbg := leaderWorkerRBG("targeting-legacy-dup", override, override)

			By("verifying no RoleInstanceSet is created for the role")
			noRoleInstanceSet(rbg)

			By("verifying the render-failed warning event fires")
			Eventually(func(g Gomega) {
				g.Expect(hasEvent(discovery.ReasonEngineRuntimeRenderFailed)).To(BeTrue())
			}, targetingTimeout, targetingInterval).Should(Succeed())
		})
	})

	Context("scope ownership exemptions and coexistence", func() {
		It("duplicate profileName-only references render as on main (D6 exemption)", func() {
			// A pure compatibility reference owns no component, so two of
			// them to the same profile stay legal — exactly what main
			// accepted. Fragment injection is name-keyed and idempotent, and
			// the absent-component default stays leader-only: neither
			// reference reaches the worker template.
			profile := makeTargetingProfile("ik-dup")
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-dup",
				workloadsv1alpha2.EngineRuntime{ProfileName: "ik-dup"},
				workloadsv1alpha2.EngineRuntime{ProfileName: "ik-dup"},
			)

			pods := rolePods(rbg.Name, rbg.Spec.Roles[0].Name, 2)
			Expect(hasVolume(pods[string(constants.LeaderComponentType)].Spec.Volumes, "ik-dup-vol")).
				To(BeTrue(), "leader: expected the profile volume rendered in the pod spec")
			Expect(hasVolume(pods[string(constants.WorkerComponentType)].Spec.Volumes, "ik-dup-vol")).
				To(BeFalse(), "worker: the absent-component default is leader-only even for duplicate references")

			By("verifying no engine-runtime event fires")
			Expect(noEngineRuntimeEvents()).To(BeTrue())
		})

		It("a pure-compat and a component-scoped reference to one profile coexist", func() {
			// The D6 rule binds component-scoped references only; a
			// profileName-only reference owns nothing, so the pair resolves.
			// The compat reference expands to the pattern's default set —
			// the leader only under leaderWorkerPattern — which the
			// component-scoped reference here also names, so the fragments
			// land in the leader once and the worker template is untouched.
			profile := makeTargetingProfile("ik-mixed")
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-mixed",
				workloadsv1alpha2.EngineRuntime{ProfileName: "ik-mixed"},
				workloadsv1alpha2.EngineRuntime{
					ProfileName: "ik-mixed",
					Component:   string(constants.LeaderComponentType),
				},
			)

			pods := rolePods(rbg.Name, rbg.Spec.Roles[0].Name, 2)
			Expect(hasVolume(pods[string(constants.LeaderComponentType)].Spec.Volumes, "ik-mixed-vol")).
				To(BeTrue(), "leader: the compat reference's fragments reach the default component set")
			Expect(hasVolume(pods[string(constants.WorkerComponentType)].Spec.Volumes, "ik-mixed-vol")).
				To(BeFalse(), "worker: neither the compat reference (leader-only default) nor the leader-scoped one delivers here")

			By("verifying no render-failed event fires")
			Expect(hasEvent(discovery.ReasonEngineRuntimeRenderFailed)).To(BeFalse())
		})

		It("legacy containers merge and role-side patches coexist on one sidecar", func() {
			// §5.5 order: fragments and the legacy containers merge land
			// first, then the role-side patches — so all three env names
			// must be present on the rendered sidecar.
			profile := withSidecar(makeTargetingProfile("ik-coexist"), "ik-sidecar",
				corev1.EnvVar{Name: "FROM_PROFILE", Value: "profile"},
			)
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-coexist", workloadsv1alpha2.EngineRuntime{
				ProfileName: "ik-coexist",
				Component:   string(constants.WorkerComponentType),
				Containers: []corev1.Container{{
					Name: "ik-sidecar",
					Env:  []corev1.EnvVar{{Name: "FROM_LEGACY", Value: "legacy"}},
				}},
				ContainerPatches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
					roleAddOp("ik-sidecar", "/env/-", corev1.EnvVar{Name: "FROM_ROLE", Value: "role"}),
				},
			})

			pods := rolePods(rbg.Name, rbg.Spec.Roles[0].Name, 2)
			sidecar := findContainer(pods[string(constants.WorkerComponentType)].Spec.Containers, "ik-sidecar")
			Expect(sidecar).NotTo(BeNil(), "worker: the profile sidecar must render in the named component")
			for _, name := range []string{"FROM_PROFILE", "FROM_LEGACY", "FROM_ROLE"} {
				_, ok := envValue(sidecar, name)
				Expect(ok).To(BeTrue(), "worker: env var %s not found on the sidecar", name)
			}

			By("verifying the coexistence stays inside the named component")
			Expect(findContainer(pods[string(constants.LeaderComponentType)].Spec.Containers, "ik-sidecar")).
				To(BeNil(), "leader: the fragments must not deliver outside the named component")

			By("verifying no warning event fires")
			Expect(hasEvent(discovery.ReasonUnaimedContainerPatch)).To(BeFalse())
			Expect(hasEvent(discovery.ReasonEngineRuntimeRenderFailed)).To(BeFalse())
		})

		It("D9 permitted direction: a role-side patch mounting the profile's own volume into the sidecar renders", func() {
			// The one-way bound blocks role content only. The profile's own
			// volume mounted into its own container is the profile
			// consuming profile content, so the render succeeds — the
			// guard must not over-refuse.
			profile := withSidecar(makeTargetingProfile("ik-d9-ok"), "ik-sidecar")
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-d9-ok", workloadsv1alpha2.EngineRuntime{
				ProfileName: "ik-d9-ok",
				Component:   string(constants.WorkerComponentType),
				ContainerPatches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
					roleAddOp("ik-sidecar", "/volumeMounts/-", corev1.VolumeMount{
						Name: "ik-d9-ok-vol", MountPath: "/opt/ik", ReadOnly: true,
					}),
				},
			})

			pods := rolePods(rbg.Name, rbg.Spec.Roles[0].Name, 2)
			sidecar := findContainer(pods[string(constants.WorkerComponentType)].Spec.Containers, "ik-sidecar")
			Expect(sidecar).NotTo(BeNil(), "worker: the profile sidecar must render in the named component")
			Expect(hasMountName(sidecar.VolumeMounts, "ik-d9-ok-vol")).
				To(BeTrue(), "worker: the profile's own volume may mount into its own container")

			By("verifying no render-failed event fires")
			Expect(hasEvent(discovery.ReasonEngineRuntimeRenderFailed)).To(BeFalse())
		})
	})

	Context("application order and positional shadowing (D10)", func() {
		It("a role-side positional insert ahead of a profile-side one warns and still renders", func() {
			// D10: the profile-side pass applies before the role-side pass,
			// so what the role writes lands where it is written — its
			// /env/0 insert lands ahead of the profile's. The render
			// succeeds, and an EngineRuntimePositionalShadowing warning
			// event is emitted so the shadowing is never silent.
			profile := makeTargetingProfile("ik-shadow",
				addOp("/env/0", corev1.EnvVar{Name: "PROFILE_FIRST", Value: "profile"}),
			)
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-shadow", workloadsv1alpha2.EngineRuntime{
				ProfileName:     "ik-shadow",
				Component:       string(constants.WorkerComponentType),
				PatchContainers: []string{"engine"},
				ContainerPatches: []workloadsv1alpha2.EngineRuntimeContainerPatch{
					roleAddOp("engine", "/env/0", corev1.EnvVar{Name: "ROLE_FIRST", Value: "role"}),
				},
			})

			pods := rolePods(rbg.Name, rbg.Spec.Roles[0].Name, 2)
			engine := findContainer(pods[string(constants.WorkerComponentType)].Spec.Containers, "engine")
			Expect(engine).NotTo(BeNil(), "worker: engine container not found")
			roleIdx, roleOK := envIndex(engine, "ROLE_FIRST")
			profileIdx, profileOK := envIndex(engine, "PROFILE_FIRST")
			Expect(roleOK).To(BeTrue(), "worker: ROLE_FIRST env var not found")
			Expect(profileOK).To(BeTrue(), "worker: PROFILE_FIRST env var not found")
			Expect(roleIdx).To(BeNumerically("<", profileIdx),
				"the role-side pass runs last, so its insert lands ahead of the profile-side one")

			By("verifying the shadowing warning event fires")
			Eventually(func(g Gomega) {
				g.Expect(hasEvent(discovery.ReasonEngineRuntimePositionalShadowing)).To(BeTrue())
			}, targetingTimeout, targetingInterval).Should(Succeed())

			By("verifying the render succeeded — no render-failed event fires")
			Expect(hasEvent(discovery.ReasonEngineRuntimeRenderFailed)).To(BeFalse())
		})
	})

	Context("RBG render-failure condition", func() {
		It("surfaces EngineRuntimeRendered=False on failure and recovers to True", func() {
			// KEP-531 validation layer split: render errors surface as RBG
			// conditions plus events. The condition is the durable surface —
			// it appears with the failure, and a fixed reference recovers it
			// to True on the next reconcile.
			profile := makeTargetingProfile("ik-cond",
				addOp("/env/-", corev1.EnvVar{Name: "IK_SITE", Value: "/opt/ik/site"}),
			)
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-cond", workloadsv1alpha2.EngineRuntime{
				ProfileName:     "ik-cond",
				Component:       string(constants.WorkerComponentType),
				PatchContainers: []string{"ghost"}, // no such container
			})

			By("verifying no RoleInstanceSet is created for the role")
			noRoleInstanceSet(rbg)

			By("verifying the EngineRuntimeRendered condition turns False")
			key := types.NamespacedName{Name: rbg.Name, Namespace: testNs}
			Eventually(func(g Gomega) {
				fetched := &workloadsv1alpha2.RoleBasedGroup{}
				g.Expect(testutil.K8sClient.Get(testutil.Ctx, key, fetched)).To(Succeed())
				cond := apimeta.FindStatusCondition(
					fetched.Status.Conditions, string(workloadsv1alpha2.RoleBasedGroupEngineRuntimeRendered))
				g.Expect(cond).NotTo(BeNil(), "EngineRuntimeRendered condition not found")
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal(discovery.ReasonEngineRuntimeRenderFailed))
			}, targetingTimeout, targetingInterval).Should(Succeed())

			By("fixing the reference's consent and verifying the condition recovers to True")
			current := &workloadsv1alpha2.RoleBasedGroup{}
			Expect(testutil.K8sClient.Get(testutil.Ctx, key, current)).To(Succeed())
			current.Spec.Roles[0].EngineRuntimes[0].PatchContainers = []string{"engine"}
			Expect(testutil.K8sClient.Update(testutil.Ctx, current)).To(Succeed())

			Eventually(func(g Gomega) {
				fetched := &workloadsv1alpha2.RoleBasedGroup{}
				g.Expect(testutil.K8sClient.Get(testutil.Ctx, key, fetched)).To(Succeed())
				cond := apimeta.FindStatusCondition(
					fetched.Status.Conditions, string(workloadsv1alpha2.RoleBasedGroupEngineRuntimeRendered))
				g.Expect(cond).NotTo(BeNil(), "EngineRuntimeRendered condition not found")
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(cond.Reason).To(Equal("EngineRuntimeRendered"))
			}, targetingTimeout, targetingInterval).Should(Succeed())

			By("verifying the recovered render actually delivers the patch")
			pods := rolePods(rbg.Name, rbg.Spec.Roles[0].Name, 2)
			engine := findContainer(pods[string(constants.WorkerComponentType)].Spec.Containers, "engine")
			Expect(engine).NotTo(BeNil(), "worker: engine container not found")
			val, ok := envValue(engine, "IK_SITE")
			Expect(ok).To(BeTrue(), "worker: IK_SITE env var not found after recovery")
			Expect(val).To(Equal("/opt/ik/site"))
		})
	})

})

// findInitContainer returns the named spec.initContainers member, if present.
func findInitContainer(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

// envIndex returns the position of the named env var in the container's env
// list, and whether it is present — the positional material of the D10
// application-order assertions.
func envIndex(container *corev1.Container, name string) (int, bool) {
	for i := range container.Env {
		if container.Env[i].Name == name {
			return i, true
		}
	}
	return -1, false
}
