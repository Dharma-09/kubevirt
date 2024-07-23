package workloadupdater

import (
	"context"
	"fmt"
	"time"

	"github.com/golang/mock/gomock"
	k8sv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	framework "k8s.io/client-go/tools/cache/testing"
	"k8s.io/client-go/tools/record"

	"kubevirt.io/client-go/api"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"

	virtcontroller "kubevirt.io/kubevirt/pkg/controller"
	metrics "kubevirt.io/kubevirt/pkg/monitoring/metrics/virt-controller"
	"kubevirt.io/kubevirt/pkg/testutils"
	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Workload Updater", func() {
	var ctrl *gomock.Controller
	var stop chan struct{}
	var virtClient *kubecli.MockKubevirtClient
	var migrationInterface *kubecli.MockVirtualMachineInstanceMigrationInterface
	var kubeVirtInterface *kubecli.MockKubeVirtInterface
	var vmiInterface *kubecli.MockVirtualMachineInstanceInterface
	var kubeVirtSource *framework.FakeControllerSource
	var kubeVirtInformer cache.SharedIndexInformer
	var recorder *record.FakeRecorder
	var mockQueue *testutils.MockWorkQueue
	var kubeClient *fake.Clientset

	var controller *WorkloadUpdateController

	var expectedImage string

	syncCaches := func(stop chan struct{}) {
		go kubeVirtInformer.Run(stop)

		Expect(cache.WaitForCacheSync(stop,
			kubeVirtInformer.HasSynced,
		)).To(BeTrue())
	}

	addKubeVirt := func(kv *v1.KubeVirt) {
		mockQueue.ExpectAdds(1)
		kubeVirtSource.Add(kv)
		mockQueue.Wait()
	}

	shouldExpectMultiplePodEvictions := func(evictionCount *int) {
		// Expect pod deletion
		kubeClient.Fake.PrependReactor("create", "pods", func(action testing.Action) (handled bool, ret runtime.Object, err error) {
			if action.GetSubresource() == "eviction" {
				*evictionCount++
				return true, nil, nil
			}
			return false, nil, nil
		})
	}

	BeforeEach(func() {

		expectedImage = "cur-image"

		err := metrics.RegisterLeaderMetrics()
		Expect(err).ToNot(HaveOccurred())
		metrics.SetOutdatedVirtualMachineInstanceWorkloads(0)

		stop = make(chan struct{})
		ctrl = gomock.NewController(GinkgoT())
		virtClient = kubecli.NewMockKubevirtClient(ctrl)
		migrationInterface = kubecli.NewMockVirtualMachineInstanceMigrationInterface(ctrl)
		kubeVirtInterface = kubecli.NewMockKubeVirtInterface(ctrl)
		vmiInterface = kubecli.NewMockVirtualMachineInstanceInterface(ctrl)

		vmiInformer, _ := testutils.NewFakeInformerWithIndexersFor(&v1.VirtualMachineInstance{}, cache.Indexers{
			cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
			"node": func(obj interface{}) (strings []string, e error) {
				return []string{obj.(*v1.VirtualMachineInstance).Status.NodeName}, nil
			},
		})
		migrationInformer, _ := testutils.NewFakeInformerFor(&v1.VirtualMachineInstanceMigration{})
		podInformer, _ := testutils.NewFakeInformerFor(&k8sv1.Pod{})
		recorder = record.NewFakeRecorder(200)
		recorder.IncludeObject = true
		config, _, _ := testutils.NewFakeClusterConfigUsingKVConfig(&v1.KubeVirtConfiguration{})

		kubeVirtInformer, _ = testutils.NewFakeInformerFor(&v1.KubeVirt{})
		kubeVirtInformer, kubeVirtSource = testutils.NewFakeInformerFor(&v1.KubeVirt{})

		controller, _ = NewWorkloadUpdateController(expectedImage, vmiInformer, podInformer, migrationInformer, kubeVirtInformer, recorder, virtClient, config)
		mockQueue = testutils.NewMockWorkQueue(controller.queue)
		controller.queue = mockQueue

		// Set up mock client
		virtClient.EXPECT().VirtualMachineInstanceMigration(k8sv1.NamespaceDefault).Return(migrationInterface).AnyTimes()
		virtClient.EXPECT().VirtualMachineInstance(k8sv1.NamespaceDefault).Return(vmiInterface).AnyTimes()
		virtClient.EXPECT().KubeVirt(k8sv1.NamespaceDefault).Return(kubeVirtInterface).AnyTimes()
		kubeClient = fake.NewSimpleClientset()
		virtClient.EXPECT().CoreV1().Return(kubeClient.CoreV1()).AnyTimes()
		virtClient.EXPECT().PolicyV1().Return(kubeClient.PolicyV1()).AnyTimes()

		// Make sure that all unexpected calls to kubeClient will fail
		kubeClient.Fake.PrependReactor("*", "*", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
			Expect(action).To(BeNil())
			return true, nil, nil
		})
		syncCaches(stop)
	})

	Context("workload update in progress", func() {
		It("should migrate the VMI", func() {
			newVirtualMachine("testvm", true, "madeup", controller.vmiStore, controller.podIndexer)
			waitForNumberOfInstancesOnVMIInformerCache(controller, 1)
			kv := newKubeVirt(1)
			kv.Spec.WorkloadUpdateStrategy.WorkloadUpdateMethods = []v1.WorkloadUpdateMethod{v1.WorkloadUpdateMethodLiveMigrate, v1.WorkloadUpdateMethodEvict}
			addKubeVirt(kv)

			migrationInterface.EXPECT().Create(context.Background(), gomock.Any(), metav1.CreateOptions{}).Return(&v1.VirtualMachineInstanceMigration{ObjectMeta: metav1.ObjectMeta{Name: "something"}}, nil)

			controller.Execute()
			testutils.ExpectEvent(recorder, SuccessfulCreateVirtualMachineInstanceMigrationReason)
		})

		It("should do nothing if deployment is updating", func() {
			newVirtualMachine("testvm", true, "madeup", controller.vmiStore, controller.podIndexer)
			waitForNumberOfInstancesOnVMIInformerCache(controller, 1)
			kv := newKubeVirt(1)
			kv.Spec.WorkloadUpdateStrategy.WorkloadUpdateMethods = []v1.WorkloadUpdateMethod{v1.WorkloadUpdateMethodLiveMigrate, v1.WorkloadUpdateMethodEvict}
			addKubeVirt(kv)

			kv.Status.ObservedDeploymentID = "something new"
			controller.Execute()
			Expect(recorder.Events).To(BeEmpty())
		})

		It("should update out of date value on kv and report prometheus metric", func() {

			By("Checking prometheus metric before sync")
			value, err := metrics.GetOutdatedVirtualMachineInstanceWorkloads()
			Expect(err).ToNot(HaveOccurred())
			Expect(value).To(BeZero(), "outdated vmi workload reported should be equal to zero")

			totalVMs := 0
			reasons := []string{}
			for i := 0; i < 50; i++ {
				newVirtualMachine(fmt.Sprintf("testvm-migratable-%d", i), true, "madeup", controller.vmiStore, controller.podIndexer)
				totalVMs++
			}
			for i := 0; i < 50; i++ {
				newVirtualMachine(fmt.Sprintf("testvm-non-migratable-%d", i), false, "madeup", controller.vmiStore, controller.podIndexer)
				totalVMs++
			}
			// add vmis that are not outdated to ensure they are not counted as outdated in count
			for i := 0; i < 100; i++ {
				newVirtualMachine(fmt.Sprintf("testvm-up-to-date-%d", i), false, expectedImage, controller.vmiStore, controller.podIndexer)
				totalVMs++
			}
			for i := 0; i < int(virtconfig.ParallelMigrationsPerClusterDefault); i++ {
				reasons = append(reasons, SuccessfulCreateVirtualMachineInstanceMigrationReason)
			}
			for i := 0; i < defaultBatchDeletionCount; i++ {
				reasons = append(reasons, SuccessfulEvictVirtualMachineInstanceReason)
			}

			waitForNumberOfInstancesOnVMIInformerCache(controller, totalVMs)
			kv := newKubeVirt(0)
			kv.Spec.WorkloadUpdateStrategy.WorkloadUpdateMethods = []v1.WorkloadUpdateMethod{v1.WorkloadUpdateMethodLiveMigrate, v1.WorkloadUpdateMethodEvict}
			addKubeVirt(kv)

			kubeVirtInterface.EXPECT().PatchStatus(context.Background(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Do(func(ctx context.Context, name string, pt types.PatchType, data []byte, patchOptions metav1.PatchOptions) {
				str := string(data)
				Expect(str).To(Equal("[{ \"op\": \"test\", \"path\": \"/status/outdatedVirtualMachineInstanceWorkloads\", \"value\": 0}, { \"op\": \"replace\", \"path\": \"/status/outdatedVirtualMachineInstanceWorkloads\", \"value\": 100}]"))

			}).Return(nil, nil).Times(1)

			migrationInterface.EXPECT().Create(context.Background(), gomock.Any(), metav1.CreateOptions{}).Return(&v1.VirtualMachineInstanceMigration{ObjectMeta: metav1.ObjectMeta{Name: "something"}}, nil).Times(int(virtconfig.ParallelMigrationsPerClusterDefault))

			evictionCount := 0
			shouldExpectMultiplePodEvictions(&evictionCount)

			controller.Execute()
			testutils.ExpectEvents(recorder, reasons...)

			By("Checking prometheus metric")
			value, err = metrics.GetOutdatedVirtualMachineInstanceWorkloads()
			Expect(err).ToNot(HaveOccurred())

			Expect(value).To(Equal(100))
			Expect(evictionCount).To(Equal(defaultBatchDeletionCount))

		})

		It("should migrate VMIs up to the global max migration count and delete up to delete batch count", func() {
			totalVMs := 0
			reasons := []string{}
			for i := 0; i < 50; i++ {
				newVirtualMachine(fmt.Sprintf("testvm-migratable-%d", i), true, "madeup", controller.vmiStore, controller.podIndexer)
				totalVMs++
			}
			for i := 0; i < 50; i++ {
				newVirtualMachine(fmt.Sprintf("testvm-%d", i), false, "madeup", controller.vmiStore, controller.podIndexer)
				totalVMs++
			}
			for i := 0; i < int(virtconfig.ParallelMigrationsPerClusterDefault); i++ {
				reasons = append(reasons, SuccessfulCreateVirtualMachineInstanceMigrationReason)
			}
			for i := 0; i < defaultBatchDeletionCount; i++ {
				reasons = append(reasons, SuccessfulEvictVirtualMachineInstanceReason)
			}

			waitForNumberOfInstancesOnVMIInformerCache(controller, totalVMs)
			kv := newKubeVirt(totalVMs)
			kv.Spec.WorkloadUpdateStrategy.WorkloadUpdateMethods = []v1.WorkloadUpdateMethod{v1.WorkloadUpdateMethodLiveMigrate, v1.WorkloadUpdateMethodEvict}
			addKubeVirt(kv)

			migrationInterface.EXPECT().Create(context.Background(), gomock.Any(), metav1.CreateOptions{}).Return(&v1.VirtualMachineInstanceMigration{ObjectMeta: metav1.ObjectMeta{Name: "something"}}, nil).Times(int(virtconfig.ParallelMigrationsPerClusterDefault))
			evictionCount := 0
			shouldExpectMultiplePodEvictions(&evictionCount)

			controller.Execute()
			testutils.ExpectEvents(recorder, reasons...)
			Expect(evictionCount).To(Equal(defaultBatchDeletionCount))
		})

		It("should detect in-flight migrations when only migrate VMIs up to the global max migration count", func() {
			const desiredNumberOfVMs = 50
			const vmsPendingMigration = int(virtconfig.ParallelMigrationsPerClusterDefault)
			kv := newKubeVirt(desiredNumberOfVMs)
			kv.Spec.WorkloadUpdateStrategy.WorkloadUpdateMethods = []v1.WorkloadUpdateMethod{v1.WorkloadUpdateMethodLiveMigrate, v1.WorkloadUpdateMethodEvict}
			addKubeVirt(kv)

			By("populating with pending migrations that should be ignored while counting the threshold")
			for i := 0; i < vmsPendingMigration; i++ {
				controller.migrationStore.Add(newMigration(fmt.Sprintf("vmim-pending-%d", i), fmt.Sprintf("testvm-migratable-pending-%d", i), v1.MigrationPending))
			}

			reasons := []string{}
			for i := 0; i < desiredNumberOfVMs; i++ {
				vmi := newVirtualMachine(fmt.Sprintf("testvm-migratable-%d", i), true, "madeup", controller.vmiStore, controller.podIndexer)
				// create enough migrations to only allow one more active one to be created
				if i < int(virtconfig.ParallelMigrationsPerClusterDefault)-1 {
					controller.migrationStore.Add(newMigration(fmt.Sprintf("vmim-%d", i), vmi.Name, v1.MigrationRunning))
				} else if i < int(virtconfig.ParallelMigrationsPerClusterDefault) {
					controller.migrationStore.Add(newMigration(fmt.Sprintf("vmim-%d", i), vmi.Name, v1.MigrationSucceeded))
					// expect only a single migration to occur due to global limit
					reasons = append(reasons, SuccessfulCreateVirtualMachineInstanceMigrationReason)
				} else {
					controller.migrationStore.Add(newMigration(fmt.Sprintf("vmim-%d", i), vmi.Name, v1.MigrationSucceeded))
				}
			}

			waitForNumberOfInstancesOnVMIInformerCache(controller, desiredNumberOfVMs)

			migrationInterface.EXPECT().Create(context.Background(), gomock.Any(), metav1.CreateOptions{}).Return(&v1.VirtualMachineInstanceMigration{ObjectMeta: metav1.ObjectMeta{Name: "something"}}, nil).Times(1)

			controller.Execute()
			testutils.ExpectEvents(recorder, reasons...)
		})

		It("should migrate/shutdown outdated VMIs and leave up to date VMIs alone", func() {
			reasons := []string{}
			newVirtualMachine("testvm-outdated-migratable", true, "madeup", controller.vmiStore, controller.podIndexer)
			reasons = append(reasons, SuccessfulCreateVirtualMachineInstanceMigrationReason)

			newVirtualMachine("testvm-outdated-non-migratable", false, "madeup", controller.vmiStore, controller.podIndexer)
			reasons = append(reasons, SuccessfulEvictVirtualMachineInstanceReason)

			totalVMs := 2
			for i := 0; i < 50; i++ {
				newVirtualMachine(fmt.Sprintf("testvm-up-to-date-migratable-%d", i), true, expectedImage, controller.vmiStore, controller.podIndexer)
				newVirtualMachine(fmt.Sprintf("testvm-up-to-date-non-migratable-%d", i), false, expectedImage, controller.vmiStore, controller.podIndexer)
				totalVMs += 2
			}

			waitForNumberOfInstancesOnVMIInformerCache(controller, totalVMs)
			kv := newKubeVirt(2)
			kv.Spec.WorkloadUpdateStrategy.WorkloadUpdateMethods = []v1.WorkloadUpdateMethod{v1.WorkloadUpdateMethodLiveMigrate, v1.WorkloadUpdateMethodEvict}
			addKubeVirt(kv)

			migrationInterface.EXPECT().Create(context.Background(), gomock.Any(), metav1.CreateOptions{}).Return(&v1.VirtualMachineInstanceMigration{ObjectMeta: metav1.ObjectMeta{Name: "something"}}, nil).Times(1)
			evictionCount := 0
			shouldExpectMultiplePodEvictions(&evictionCount)

			controller.Execute()
			testutils.ExpectEvents(recorder, reasons...)
			Expect(evictionCount).To(Equal(1))
		})

		It("should do nothing if no method is set", func() {
			totalVMs := 0
			for i := 0; i < 50; i++ {
				newVirtualMachine(fmt.Sprintf("testvm-migratable-%d", i), true, "madeup", controller.vmiStore, controller.podIndexer)
				totalVMs++
			}
			for i := 0; i < 50; i++ {
				newVirtualMachine(fmt.Sprintf("testvm-%d", i), false, "madeup", controller.vmiStore, controller.podIndexer)
				totalVMs++
			}

			waitForNumberOfInstancesOnVMIInformerCache(controller, totalVMs)
			kv := newKubeVirt(totalVMs)
			addKubeVirt(kv)
			controller.Execute()
		})

		It("should shutdown VMIs and not migrate when only shutdown method is set", func() {
			const desiredNumberOfVMs = 50
			reasons := []string{}
			for i := 0; i < desiredNumberOfVMs; i++ {
				newVirtualMachine(fmt.Sprintf("testvm-migratable-%d", i), true, "madeup", controller.vmiStore, controller.podIndexer)
			}
			for i := 0; i < defaultBatchDeletionCount; i++ {
				reasons = append(reasons, SuccessfulEvictVirtualMachineInstanceReason)
			}

			waitForNumberOfInstancesOnVMIInformerCache(controller, desiredNumberOfVMs)
			kv := newKubeVirt(desiredNumberOfVMs)
			kv.Spec.WorkloadUpdateStrategy.WorkloadUpdateMethods = []v1.WorkloadUpdateMethod{v1.WorkloadUpdateMethodEvict}
			addKubeVirt(kv)

			evictionCount := 0
			shouldExpectMultiplePodEvictions(&evictionCount)

			controller.Execute()
			testutils.ExpectEvents(recorder, reasons...)
			Expect(evictionCount).To(Equal(defaultBatchDeletionCount))
		})

		It("should not evict VMIs when an active migration is in flight", func() {
			const desiredNumberOfVMs = 2
			kv := newKubeVirt(desiredNumberOfVMs)
			kv.Spec.WorkloadUpdateStrategy.WorkloadUpdateMethods = []v1.WorkloadUpdateMethod{v1.WorkloadUpdateMethodEvict}
			addKubeVirt(kv)

			vmi := newVirtualMachine("testvm-migratable", true, "madeup", controller.vmiStore, controller.podIndexer)
			controller.migrationStore.Add(newMigration("vmim-1", vmi.Name, v1.MigrationRunning))
			vmi = newVirtualMachine("testvm-nonmigratable", true, "madeup", controller.vmiStore, controller.podIndexer)
			controller.migrationStore.Add(newMigration("vmim-2", vmi.Name, v1.MigrationRunning))

			waitForNumberOfInstancesOnVMIInformerCache(controller, desiredNumberOfVMs)

			controller.Execute()
			Expect(recorder.Events).To(BeEmpty())
		})

		It("should respect custom batch deletion count", func() {
			const desiredNumberOfVMs = 50
			batchDeletions := 30
			reasons := []string{}
			for i := 0; i < desiredNumberOfVMs; i++ {
				newVirtualMachine(fmt.Sprintf("testvm-migratable-%d", i), true, "madeup", controller.vmiStore, controller.podIndexer)
			}
			for i := 0; i < batchDeletions; i++ {
				reasons = append(reasons, SuccessfulEvictVirtualMachineInstanceReason)
			}

			waitForNumberOfInstancesOnVMIInformerCache(controller, desiredNumberOfVMs)
			kv := newKubeVirt(desiredNumberOfVMs)
			kv.Spec.WorkloadUpdateStrategy.WorkloadUpdateMethods = []v1.WorkloadUpdateMethod{v1.WorkloadUpdateMethodEvict}
			kv.Spec.WorkloadUpdateStrategy.BatchEvictionSize = &batchDeletions
			addKubeVirt(kv)

			evictionCount := 0
			shouldExpectMultiplePodEvictions(&evictionCount)

			controller.Execute()
			testutils.ExpectEvents(recorder, reasons...)
			Expect(evictionCount).To(Equal(batchDeletions))
		})

		It("should respect custom batch interval", func() {
			batchDeletions := 5
			batchInterval := time.Duration(2) * time.Second
			reasons := []string{}
			for i := 0; i < batchDeletions; i++ {
				reasons = append(reasons, SuccessfulEvictVirtualMachineInstanceReason)
			}

			for i := 0; i < batchDeletions*2; i++ {
				newVirtualMachine(fmt.Sprintf("testvm-migratable-1-%d", i), true, "madeup", controller.vmiStore, controller.podIndexer)
			}
			waitForNumberOfInstancesOnVMIInformerCache(controller, batchDeletions*2)
			kv := newKubeVirt(batchDeletions * 2)
			kv.Spec.WorkloadUpdateStrategy.WorkloadUpdateMethods = []v1.WorkloadUpdateMethod{v1.WorkloadUpdateMethodEvict}
			kv.Spec.WorkloadUpdateStrategy.BatchEvictionSize = &batchDeletions
			kv.Spec.WorkloadUpdateStrategy.BatchEvictionInterval = &metav1.Duration{
				Duration: batchInterval,
			}

			evictionCount := 0
			shouldExpectMultiplePodEvictions(&evictionCount)

			addKubeVirt(kv)
			controller.Execute()
			testutils.ExpectEvents(recorder, reasons...)

			// Should do nothing this second execute due to interval
			addKubeVirt(kv)
			controller.Execute()
			Expect(recorder.Events).To(BeEmpty())

			// sleep to account for batch interval
			time.Sleep(3 * time.Second)

			// Should execute another batch of deletions after sleep
			addKubeVirt(kv)
			controller.Execute()
			testutils.ExpectEvents(recorder, reasons...)
			Expect(evictionCount).To(Equal(batchDeletions * 2))
		})

	})

	Context("LiveUpdate features", func() {
		It("VMI needs to be migrated when memory hotplug is requested", func() {
			vmi := api.NewMinimalVMI("testvm")

			condition := v1.VirtualMachineInstanceCondition{
				Type:   v1.VirtualMachineInstanceMemoryChange,
				Status: k8sv1.ConditionTrue,
			}
			virtcontroller.NewVirtualMachineInstanceConditionManager().UpdateCondition(vmi, &condition)

			Expect(controller.doesRequireMigration(vmi)).To(BeTrue())
		})
	})

	Context("Abort changes due to an automated live update", func() {
		createVM := func(annotation, hasChangeCondition bool) *v1.VirtualMachineInstance {
			vmi := api.NewMinimalVMI("testvm")
			vmi.Namespace = k8sv1.NamespaceDefault
			vmi.Status.Phase = v1.Running
			vmi.Status.Conditions = append(vmi.Status.Conditions, v1.VirtualMachineInstanceCondition{
				Type: v1.VirtualMachineInstanceIsMigratable, Status: k8sv1.ConditionTrue})
			if hasChangeCondition {
				vmi.Status.Conditions = append(vmi.Status.Conditions, v1.VirtualMachineInstanceCondition{
					Type:   v1.VirtualMachineInstanceMemoryChange,
					Status: k8sv1.ConditionTrue,
				})
			}
			if annotation {
				vmi.ObjectMeta.Annotations = make(map[string]string)
				vmi.ObjectMeta.Annotations[v1.WorkloadUpdateMigrationAbortionAnnotation] = ""
			}
			controller.vmiStore.Add(vmi)
			return vmi
		}
		createMig := func(vmiName string, phase v1.VirtualMachineInstanceMigrationPhase) *v1.VirtualMachineInstanceMigration {
			mig := newMigration("test", vmiName, phase)
			mig.Annotations = map[string]string{v1.WorkloadUpdateMigrationAnnotation: ""}
			controller.migrationStore.Add(mig)
			return mig
		}

		BeforeEach(func() {
			kv := newKubeVirt(0)
			kv.Spec.WorkloadUpdateStrategy.WorkloadUpdateMethods = []v1.WorkloadUpdateMethod{v1.WorkloadUpdateMethodLiveMigrate}
			addKubeVirt(kv)
		})

		DescribeTable("should delete the migration", func(phase v1.VirtualMachineInstanceMigrationPhase) {
			vmi := createVM(false, false)
			mig := createMig(vmi.Name, phase)

			if !mig.IsFinal() {
				migrationInterface.EXPECT().Delete(gomock.Any(), mig.Name, metav1.DeleteOptions{}).Return(nil)
			}

			controller.Execute()
			if mig.IsFinal() {
				Expect(recorder.Events).To(BeEmpty())
			} else {
				testutils.ExpectEvent(recorder, SuccessfulChangeAbortionReason)
			}
		},
			Entry("in running phase", v1.MigrationRunning),
			Entry("in failed phase", v1.MigrationFailed),
			Entry("in succeeded phase", v1.MigrationSucceeded),
		)

		DescribeTable("should handle", func(hasCond, hasMig bool) {
			vmi := createVM(false, hasCond)
			if hasCond {
				kubeVirtInterface.EXPECT().PatchStatus(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
			}
			var mig *v1.VirtualMachineInstanceMigration
			if hasMig {
				mig = createMig(vmi.Name, v1.MigrationRunning)
			}
			changeAborted := hasMig && !hasCond
			if changeAborted {
				migrationInterface.EXPECT().Delete(gomock.Any(), mig.Name, metav1.DeleteOptions{}).Return(nil)
			}
			controller.Execute()
			if changeAborted {
				testutils.ExpectEvent(recorder, SuccessfulChangeAbortionReason)
			} else {
				Expect(recorder.Events).To(BeEmpty())
			}
		},
			Entry("a in progress change update", true, true),
			Entry("a change abortion", false, true),
			Entry("no change in progress", false, false),
		)

		DescribeTable("should always cancel the migration when the testWorkloadUpdateMigrationAbortion annotation is present", func(hasCond bool) {
			vmi := createVM(true, hasCond)
			mig := createMig(vmi.Name, v1.MigrationRunning)
			migrationInterface.EXPECT().Delete(gomock.Any(), mig.Name, metav1.DeleteOptions{}).Return(nil)
			controller.Execute()
			testutils.ExpectEvent(recorder, SuccessfulChangeAbortionReason)
		},
			Entry("with the change condition", true),
			Entry("without the change condition", false),
		)

		It("should return an error if the migration hasn't been deleted", func() {
			vmi := createVM(true, false)
			mig := createMig(vmi.Name, v1.MigrationRunning)
			migrationInterface.EXPECT().Delete(gomock.Any(), mig.Name, metav1.DeleteOptions{}).Return(fmt.Errorf("some error"))

			controller.Execute()
			testutils.ExpectEvent(recorder, FailedChangeAbortionReason)
		})
	})

	AfterEach(func() {

		close(stop)

		Expect(recorder.Events).To(BeEmpty())
	})
})

func waitForNumberOfInstancesOnVMIInformerCache(wu *WorkloadUpdateController, vmisNo int) {
	EventuallyWithOffset(1, func() []interface{} {
		return wu.vmiStore.List()
	}, 3*time.Second, 200*time.Millisecond).Should(HaveLen(vmisNo))
}

func newKubeVirt(expectedNumOutdated int) *v1.KubeVirt {
	return &v1.KubeVirt{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
		},
		Spec: v1.KubeVirtSpec{},
		Status: v1.KubeVirtStatus{
			Phase:                                   v1.KubeVirtPhaseDeployed,
			OutdatedVirtualMachineInstanceWorkloads: &expectedNumOutdated,
		},
	}
}

func newVirtualMachine(name string, isMigratable bool, image string, vmiIndexer cache.Store, podStore cache.Store) *v1.VirtualMachineInstance {
	vmi := api.NewMinimalVMI("testvm")
	vmi.Name = name
	vmi.Namespace = k8sv1.NamespaceDefault
	vmi.Status.LauncherContainerImageVersion = image
	vmi.Status.Phase = v1.Running
	vmi.UID = "1234"
	if isMigratable {
		vmi.Status.Conditions = []v1.VirtualMachineInstanceCondition{{Type: v1.VirtualMachineInstanceIsMigratable, Status: k8sv1.ConditionTrue}}
	}

	pod := &k8sv1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vmi.Name,
			Namespace: vmi.Namespace,
			UID:       types.UID(vmi.Name),
			Labels: map[string]string{
				v1.AppLabel:       "virt-launcher",
				v1.CreatedByLabel: string(vmi.UID),
			},
			Annotations: map[string]string{
				v1.DomainAnnotation: vmi.Name,
			},
		},
		Status: k8sv1.PodStatus{
			Phase: k8sv1.PodRunning,
			ContainerStatuses: []k8sv1.ContainerStatus{
				{Ready: false, Name: "compute", State: k8sv1.ContainerState{Running: &k8sv1.ContainerStateRunning{}}},
			},
		},
	}
	vmi.Status.ActivePods = map[types.UID]string{
		pod.UID: "node1",
	}

	vmiIndexer.Add(vmi)
	podStore.Add(pod)
	return vmi
}

func newMigration(name string, vmi string, phase v1.VirtualMachineInstanceMigrationPhase) *v1.VirtualMachineInstanceMigration {
	migration := kubecli.NewMinimalMigration(name)
	migration.Status.Phase = phase
	migration.Spec.VMIName = vmi
	migration.Namespace = k8sv1.NamespaceDefault
	return migration
}
