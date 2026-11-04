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
// absent component expands to every component (main's behavior — scenario 1);
// a named component scopes fragments and legacy merge to that one component
// (scenario 2); patchContainers consents the profile's patches and
// containerPatches carries the role's patches, both within the named
// component (scenario 3).
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

	// leaderWorkerRBG creates a v1alpha2 RBG whose single role is a
	// leaderWorkerPattern role (the RoleInstanceSet path) referencing the
	// given engine runtimes. BuildLeaderWorkerRole declares an `nginx`
	// container in the shared base template; the targeting semantics
	// address it as `engine`, so the fixture renames it and gives it a
	// PYTHONPATH env the profile must not collide with.
	leaderWorkerRBG := func(name string, runtimes ...workloadsv1alpha2.EngineRuntime) *workloadsv1alpha2.RoleBasedGroup {
		role := wrappersv2.BuildLeaderWorkerRole("worker").Obj()
		base := role.Pattern.LeaderWorkerPattern.TemplateSource.Template
		base.Spec.Containers[0].Name = "engine"
		base.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "PYTHONPATH", Value: "/usr/lib/python"}}
		role.EngineRuntimes = runtimes

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
			!hasEvent(discovery.ReasonEngineRuntimeRenderFailed)
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
		It("delivers the fragments to every component with no events (patch-free profile)", func() {
			profile := makeTargetingProfile("ik-compat")
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-compat", workloadsv1alpha2.EngineRuntime{
				ProfileName: "ik-compat",
			})

			pods := rolePods(rbg.Name, rbg.Spec.Roles[0].Name, 2)
			for _, component := range []string{
				string(constants.LeaderComponentType), string(constants.WorkerComponentType),
			} {
				engine := findContainer(pods[component].Spec.Containers, "engine")
				Expect(engine).NotTo(BeNil(), "%s: engine container not found", component)
				Expect(hasVolume(pods[component].Spec.Volumes, "ik-compat-vol")).
					To(BeTrue(), "%s: expected the profile volume in the rendered pod spec", component)
				// No patch dimension is expressed by this reference, so no
				// patch env var may appear on the engine.
				_, patched := envValue(engine, "IK_SITE")
				Expect(patched).To(BeFalse(), "%s: no patch env may fire on a profileName-only reference", component)
			}

			By("verifying no engine-runtime event of any reason fires")
			Consistently(func(g Gomega) {
				g.Expect(noEngineRuntimeEvents()).To(BeTrue())
			}, 3*time.Second, targetingInterval).Should(Succeed())
		})

		It("withholds the patches of a patch-carrying profile and warns", func() {
			// A profile that later gains containerPatches must not change a
			// pre-existing reference's rendered output: the fragments still
			// land in every component, the patches do not fire, and the
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
			for _, component := range []string{
				string(constants.LeaderComponentType), string(constants.WorkerComponentType),
			} {
				engine := findContainer(pods[component].Spec.Containers, "engine")
				Expect(engine).NotTo(BeNil(), "%s: engine container not found", component)
				Expect(hasVolume(pods[component].Spec.Volumes, "ik-compat-patches-vol")).
					To(BeTrue(), "%s: the fragments must keep landing in every component", component)
				_, patched := envValue(engine, "IK_SITE")
				Expect(patched).To(BeFalse(), "%s: the patches must not fire without patchContainers consent", component)
			}

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
	})

	Context("scope ownership exemptions and coexistence", func() {
		It("duplicate profileName-only references render as on main (D6 exemption)", func() {
			// A pure compatibility reference owns no component, so two of
			// them to the same profile stay legal — exactly what main
			// accepted. Fragment injection is name-keyed and idempotent.
			profile := makeTargetingProfile("ik-dup")
			Expect(testutil.K8sClient.Create(testutil.Ctx, profile)).To(Succeed())
			rbg := leaderWorkerRBG("targeting-dup",
				workloadsv1alpha2.EngineRuntime{ProfileName: "ik-dup"},
				workloadsv1alpha2.EngineRuntime{ProfileName: "ik-dup"},
			)

			pods := rolePods(rbg.Name, rbg.Spec.Roles[0].Name, 2)
			for _, component := range []string{
				string(constants.LeaderComponentType), string(constants.WorkerComponentType),
			} {
				Expect(hasVolume(pods[component].Spec.Volumes, "ik-dup-vol")).
					To(BeTrue(), "%s: expected the profile volume rendered in the pod spec", component)
			}

			By("verifying no engine-runtime event fires")
			Expect(noEngineRuntimeEvents()).To(BeTrue())
		})

		It("a pure-compat and a component-scoped reference to one profile coexist", func() {
			// The D6 rule binds component-scoped references only; a
			// profileName-only reference owns nothing, so the pair resolves
			// and the compat reference still expands to every component.
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
			for _, component := range []string{
				string(constants.LeaderComponentType), string(constants.WorkerComponentType),
			} {
				Expect(hasVolume(pods[component].Spec.Volumes, "ik-mixed-vol")).
					To(BeTrue(), "%s: the compat reference's fragments reach every component", component)
			}

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
