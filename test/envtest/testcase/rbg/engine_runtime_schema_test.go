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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	workloadsv1alpha2 "sigs.k8s.io/rbgs/api/workloads/v1alpha2"
	"sigs.k8s.io/rbgs/test/envtest/testutil"
)

var _ = Describe("RoleBasedGroup engine runtime schema", func() {
	const (
		timeout  = time.Second * 30
		interval = time.Millisecond * 250
	)

	var testNs string

	BeforeEach(func() {
		testNs = fmt.Sprintf("test-erts-%d", time.Now().UnixNano())
		testutil.CreateNamespace(testNs)
	})
	AfterEach(func() {
		testutil.DeleteNamespace(testNs)
	})

	makeSchemaRBG := func(name string, rt workloadsv1alpha2.EngineRuntime) *workloadsv1alpha2.RoleBasedGroup {
		return &workloadsv1alpha2.RoleBasedGroup{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNs},
			Spec: workloadsv1alpha2.RoleBasedGroupSpec{
				Roles: []workloadsv1alpha2.RoleSpec{{
					Name: "worker",
					Pattern: workloadsv1alpha2.Pattern{
						StandalonePattern: &workloadsv1alpha2.StandalonePattern{
							TemplateSource: workloadsv1alpha2.TemplateSource{
								Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
									Name:  "engine",
									Image: "nginx:latest",
								}}}},
							},
						},
					},
					EngineRuntimes: []workloadsv1alpha2.EngineRuntime{rt},
				}},
			},
		}
	}

	makeSchemaRBGLW := func(name string, rt workloadsv1alpha2.EngineRuntime) *workloadsv1alpha2.RoleBasedGroup {
		return &workloadsv1alpha2.RoleBasedGroup{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNs},
			Spec: workloadsv1alpha2.RoleBasedGroupSpec{
				Roles: []workloadsv1alpha2.RoleSpec{{
					Name: "worker",
					Pattern: workloadsv1alpha2.Pattern{
						LeaderWorkerPattern: &workloadsv1alpha2.LeaderWorkerPattern{
							Size: ptr.To(int32(1)),
							TemplateSource: workloadsv1alpha2.TemplateSource{
								Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
									Name:  "engine",
									Image: "nginx:latest",
								}}}},
							},
						},
					},
					EngineRuntimes: []workloadsv1alpha2.EngineRuntime{rt},
				}},
			},
		}
	}

	expectRejected := func(name string, rt workloadsv1alpha2.EngineRuntime, substring string) {
		Expect(testutil.K8sClient.Create(testutil.Ctx, makeSchemaRBG(name, rt))).
			NotTo(Succeed(), "%s must be rejected at write time", name)
		// re-run to capture the error message
		err := testutil.K8sClient.Create(testutil.Ctx, makeSchemaRBG(name, rt))
		ExpectWithOffset(1, err).To(HaveOccurred())
		ExpectWithOffset(1, err.Error()).To(ContainSubstring(substring))
	}

	expectAccepted := func(name string, rt workloadsv1alpha2.EngineRuntime) {
		ExpectWithOffset(1, testutil.K8sClient.Create(testutil.Ctx, makeSchemaRBG(name, rt))).To(Succeed())
	}

	expectRejectedLW := func(name string, rt workloadsv1alpha2.EngineRuntime, substring string) {
		Expect(testutil.K8sClient.Create(testutil.Ctx, makeSchemaRBGLW(name, rt))).
			NotTo(Succeed(), "%s must be rejected at write time", name)
		// re-run to capture the error message
		err := testutil.K8sClient.Create(testutil.Ctx, makeSchemaRBGLW(name, rt))
		ExpectWithOffset(1, err).To(HaveOccurred())
		ExpectWithOffset(1, err.Error()).To(ContainSubstring(substring))
	}

	expectAcceptedLW := func(name string, rt workloadsv1alpha2.EngineRuntime) {
		ExpectWithOffset(1, testutil.K8sClient.Create(testutil.Ctx, makeSchemaRBGLW(name, rt))).To(Succeed())
	}

	It("rejects a component that is not a DNS label", func() {
		// Pattern-marker violations surface as the structural-schema
		// "should match" error, not a CEL message.
		expectRejected("bad-component", workloadsv1alpha2.EngineRuntime{
			ProfileName: "p", Component: "Bad_Name",
		}, "component in body should match '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$'")
	})

	It("accepts a legal component", func() {
		expectAcceptedLW("component-ok", workloadsv1alpha2.EngineRuntime{
			ProfileName: "p", Component: "worker",
		})
	})

	It("rejects a patchContainers entry that is not a DNS label", func() {
		expectRejectedLW("bad-pc", workloadsv1alpha2.EngineRuntime{
			ProfileName: "p", Component: "worker", PatchContainers: []string{"Bad_Shape"},
		}, "container names must be DNS labels")
	})

	It("rejects more than 32 patchContainers entries", func() {
		names := make([]string, 33)
		for i := range names {
			names[i] = "engine"
		}
		expectRejectedLW("33-pc", workloadsv1alpha2.EngineRuntime{
			ProfileName: "p", Component: "worker", PatchContainers: names,
		}, "32")
	})

	It("rejects a role-side patch entry without jsonPatch", func() {
		expectRejectedLW("no-op", workloadsv1alpha2.EngineRuntime{
			ProfileName: "p", Component: "worker",
			ContainerPatches: []workloadsv1alpha2.EngineRuntimeContainerPatch{{Container: "sidecar"}},
		}, "exactly one patch field must be set")
	})

	It("rejects a role-side patch entry with a container that is not a DNS label", func() {
		expectRejectedLW("bad-c", workloadsv1alpha2.EngineRuntime{
			ProfileName: "p", Component: "worker",
			ContainerPatches: []workloadsv1alpha2.EngineRuntimeContainerPatch{{
				Container: "Bad_Name",
				JSONPatch: &workloadsv1alpha2.ContainerJSONPatch{Op: "add", Path: "/env/-",
					Value: runtime.RawExtension{Raw: []byte(`{"name":"A","value":"b"}`)}},
			}},
		}, "container in body should match '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$'")
	})

	It("rejects a role-side patch path outside the allowlist", func() {
		expectRejectedLW("bad-path", workloadsv1alpha2.EngineRuntime{
			ProfileName: "p", Component: "worker",
			ContainerPatches: []workloadsv1alpha2.EngineRuntimeContainerPatch{{
				Container: "sidecar",
				JSONPatch: &workloadsv1alpha2.ContainerJSONPatch{Op: "add", Path: "/image",
					Value: runtime.RawExtension{Raw: []byte(`"x"`)}},
			}},
		}, "path must be an element pointer under /command, /args, /env, /volumeMounts or /securityContext/capabilities/add")
	})

	It("D7: rejects patchContainers without component", func() {
		expectRejectedLW("d7-pc", workloadsv1alpha2.EngineRuntime{
			ProfileName: "p", PatchContainers: []string{"engine"},
		}, "patchContainers and containerPatches require component to be set")
	})

	It("D7: rejects containerPatches without component", func() {
		expectRejectedLW("d7-cp", workloadsv1alpha2.EngineRuntime{
			ProfileName: "p",
			ContainerPatches: []workloadsv1alpha2.EngineRuntimeContainerPatch{{
				Container: "sidecar",
				JSONPatch: &workloadsv1alpha2.ContainerJSONPatch{Op: "add", Path: "/env/-",
					Value: runtime.RawExtension{Raw: []byte(`{"name":"A","value":"b"}`)}},
			}},
		}, "patchContainers and containerPatches require component to be set")
	})

	It("D7: rejects both patch fields together without component", func() {
		expectRejectedLW("d7-both", workloadsv1alpha2.EngineRuntime{
			ProfileName:     "p",
			PatchContainers: []string{"engine"},
			ContainerPatches: []workloadsv1alpha2.EngineRuntimeContainerPatch{{
				Container: "sidecar",
				JSONPatch: &workloadsv1alpha2.ContainerJSONPatch{Op: "add", Path: "/env/-",
					Value: runtime.RawExtension{Raw: []byte(`{"name":"A","value":"b"}`)}},
			}},
		}, "patchContainers and containerPatches require component to be set")
	})

	It("D7: accepts both patch fields with component (scenario 3)", func() {
		expectAcceptedLW("d7-ok", workloadsv1alpha2.EngineRuntime{
			ProfileName: "p", Component: "worker",
			PatchContainers: []string{"engine"},
			ContainerPatches: []workloadsv1alpha2.EngineRuntimeContainerPatch{{
				Container: "sidecar",
				JSONPatch: &workloadsv1alpha2.ContainerJSONPatch{Op: "add", Path: "/env/-",
					Value: runtime.RawExtension{Raw: []byte(`{"name":"A","value":"b"}`)}},
			}},
		})
	})

	It("scenario 1: accepts a profileName-only reference", func() {
		expectAccepted("compat-only", workloadsv1alpha2.EngineRuntime{ProfileName: "p"})
	})

	It("injectContainers stays accepted with arbitrary values (inert field)", func() {
		expectAccepted("inert-ok", workloadsv1alpha2.EngineRuntime{
			ProfileName: "p", InjectContainers: []string{"whatever/shape", "Bad_Shape"},
		})
	})
})
