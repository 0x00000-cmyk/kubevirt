//nolint:dupl,lll
package revision_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/golang/mock/gomock"
	appsv1 "k8s.io/api/apps/v1"
	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	k8sfake "k8s.io/client-go/kubernetes/fake"

	"kubevirt.io/client-go/kubecli"

	"kubevirt.io/kubevirt/pkg/instancetype"
	"kubevirt.io/kubevirt/pkg/instancetype/conflict"
	"kubevirt.io/kubevirt/pkg/instancetype/revision"
	"kubevirt.io/kubevirt/pkg/testutils"

	virtv1 "kubevirt.io/api/core/v1"
	apiinstancetype "kubevirt.io/api/instancetype"
	instancetypev1beta1 "kubevirt.io/api/instancetype/v1beta1"
	fakeclientset "kubevirt.io/client-go/kubevirt/fake"
	instancetypeclientv1beta1 "kubevirt.io/client-go/kubevirt/typed/instancetype/v1beta1"
)

const (
	nonExistingResourceName           = "non-existing-resource"
	resourceUID             types.UID = "9160e5de-2540-476a-86d9-af0081aee68a"
	resourceGeneration      int64     = 1
)

type handler interface {
	Store(*virtv1.VirtualMachine) error
}

var _ = Describe("Instancetype and Preferences revision handler", func() {
	var (
		storeHandler                     handler
		vm                               *virtv1.VirtualMachine
		virtClient                       *kubecli.MockKubevirtClient
		vmInterface                      *kubecli.MockVirtualMachineInterface
		k8sClient                        *k8sfake.Clientset
		fakeInstancetypeClients          instancetypeclientv1beta1.InstancetypeV1beta1Interface
		instancetypeInformerStore        cache.Store
		clusterInstancetypeInformerStore cache.Store
		preferenceInformerStore          cache.Store
		clusterPreferenceInformerStore   cache.Store
	)

	expectControllerRevisionCreation := func(expectedControllerRevision *appsv1.ControllerRevision) {
		k8sClient.Fake.PrependReactor("create", "controllerrevisions", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
			created, ok := action.(testing.CreateAction)
			Expect(ok).To(BeTrue())

			createObj := created.GetObject().(*appsv1.ControllerRevision)

			// This is already covered by the below assertion but be explicit here to ensure coverage
			Expect(createObj.Labels).To(HaveKey(apiinstancetype.ControllerRevisionObjectGenerationLabel))
			Expect(createObj.Labels).To(HaveKey(apiinstancetype.ControllerRevisionObjectKindLabel))
			Expect(createObj.Labels).To(HaveKey(apiinstancetype.ControllerRevisionObjectNameLabel))
			Expect(createObj.Labels).To(HaveKey(apiinstancetype.ControllerRevisionObjectUIDLabel))
			Expect(createObj.Labels).To(HaveKey(apiinstancetype.ControllerRevisionObjectVersionLabel))
			Expect(createObj).To(Equal(expectedControllerRevision))

			return true, created.GetObject(), nil
		})
	}

	BeforeEach(func() {
		k8sClient = k8sfake.NewSimpleClientset()
		ctrl := gomock.NewController(GinkgoT())
		virtClient = kubecli.NewMockKubevirtClient(ctrl)
		vmInterface = kubecli.NewMockVirtualMachineInterface(ctrl)
		virtClient.EXPECT().VirtualMachine(metav1.NamespaceDefault).Return(vmInterface).AnyTimes()
		virtClient.EXPECT().AppsV1().Return(k8sClient.AppsV1()).AnyTimes()
		fakeInstancetypeClients = fakeclientset.NewSimpleClientset().InstancetypeV1beta1()

		instancetypeInformer, _ := testutils.NewFakeInformerFor(&instancetypev1beta1.VirtualMachineInstancetype{})
		instancetypeInformerStore = instancetypeInformer.GetStore()

		clusterInstancetypeInformer, _ := testutils.NewFakeInformerFor(&instancetypev1beta1.VirtualMachineClusterInstancetype{})
		clusterInstancetypeInformerStore = clusterInstancetypeInformer.GetStore()

		preferenceInformer, _ := testutils.NewFakeInformerFor(&instancetypev1beta1.VirtualMachinePreference{})
		preferenceInformerStore = preferenceInformer.GetStore()

		clusterPreferenceInformer, _ := testutils.NewFakeInformerFor(&instancetypev1beta1.VirtualMachineClusterPreference{})
		clusterPreferenceInformerStore = clusterPreferenceInformer.GetStore()

		storeHandler = revision.New(
			instancetypeInformerStore,
			clusterInstancetypeInformerStore,
			preferenceInformerStore,
			clusterInstancetypeInformerStore,
			virtClient)

		vm = kubecli.NewMinimalVM("testvm")
		vm.Spec.Template = &virtv1.VirtualMachineInstanceTemplateSpec{
			Spec: virtv1.VirtualMachineInstanceSpec{
				Domain: virtv1.DomainSpec{},
			},
		}
		vm.Namespace = k8sv1.NamespaceDefault
	})

	Context("store preference", func() {
		It("store returns error when preferenceMatcher kind is invalid", func() {
			vm.Spec.Preference = &virtv1.PreferenceMatcher{
				Kind: "foobar",
			}
			Expect(storeHandler.Store(vm)).To(MatchError(ContainSubstring("got unexpected kind in PreferenceMatcher")))
		})

		It("store returns nil when no preference is specified", func() {
			vm.Spec.Preference = nil
			Expect(storeHandler.Store(vm)).To(Succeed())
		})
	})

	Context("store instancetype", func() {
		It("store returns error when instancetypeMatcher kind is invalid", func() {
			vm.Spec.Instancetype = &virtv1.InstancetypeMatcher{
				Kind: "foobar",
			}
			Expect(storeHandler.Store(vm)).To(MatchError(ContainSubstring("got unexpected kind in InstancetypeMatcher")))
		})

		It("store returns nil when no instancetypeMatcher is specified", func() {
			vm.Spec.Instancetype = nil
			Expect(storeHandler.Store(vm)).To(Succeed())
		})

		Context("using global ClusterInstancetype", func() {
			var clusterInstancetype *instancetypev1beta1.VirtualMachineClusterInstancetype
			var fakeClusterInstancetypeClient instancetypeclientv1beta1.VirtualMachineClusterInstancetypeInterface

			BeforeEach(func() {
				fakeClusterInstancetypeClient = fakeInstancetypeClients.VirtualMachineClusterInstancetypes()
				virtClient.EXPECT().VirtualMachineClusterInstancetype().Return(fakeClusterInstancetypeClient).AnyTimes()

				clusterInstancetype = &instancetypev1beta1.VirtualMachineClusterInstancetype{
					TypeMeta: metav1.TypeMeta{
						Kind:       "VirtualMachineClusterInstancetype",
						APIVersion: instancetypev1beta1.SchemeGroupVersion.String(),
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:       "test-cluster-instancetype",
						UID:        resourceUID,
						Generation: resourceGeneration,
					},
					Spec: instancetypev1beta1.VirtualMachineInstancetypeSpec{
						CPU: instancetypev1beta1.CPUInstancetype{
							Guest: uint32(2),
						},
						Memory: instancetypev1beta1.MemoryInstancetype{
							Guest: resource.MustParse("128Mi"),
						},
					},
				}

				_, err := virtClient.VirtualMachineClusterInstancetype().Create(context.Background(), clusterInstancetype, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())

				err = clusterInstancetypeInformerStore.Add(clusterInstancetype)
				Expect(err).ToNot(HaveOccurred())

				vm.Spec.Instancetype = &virtv1.InstancetypeMatcher{
					Name: clusterInstancetype.Name,
					Kind: apiinstancetype.ClusterSingularResourceName,
				}
			})

			It("store VirtualMachineClusterInstancetype ControllerRevision", func() {
				clusterInstancetypeControllerRevision, err := instancetype.CreateControllerRevision(vm, clusterInstancetype)
				Expect(err).ToNot(HaveOccurred())

				expectedRevisionNamePatch, err := revision.GeneratePatch(clusterInstancetypeControllerRevision, nil)
				Expect(err).ToNot(HaveOccurred())

				vmInterface.EXPECT().Patch(context.Background(), vm.Name, types.JSONPatchType, expectedRevisionNamePatch, metav1.PatchOptions{})

				expectControllerRevisionCreation(clusterInstancetypeControllerRevision)

				Expect(storeHandler.Store(vm)).To(Succeed())
				Expect(vm.Spec.Instancetype.RevisionName).To(Equal(clusterInstancetypeControllerRevision.Name))
			})

			It("store returns a nil revision when RevisionName already populated", func() {
				clusterInstancetypeControllerRevision, err := instancetype.CreateControllerRevision(vm, clusterInstancetype)
				Expect(err).ToNot(HaveOccurred())

				_, err = virtClient.AppsV1().ControllerRevisions(vm.Namespace).Create(context.Background(), clusterInstancetypeControllerRevision, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())

				vm.Spec.Instancetype = &virtv1.InstancetypeMatcher{
					Name:         clusterInstancetype.Name,
					RevisionName: clusterInstancetypeControllerRevision.Name,
					Kind:         apiinstancetype.ClusterSingularResourceName,
				}

				Expect(storeHandler.Store(vm)).To(Succeed())
				Expect(vm.Spec.Instancetype.RevisionName).To(Equal(clusterInstancetypeControllerRevision.Name))
			})

			It("store fails when instancetype does not exist", func() {
				vm.Spec.Instancetype.Name = nonExistingResourceName
				err := storeHandler.Store(vm)
				Expect(err).To(HaveOccurred())
				Expect(errors.IsNotFound(err)).To(BeTrue())
			})

			It("store ControllerRevision succeeds if a revision exists with expected data", func() {
				instancetypeControllerRevision, err := instancetype.CreateControllerRevision(vm, clusterInstancetype)
				Expect(err).ToNot(HaveOccurred())

				_, err = virtClient.AppsV1().ControllerRevisions(vm.Namespace).Create(context.Background(), instancetypeControllerRevision, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())

				expectedRevisionNamePatch, err := revision.GeneratePatch(instancetypeControllerRevision, nil)
				Expect(err).ToNot(HaveOccurred())

				vmInterface.EXPECT().Patch(context.Background(), vm.Name, types.JSONPatchType, expectedRevisionNamePatch, metav1.PatchOptions{})

				Expect(storeHandler.Store(vm)).To(Succeed())
				Expect(vm.Spec.Instancetype.RevisionName).To(Equal(instancetypeControllerRevision.Name))
			})

			It("store ControllerRevision fails if a revision exists with unexpected data", func() {
				unexpectedInstancetype := clusterInstancetype.DeepCopy()
				unexpectedInstancetype.Spec.CPU.Guest = 15

				instancetypeControllerRevision, err := instancetype.CreateControllerRevision(vm, unexpectedInstancetype)
				Expect(err).ToNot(HaveOccurred())

				_, err = virtClient.AppsV1().ControllerRevisions(vm.Namespace).Create(context.Background(), instancetypeControllerRevision, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())

				Expect(storeHandler.Store(vm)).To(MatchError(ContainSubstring("found existing ControllerRevision with unexpected data")))
			})

			It("store ControllerRevision fails if instancetype conflicts with vm", func() {
				vm.Spec.Template.Spec.Domain.CPU = &virtv1.CPU{
					Cores: 1,
				}
				Expect(storeHandler.Store(vm)).To(MatchError(conflict.Conflicts{conflict.New("spec", "template", "spec", "domain", "cpu", "cores")}))
			})
		})
	})

	Context("using namespaced Instancetype", func() {
		var fakeInstancetype *instancetypev1beta1.VirtualMachineInstancetype
		var fakeInstancetypeClient instancetypeclientv1beta1.VirtualMachineInstancetypeInterface

		BeforeEach(func() {
			fakeInstancetypeClient = fakeInstancetypeClients.VirtualMachineInstancetypes(vm.Namespace)
			virtClient.EXPECT().VirtualMachineInstancetype(gomock.Any()).Return(fakeInstancetypeClient).AnyTimes()

			fakeInstancetype = &instancetypev1beta1.VirtualMachineInstancetype{
				TypeMeta: metav1.TypeMeta{
					Kind:       "VirtualMachineInstancetype",
					APIVersion: instancetypev1beta1.SchemeGroupVersion.String(),
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-instancetype",
					Namespace:  vm.Namespace,
					UID:        resourceUID,
					Generation: resourceGeneration,
				},
				Spec: instancetypev1beta1.VirtualMachineInstancetypeSpec{
					CPU: instancetypev1beta1.CPUInstancetype{
						Guest: uint32(2),
					},
					Memory: instancetypev1beta1.MemoryInstancetype{
						Guest: resource.MustParse("128Mi"),
					},
				},
			}

			_, err := virtClient.VirtualMachineInstancetype(vm.Namespace).Create(context.Background(), fakeInstancetype, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			err = instancetypeInformerStore.Add(fakeInstancetype)
			Expect(err).ToNot(HaveOccurred())

			vm.Spec.Instancetype = &virtv1.InstancetypeMatcher{
				Name: fakeInstancetype.Name,
				Kind: apiinstancetype.SingularResourceName,
			}
		})

		It("store VirtualMachineInstancetype ControllerRevision", func() {
			instancetypeControllerRevision, err := instancetype.CreateControllerRevision(vm, fakeInstancetype)
			Expect(err).ToNot(HaveOccurred())

			expectedRevisionNamePatch, err := revision.GeneratePatch(instancetypeControllerRevision, nil)
			Expect(err).ToNot(HaveOccurred())

			vmInterface.EXPECT().Patch(context.Background(), vm.Name, types.JSONPatchType, expectedRevisionNamePatch, metav1.PatchOptions{})

			expectControllerRevisionCreation(instancetypeControllerRevision)

			Expect(storeHandler.Store(vm)).To(Succeed())
			Expect(vm.Spec.Instancetype.RevisionName).To(Equal(instancetypeControllerRevision.Name))
		})

		It("store fails when instancetype does not exist", func() {
			vm.Spec.Instancetype.Name = nonExistingResourceName
			err := storeHandler.Store(vm)
			Expect(err).To(HaveOccurred())
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("store returns a nil revision when RevisionName already populated", func() {
			instancetypeControllerRevision, err := instancetype.CreateControllerRevision(vm, fakeInstancetype)
			Expect(err).ToNot(HaveOccurred())

			_, err = virtClient.AppsV1().ControllerRevisions(vm.Namespace).Create(context.Background(), instancetypeControllerRevision, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			vm.Spec.Instancetype = &virtv1.InstancetypeMatcher{
				Name:         fakeInstancetype.Name,
				RevisionName: instancetypeControllerRevision.Name,
				Kind:         apiinstancetype.SingularResourceName,
			}

			Expect(storeHandler.Store(vm)).To(Succeed())
			Expect(vm.Spec.Instancetype.RevisionName).To(Equal(instancetypeControllerRevision.Name))
		})

		It("store ControllerRevision succeeds if a revision exists with expected data", func() {
			instancetypeControllerRevision, err := instancetype.CreateControllerRevision(vm, fakeInstancetype)
			Expect(err).ToNot(HaveOccurred())

			_, err = virtClient.AppsV1().ControllerRevisions(vm.Namespace).Create(context.Background(), instancetypeControllerRevision, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			expectedRevisionNamePatch, err := revision.GeneratePatch(instancetypeControllerRevision, nil)
			Expect(err).ToNot(HaveOccurred())

			vmInterface.EXPECT().Patch(context.Background(), vm.Name, types.JSONPatchType, expectedRevisionNamePatch, metav1.PatchOptions{})

			Expect(storeHandler.Store(vm)).To(Succeed())
			Expect(vm.Spec.Instancetype.RevisionName).To(Equal(instancetypeControllerRevision.Name))
		})

		It("store ControllerRevision fails if a revision exists with unexpected data", func() {
			unexpectedInstancetype := fakeInstancetype.DeepCopy()
			unexpectedInstancetype.Spec.CPU.Guest = 15

			instancetypeControllerRevision, err := instancetype.CreateControllerRevision(vm, unexpectedInstancetype)
			Expect(err).ToNot(HaveOccurred())

			_, err = virtClient.AppsV1().ControllerRevisions(vm.Namespace).Create(context.Background(), instancetypeControllerRevision, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			Expect(storeHandler.Store(vm)).To(MatchError(ContainSubstring("found existing ControllerRevision with unexpected data")))
		})

		It("store ControllerRevision fails if instancetype conflicts with vm", func() {
			vm.Spec.Template.Spec.Domain.CPU = &virtv1.CPU{
				Cores: 1,
			}
			Expect(storeHandler.Store(vm)).To(MatchError(conflict.Conflicts{conflict.New("spec", "template", "spec", "domain", "cpu", "cores")}))
		})
	})
	Context("using global ClusterPreference", func() {
		var clusterPreference *instancetypev1beta1.VirtualMachineClusterPreference
		var fakeClusterPreferenceClient instancetypeclientv1beta1.VirtualMachineClusterPreferenceInterface

		BeforeEach(func() {
			fakeClusterPreferenceClient = fakeInstancetypeClients.VirtualMachineClusterPreferences()
			virtClient.EXPECT().VirtualMachineClusterPreference().Return(fakeClusterPreferenceClient).AnyTimes()

			preferredCPUTopology := instancetypev1beta1.Cores
			clusterPreference = &instancetypev1beta1.VirtualMachineClusterPreference{
				TypeMeta: metav1.TypeMeta{
					Kind:       "VirtualMachineClusterPreference",
					APIVersion: instancetypev1beta1.SchemeGroupVersion.String(),
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-cluster-preference",
					UID:        resourceUID,
					Generation: resourceGeneration,
				},
				Spec: instancetypev1beta1.VirtualMachinePreferenceSpec{
					CPU: &instancetypev1beta1.CPUPreferences{
						PreferredCPUTopology: &preferredCPUTopology,
					},
				},
			}

			_, err := virtClient.VirtualMachineClusterPreference().Create(context.Background(), clusterPreference, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			err = clusterPreferenceInformerStore.Add(clusterPreference)
			Expect(err).ToNot(HaveOccurred())

			vm.Spec.Preference = &virtv1.PreferenceMatcher{
				Name: clusterPreference.Name,
				Kind: apiinstancetype.ClusterSingularPreferenceResourceName,
			}
		})

		It("store VirtualMachineClusterPreference ControllerRevision", func() {
			clusterPreferenceControllerRevision, err := instancetype.CreateControllerRevision(vm, clusterPreference)
			Expect(err).ToNot(HaveOccurred())

			expectedRevisionNamePatch, err := revision.GeneratePatch(nil, clusterPreferenceControllerRevision)
			Expect(err).ToNot(HaveOccurred())

			vmInterface.EXPECT().Patch(context.Background(), vm.Name, types.JSONPatchType, expectedRevisionNamePatch, metav1.PatchOptions{})

			expectControllerRevisionCreation(clusterPreferenceControllerRevision)

			Expect(storeHandler.Store(vm)).To(Succeed())
			Expect(vm.Spec.Preference.RevisionName).To(Equal(clusterPreferenceControllerRevision.Name))
		})

		It("store fails when VirtualMachineClusterPreference doesn't exist", func() {
			vm.Spec.Preference.Name = nonExistingResourceName
			err := storeHandler.Store(vm)
			Expect(err).To(HaveOccurred())
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("store returns nil revision when RevisionName already populated", func() {
			clusterPreferenceControllerRevision, err := instancetype.CreateControllerRevision(vm, clusterPreference)
			Expect(err).ToNot(HaveOccurred())

			_, err = virtClient.AppsV1().ControllerRevisions(vm.Namespace).Create(context.Background(), clusterPreferenceControllerRevision, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			vm.Spec.Preference.RevisionName = clusterPreferenceControllerRevision.Name

			Expect(storeHandler.Store(vm)).To(Succeed())
			Expect(vm.Spec.Preference.RevisionName).To(Equal(clusterPreferenceControllerRevision.Name))
		})

		It("store ControllerRevision succeeds if a revision exists with expected data", func() {
			clusterPreferenceControllerRevision, err := instancetype.CreateControllerRevision(vm, clusterPreference)
			Expect(err).ToNot(HaveOccurred())

			_, err = virtClient.AppsV1().ControllerRevisions(vm.Namespace).Create(context.Background(), clusterPreferenceControllerRevision, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			expectedRevisionNamePatch, err := revision.GeneratePatch(nil, clusterPreferenceControllerRevision)
			Expect(err).ToNot(HaveOccurred())

			vmInterface.EXPECT().Patch(context.Background(), vm.Name, types.JSONPatchType, expectedRevisionNamePatch, metav1.PatchOptions{})

			Expect(storeHandler.Store(vm)).To(Succeed())
			Expect(vm.Spec.Preference.RevisionName).To(Equal(clusterPreferenceControllerRevision.Name))
		})

		It("store ControllerRevision fails if a revision exists with unexpected data", func() {
			unexpectedPreference := clusterPreference.DeepCopy()
			preferredCPUTopology := instancetypev1beta1.Threads
			unexpectedPreference.Spec.CPU.PreferredCPUTopology = &preferredCPUTopology

			clusterPreferenceControllerRevision, err := instancetype.CreateControllerRevision(vm, unexpectedPreference)
			Expect(err).ToNot(HaveOccurred())

			_, err = virtClient.AppsV1().ControllerRevisions(vm.Namespace).Create(context.Background(), clusterPreferenceControllerRevision, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			Expect(storeHandler.Store(vm)).To(MatchError(ContainSubstring("found existing ControllerRevision with unexpected data")))
		})
	})
	Context("using namespaced Preference", func() {
		var preference *instancetypev1beta1.VirtualMachinePreference
		var fakePreferenceClient instancetypeclientv1beta1.VirtualMachinePreferenceInterface

		BeforeEach(func() {
			fakePreferenceClient = fakeInstancetypeClients.VirtualMachinePreferences(vm.Namespace)
			virtClient.EXPECT().VirtualMachinePreference(gomock.Any()).Return(fakePreferenceClient).AnyTimes()

			preferredCPUTopology := instancetypev1beta1.Cores
			preference = &instancetypev1beta1.VirtualMachinePreference{
				TypeMeta: metav1.TypeMeta{
					Kind:       "VirtualMachinePreference",
					APIVersion: instancetypev1beta1.SchemeGroupVersion.String(),
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-preference",
					Namespace:  vm.Namespace,
					UID:        resourceUID,
					Generation: resourceGeneration,
				},
				Spec: instancetypev1beta1.VirtualMachinePreferenceSpec{
					CPU: &instancetypev1beta1.CPUPreferences{
						PreferredCPUTopology: &preferredCPUTopology,
					},
				},
			}

			_, err := virtClient.VirtualMachinePreference(vm.Namespace).Create(context.Background(), preference, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			err = preferenceInformerStore.Add(preference)
			Expect(err).ToNot(HaveOccurred())

			vm.Spec.Preference = &virtv1.PreferenceMatcher{
				Name: preference.Name,
				Kind: apiinstancetype.SingularPreferenceResourceName,
			}
		})

		It("store VirtualMachinePreference ControllerRevision", func() {
			preferenceControllerRevision, err := instancetype.CreateControllerRevision(vm, preference)
			Expect(err).ToNot(HaveOccurred())

			expectedRevisionNamePatch, err := revision.GeneratePatch(nil, preferenceControllerRevision)
			Expect(err).ToNot(HaveOccurred())

			vmInterface.EXPECT().Patch(context.Background(), vm.Name, types.JSONPatchType, expectedRevisionNamePatch, metav1.PatchOptions{})

			expectControllerRevisionCreation(preferenceControllerRevision)

			Expect(storeHandler.Store(vm)).To(Succeed())
			Expect(vm.Spec.Preference.RevisionName).To(Equal(preferenceControllerRevision.Name))
		})

		It("store fails when VirtualMachinePreference doesn't exist", func() {
			vm.Spec.Preference.Name = nonExistingResourceName
			err := storeHandler.Store(vm)
			Expect(err).To(HaveOccurred())
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("store returns nil revision when RevisionName already populated", func() {
			preferenceControllerRevision, err := instancetype.CreateControllerRevision(vm, preference)
			Expect(err).ToNot(HaveOccurred())

			_, err = virtClient.AppsV1().ControllerRevisions(vm.Namespace).Create(context.Background(), preferenceControllerRevision, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			vm.Spec.Preference.RevisionName = preferenceControllerRevision.Name

			Expect(storeHandler.Store(vm)).To(Succeed())
			Expect(vm.Spec.Preference.RevisionName).To(Equal(preferenceControllerRevision.Name))
		})

		It("store ControllerRevision succeeds if a revision exists with expected data", func() {
			preferenceControllerRevision, err := instancetype.CreateControllerRevision(vm, preference)
			Expect(err).ToNot(HaveOccurred())

			_, err = virtClient.AppsV1().ControllerRevisions(vm.Namespace).Create(context.Background(), preferenceControllerRevision, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			expectedRevisionNamePatch, err := revision.GeneratePatch(nil, preferenceControllerRevision)
			Expect(err).ToNot(HaveOccurred())

			vmInterface.EXPECT().Patch(context.Background(), vm.Name, types.JSONPatchType, expectedRevisionNamePatch, metav1.PatchOptions{})

			Expect(storeHandler.Store(vm)).To(Succeed())
			Expect(vm.Spec.Preference.RevisionName).To(Equal(preferenceControllerRevision.Name))
		})

		It("store ControllerRevision fails if a revision exists with unexpected data", func() {
			unexpectedPreference := preference.DeepCopy()
			preferredCPUTopology := instancetypev1beta1.Threads
			unexpectedPreference.Spec.CPU.PreferredCPUTopology = &preferredCPUTopology

			preferenceControllerRevision, err := instancetype.CreateControllerRevision(vm, unexpectedPreference)
			Expect(err).ToNot(HaveOccurred())

			_, err = virtClient.AppsV1().ControllerRevisions(vm.Namespace).Create(context.Background(), preferenceControllerRevision, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			Expect(storeHandler.Store(vm)).To(MatchError(ContainSubstring("found existing ControllerRevision with unexpected data")))
		})
	})
})
