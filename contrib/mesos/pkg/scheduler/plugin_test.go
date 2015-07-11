/*
Copyright 2015 The Kubernetes Authors All rights reserved.

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

package scheduler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api/testapi"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client/cache"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"
	kutil "github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/watch"

	assertext "github.com/GoogleCloudPlatform/kubernetes/contrib/mesos/pkg/assert"
	"github.com/GoogleCloudPlatform/kubernetes/contrib/mesos/pkg/executor/messages"
	"github.com/GoogleCloudPlatform/kubernetes/contrib/mesos/pkg/queue"
	schedcfg "github.com/GoogleCloudPlatform/kubernetes/contrib/mesos/pkg/scheduler/config"
	"github.com/GoogleCloudPlatform/kubernetes/contrib/mesos/pkg/scheduler/ha"
	"github.com/GoogleCloudPlatform/kubernetes/contrib/mesos/pkg/scheduler/podtask"
	log "github.com/golang/glog"
	mesos "github.com/mesos/mesos-go/mesosproto"
	util "github.com/mesos/mesos-go/mesosutil"
	bindings "github.com/mesos/mesos-go/scheduler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// A apiserver mock which partially mocks the pods API
type TestServer struct {
	server *httptest.Server
	stats  map[string]uint
	lock   sync.Mutex
}

func NewTestServer(t *testing.T, namespace string, mockPodListWatch *MockPodsListWatch) *TestServer {
	ts := TestServer{
		stats: map[string]uint{},
	}
	mux := http.NewServeMux()

	mux.HandleFunc(testapi.ResourcePath("pods", namespace, ""), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		pods := mockPodListWatch.Pods()
		w.Write([]byte(runtime.EncodeOrDie(testapi.Codec(), &pods)))
	})

	podsPrefix := testapi.ResourcePath("pods", namespace, "") + "/"
	mux.HandleFunc(podsPrefix, func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[len(podsPrefix):]

		// update statistics for this pod
		ts.lock.Lock()
		defer ts.lock.Unlock()
		ts.stats[name] = ts.stats[name] + 1

		p := mockPodListWatch.GetPod(name)
		if p != nil {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(runtime.EncodeOrDie(testapi.Codec(), p)))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	mux.HandleFunc(testapi.ResourcePath("events", namespace, ""), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/", func(res http.ResponseWriter, req *http.Request) {
		t.Errorf("unexpected request: %v", req.RequestURI)
		res.WriteHeader(http.StatusNotFound)
	})

	ts.server = httptest.NewServer(mux)
	return &ts
}
func (ts *TestServer) Stats(name string) uint {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	return ts.stats[name]
}

// Create mock of pods ListWatch, usually listening on the apiserver pods watch endpoint
type MockPodsListWatch struct {
	ListWatch   cache.ListWatch
	fakeWatcher *watch.FakeWatcher
	list        api.PodList
	lock        sync.Mutex
}

func NewMockPodsListWatch(initialPodList api.PodList) *MockPodsListWatch {
	lw := MockPodsListWatch{
		fakeWatcher: watch.NewFake(),
		list:        initialPodList,
	}
	lw.ListWatch = cache.ListWatch{
		WatchFunc: func(resourceVersion string) (watch.Interface, error) {
			return lw.fakeWatcher, nil
		},
		ListFunc: func() (runtime.Object, error) {
			lw.lock.Lock()
			defer lw.lock.Unlock()

			listCopy, err := api.Scheme.DeepCopy(&lw.list)
			return listCopy.(*api.PodList), err
		},
	}
	return &lw
}
func (lw *MockPodsListWatch) Pods() api.PodList {
	lw.lock.Lock()
	defer lw.lock.Unlock()

	return lw.list
}
func (lw *MockPodsListWatch) GetPod(name string) *api.Pod {
	lw.lock.Lock()
	defer lw.lock.Unlock()

	for _, p := range lw.list.Items {
		if p.Name == name {
			return &p
		}
	}

	return nil
}
func (lw *MockPodsListWatch) Add(pod *api.Pod, notify bool) {
	lw.lock.Lock()
	defer lw.lock.Unlock()

	lw.list.Items = append(lw.list.Items, *pod)
	if notify {
		lw.fakeWatcher.Add(pod)
	}
}
func (lw *MockPodsListWatch) Modify(pod *api.Pod, notify bool) {
	lw.lock.Lock()
	defer lw.lock.Unlock()

	for i, otherPod := range lw.list.Items {
		if otherPod.Name == pod.Name {
			lw.list.Items[i] = *pod
			if notify {
				lw.fakeWatcher.Modify(pod)
			}
			return
		}
	}
	log.Fatalf("Cannot find pod %v to modify in MockPodsListWatch", pod.Name)
}
func (lw *MockPodsListWatch) Delete(pod *api.Pod, notify bool) {
	lw.lock.Lock()
	defer lw.lock.Unlock()

	for i, otherPod := range lw.list.Items {
		if otherPod.Name == pod.Name {
			lw.list.Items = append(lw.list.Items[:i], lw.list.Items[i+1:]...)
			if notify {
				lw.fakeWatcher.Delete(&otherPod)
			}
			return
		}
	}
	log.Fatalf("Cannot find pod %v to delete in MockPodsListWatch", pod.Name)
}

// Create a pod with a given index, requiring one port
func NewTestPod(i int) *api.Pod {
	name := fmt.Sprintf("pod%d", i)
	return &api.Pod{
		TypeMeta: api.TypeMeta{APIVersion: testapi.Version()},
		ObjectMeta: api.ObjectMeta{
			Name:      name,
			Namespace: "default",
			SelfLink:  fmt.Sprintf("http://1.2.3.4/api/v1beta1/pods/%s", name),
		},
		Spec: api.PodSpec{
			Containers: []api.Container{
				{
					Ports: []api.ContainerPort{
						{
							ContainerPort: 8000 + i,
							Protocol:      api.ProtocolTCP,
						},
					},
				},
			},
		},
		Status: api.PodStatus{
			PodIP: fmt.Sprintf("1.2.3.%d", 4+i),
			Conditions: []api.PodCondition{
				{
					Type:   api.PodReady,
					Status: api.ConditionTrue,
				},
			},
		},
	}
}

// Offering some cpus and memory and the 8000-9000 port range
func NewTestOffer(i int) *mesos.Offer {
	hostname := fmt.Sprintf("h%d", i)
	cpus := util.NewScalarResource("cpus", 3.75)
	mem := util.NewScalarResource("mem", 940)
	var port8000 uint64 = 8000
	var port9000 uint64 = 9000
	ports8000to9000 := mesos.Value_Range{Begin: &port8000, End: &port9000}
	ports := util.NewRangesResource("ports", []*mesos.Value_Range{&ports8000to9000})
	return &mesos.Offer{
		Id:        util.NewOfferID(fmt.Sprintf("offer%d", i)),
		Hostname:  &hostname,
		SlaveId:   util.NewSlaveID(hostname),
		Resources: []*mesos.Resource{cpus, mem, ports},
	}
}

// Add assertions to reason about event streams
type Event struct {
	Object  runtime.Object
	Reason  string
	Message string
}

type EventPredicate func(e Event) bool

type EventAssertions struct {
	assert.Assertions
}

// EventObserver implements record.EventRecorder for the purposes of validation via EventAssertions.
type EventObserver struct {
	fifo chan Event
}

func NewEventObserver() *EventObserver {
	return &EventObserver{
		fifo: make(chan Event, 1000),
	}
}
func (o *EventObserver) Event(object runtime.Object, reason, message string) {
	o.fifo <- Event{Object: object, Reason: reason, Message: message}
}
func (o *EventObserver) Eventf(object runtime.Object, reason, messageFmt string, args ...interface{}) {
	o.fifo <- Event{Object: object, Reason: reason, Message: fmt.Sprintf(messageFmt, args...)}
}
func (o *EventObserver) PastEventf(object runtime.Object, timestamp kutil.Time, reason, messageFmt string, args ...interface{}) {
	o.fifo <- Event{Object: object, Reason: reason, Message: fmt.Sprintf(messageFmt, args...)}
}

func (a *EventAssertions) Event(observer *EventObserver, pred EventPredicate, msgAndArgs ...interface{}) bool {
	// parse msgAndArgs: first possibly a duration, otherwise a format string with further args
	timeout := time.Second * 2
	msg := "event not received"
	msgArgStart := 0
	if len(msgAndArgs) > 0 {
		switch msgAndArgs[0].(type) {
		case time.Duration:
			timeout = msgAndArgs[0].(time.Duration)
			msgArgStart += 1
		}
	}
	if len(msgAndArgs) > msgArgStart {
		msg = fmt.Sprintf(msgAndArgs[msgArgStart].(string), msgAndArgs[msgArgStart+1:]...)
	}

	// watch events
	result := make(chan bool)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case e, ok := <-observer.fifo:
				if !ok {
					result <- false
					return
				} else if pred(e) {
					log.V(3).Infof("found asserted event for reason '%v': %v", e.Reason, e.Message)
					result <- true
					return
				} else {
					log.V(5).Infof("ignoring not-asserted event for reason '%v': %v", e.Reason, e.Message)
				}
			case _, ok := <-stop:
				if !ok {
					return
				}
			}
		}
	}()
	defer close(stop)

	// wait for watch to match or timeout
	select {
	case matched := <-result:
		return matched
	case <-time.After(timeout):
		return a.Fail(msg)
	}
}
func (a *EventAssertions) EventWithReason(observer *EventObserver, reason string, msgAndArgs ...interface{}) bool {
	return a.Event(observer, func(e Event) bool {
		return e.Reason == reason
	}, msgAndArgs...)
}

type joinableDriver struct {
	MockSchedulerDriver
	joinFunc func() (mesos.Status, error)
}

// Join invokes joinFunc if it has been set, otherwise blocks forever
func (m *joinableDriver) Join() (mesos.Status, error) {
	if m.joinFunc != nil {
		return m.joinFunc()
	}
	select {}
}

// Create mesos.TaskStatus for a given task
func newTaskStatusForTask(task *mesos.TaskInfo, state mesos.TaskState) *mesos.TaskStatus {
	healthy := state == mesos.TaskState_TASK_RUNNING
	ts := float64(time.Now().Nanosecond()) / 1000000000.0
	source := mesos.TaskStatus_SOURCE_EXECUTOR
	return &mesos.TaskStatus{
		TaskId:     task.TaskId,
		State:      &state,
		SlaveId:    task.SlaveId,
		ExecutorId: task.Executor.ExecutorId,
		Timestamp:  &ts,
		Healthy:    &healthy,
		Source:     &source,
		Data:       task.Data,
	}
}

// Test to create the scheduler plugin with an empty plugin config
func TestPlugin_New(t *testing.T) {
	assert := assert.New(t)

	c := PluginConfig{}
	p := NewPlugin(&c)
	assert.NotNil(p)
}

// Test to create the scheduler plugin with the config returned by the scheduler,
// and play through the whole life cycle of the plugin while creating pods, deleting
// and failing them.
func TestPlugin_LifeCycle(t *testing.T) {
	t.Skip("disabled due to flakiness; see #10795")
	assert := &EventAssertions{*assert.New(t)}

	// create a fake pod watch. We use that below to submit new pods to the scheduler
	podListWatch := NewMockPodsListWatch(api.PodList{})

	// create fake apiserver
	testApiServer := NewTestServer(t, api.NamespaceDefault, podListWatch)
	defer testApiServer.server.Close()

	// create executor with some data for static pods if set
	executor := util.NewExecutorInfo(
		util.NewExecutorID("executor-id"),
		util.NewCommandInfo("executor-cmd"),
	)
	executor.Data = []byte{0, 1, 2}

	// create scheduler
	testScheduler := New(Config{
		Executor:     executor,
		Client:       client.NewOrDie(&client.Config{Host: testApiServer.server.URL, Version: testapi.Version()}),
		ScheduleFunc: FCFSScheduleFunc,
		Schedcfg:     *schedcfg.CreateDefaultConfig(),
	})

	assert.NotNil(testScheduler.client, "client is nil")
	assert.NotNil(testScheduler.executor, "executor is nil")
	assert.NotNil(testScheduler.offers, "offer registry is nil")

	// create scheduler process
	schedulerProcess := ha.New(testScheduler)

	// get plugin config from it
	c := testScheduler.NewPluginConfig(schedulerProcess.Terminal(), http.DefaultServeMux, &podListWatch.ListWatch)
	assert.NotNil(c)

	// make events observable
	eventObserver := NewEventObserver()
	c.Recorder = eventObserver

	// create plugin
	p := NewPlugin(c)
	assert.NotNil(p)

	// run plugin
	p.Run(schedulerProcess.Terminal())
	defer schedulerProcess.End()

	// init scheduler
	err := testScheduler.Init(schedulerProcess.Master(), p, http.DefaultServeMux)
	assert.NoError(err)

	// create mock mesos scheduler driver
	mockDriver := &joinableDriver{}
	mockDriver.On("Start").Return(mesos.Status_DRIVER_RUNNING, nil).Once()
	started := mockDriver.Upon()

	mAny := mock.AnythingOfType
	mockDriver.On("ReconcileTasks", mAny("[]*mesosproto.TaskStatus")).Return(mesos.Status_DRIVER_RUNNING, nil)
	mockDriver.On("SendFrameworkMessage", mAny("*mesosproto.ExecutorID"), mAny("*mesosproto.SlaveID"), mAny("string")).
		Return(mesos.Status_DRIVER_RUNNING, nil)

	launchedTasks := make(chan *mesos.TaskInfo, 1)
	launchTasksCalledFunc := func(args mock.Arguments) {
		taskInfos := args.Get(1).([]*mesos.TaskInfo)
		assert.Equal(1, len(taskInfos))
		launchedTasks <- taskInfos[0]
	}
	mockDriver.On("LaunchTasks", mAny("[]*mesosproto.OfferID"), mAny("[]*mesosproto.TaskInfo"), mAny("*mesosproto.Filters")).
		Return(mesos.Status_DRIVER_RUNNING, nil).Run(launchTasksCalledFunc)
	mockDriver.On("DeclineOffer", mAny("*mesosproto.OfferID"), mAny("*mesosproto.Filters")).
		Return(mesos.Status_DRIVER_RUNNING, nil)

	// elect master with mock driver
	driverFactory := ha.DriverFactory(func() (bindings.SchedulerDriver, error) {
		return mockDriver, nil
	})
	schedulerProcess.Elect(driverFactory)
	elected := schedulerProcess.Elected()

	// driver will be started
	<-started

	// tell scheduler to be registered
	testScheduler.Registered(
		mockDriver,
		util.NewFrameworkID("kubernetes-id"),
		util.NewMasterInfo("master-id", (192<<24)+(168<<16)+(0<<8)+1, 5050),
	)

	// wait for being elected
	<-elected

	//TODO(jdef) refactor things above here into a test suite setup of some sort

	// fake new, unscheduled pod
	pod1 := NewTestPod(1)
	podListWatch.Add(pod1, true) // notify watchers

	// wait for failedScheduling event because there is no offer
	assert.EventWithReason(eventObserver, "failedScheduling", "failedScheduling event not received")

	// add some matching offer
	offers1 := []*mesos.Offer{NewTestOffer(1)}
	testScheduler.ResourceOffers(nil, offers1)

	// and wait for scheduled pod
	assert.EventWithReason(eventObserver, "scheduled")
	select {
	case launchedTask := <-launchedTasks:
		// report back that the task has been staged, and then started by mesos
		testScheduler.StatusUpdate(mockDriver, newTaskStatusForTask(launchedTask, mesos.TaskState_TASK_STAGING))
		testScheduler.StatusUpdate(mockDriver, newTaskStatusForTask(launchedTask, mesos.TaskState_TASK_RUNNING))

		// check that ExecutorInfo.data has the static pod data
		assert.Len(launchedTask.Executor.Data, 3)

		// report back that the task has been lost
		mockDriver.AssertNumberOfCalls(t, "SendFrameworkMessage", 0)
		testScheduler.StatusUpdate(mockDriver, newTaskStatusForTask(launchedTask, mesos.TaskState_TASK_LOST))

		// and wait that framework message is sent to executor
		mockDriver.AssertNumberOfCalls(t, "SendFrameworkMessage", 1)

	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for launchTasks call")
	}

	// start another pod
	podNum := 1
	startPod := func(offers []*mesos.Offer) (*api.Pod, *mesos.TaskInfo) {
		podNum = podNum + 1

		// create pod and matching offer
		pod := NewTestPod(podNum)
		podListWatch.Add(pod, true) // notify watchers
		testScheduler.ResourceOffers(mockDriver, offers)
		assert.EventWithReason(eventObserver, "scheduled")

		// wait for driver.launchTasks call
		select {
		case launchedTask := <-launchedTasks:
			testScheduler.StatusUpdate(mockDriver, newTaskStatusForTask(launchedTask, mesos.TaskState_TASK_STAGING))
			testScheduler.StatusUpdate(mockDriver, newTaskStatusForTask(launchedTask, mesos.TaskState_TASK_RUNNING))
			return pod, launchedTask

		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for launchTasks")
			return nil, nil
		}
	}

	pod, launchedTask := startPod(offers1)

	// mock drvier.KillTask, should be invoked when a pod is deleted
	mockDriver.On("KillTask", mAny("*mesosproto.TaskID")).Return(mesos.Status_DRIVER_RUNNING, nil).Run(func(args mock.Arguments) {
		killedTaskId := *(args.Get(0).(*mesos.TaskID))
		assert.Equal(*launchedTask.TaskId, killedTaskId, "expected same TaskID as during launch")
	})
	killTaskCalled := mockDriver.Upon()

	// stop it again via the apiserver mock
	podListWatch.Delete(pod, true) // notify watchers

	// and wait for the driver killTask call with the correct TaskId
	select {
	case <-killTaskCalled:
		// report back that the task is finished
		testScheduler.StatusUpdate(mockDriver, newTaskStatusForTask(launchedTask, mesos.TaskState_TASK_FINISHED))

	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for KillTask")
	}

	// start pods:
	// - which are failing while binding,
	// - leading to reconciliation
	// - with different states on the apiserver

	failPodFromExecutor := func(task *mesos.TaskInfo) {
		beforePodLookups := testApiServer.Stats(pod.Name)
		status := newTaskStatusForTask(task, mesos.TaskState_TASK_FAILED)
		message := messages.CreateBindingFailure
		status.Message = &message
		testScheduler.StatusUpdate(mockDriver, status)

		// wait until pod is looked up at the apiserver
		assertext.EventuallyTrue(t, time.Second, func() bool {
			return testApiServer.Stats(pod.Name) == beforePodLookups+1
		}, "expect that reconcilePod will access apiserver for pod %v", pod.Name)
	}

	// 1. with pod deleted from the apiserver
	pod, launchedTask = startPod(offers1)
	podListWatch.Delete(pod, false) // not notifying the watchers
	failPodFromExecutor(launchedTask)

	// 2. with pod still on the apiserver, not bound
	pod, launchedTask = startPod(offers1)
	failPodFromExecutor(launchedTask)

	// 3. with pod still on the apiserver, bound i.e. host!=""
	pod, launchedTask = startPod(offers1)
	pod.Spec.NodeName = *offers1[0].Hostname
	podListWatch.Modify(pod, false) // not notifying the watchers
	failPodFromExecutor(launchedTask)

	// 4. with pod still on the apiserver, bound i.e. host!="", notified via ListWatch
	pod, launchedTask = startPod(offers1)
	pod.Spec.NodeName = *offers1[0].Hostname
	podListWatch.Modify(pod, true) // notifying the watchers
	time.Sleep(time.Second / 2)
	failPodFromExecutor(launchedTask)
}

func TestDeleteOne_NonexistentPod(t *testing.T) {
	assert := assert.New(t)
	obj := &MockScheduler{}
	reg := podtask.NewInMemoryRegistry()
	obj.On("tasks").Return(reg)

	qr := newQueuer(nil)
	assert.Equal(0, len(qr.podQueue.List()))
	d := &deleter{
		api: obj,
		qr:  qr,
	}
	pod := &Pod{Pod: &api.Pod{
		ObjectMeta: api.ObjectMeta{
			Name:      "foo",
			Namespace: api.NamespaceDefault,
		}}}
	err := d.deleteOne(pod)
	assert.Equal(err, noSuchPodErr)
	obj.AssertExpectations(t)
}

func TestDeleteOne_PendingPod(t *testing.T) {
	assert := assert.New(t)
	obj := &MockScheduler{}
	reg := podtask.NewInMemoryRegistry()
	obj.On("tasks").Return(reg)

	pod := &Pod{Pod: &api.Pod{
		ObjectMeta: api.ObjectMeta{
			Name:      "foo",
			UID:       "foo0",
			Namespace: api.NamespaceDefault,
		}}}
	_, err := reg.Register(podtask.New(api.NewDefaultContext(), "bar", *pod.Pod, &mesos.ExecutorInfo{}))
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	// preconditions
	qr := newQueuer(nil)
	qr.podQueue.Add(pod, queue.ReplaceExisting)
	assert.Equal(1, len(qr.podQueue.List()))
	_, found := qr.podQueue.Get("default/foo")
	assert.True(found)

	// exec & post conditions
	d := &deleter{
		api: obj,
		qr:  qr,
	}
	err = d.deleteOne(pod)
	assert.Nil(err)
	_, found = qr.podQueue.Get("foo0")
	assert.False(found)
	assert.Equal(0, len(qr.podQueue.List()))
	obj.AssertExpectations(t)
}

func TestDeleteOne_Running(t *testing.T) {
	assert := assert.New(t)
	obj := &MockScheduler{}
	reg := podtask.NewInMemoryRegistry()
	obj.On("tasks").Return(reg)

	pod := &Pod{Pod: &api.Pod{
		ObjectMeta: api.ObjectMeta{
			Name:      "foo",
			UID:       "foo0",
			Namespace: api.NamespaceDefault,
		}}}
	task, err := reg.Register(podtask.New(api.NewDefaultContext(), "bar", *pod.Pod, &mesos.ExecutorInfo{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	task.Set(podtask.Launched)
	err = reg.Update(task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// preconditions
	qr := newQueuer(nil)
	qr.podQueue.Add(pod, queue.ReplaceExisting)
	assert.Equal(1, len(qr.podQueue.List()))
	_, found := qr.podQueue.Get("default/foo")
	assert.True(found)

	obj.On("killTask", task.ID).Return(nil)

	// exec & post conditions
	d := &deleter{
		api: obj,
		qr:  qr,
	}
	err = d.deleteOne(pod)
	assert.Nil(err)
	_, found = qr.podQueue.Get("foo0")
	assert.False(found)
	assert.Equal(0, len(qr.podQueue.List()))
	obj.AssertExpectations(t)
}

func TestDeleteOne_badPodNaming(t *testing.T) {
	assert := assert.New(t)
	obj := &MockScheduler{}
	pod := &Pod{Pod: &api.Pod{}}
	d := &deleter{
		api: obj,
		qr:  newQueuer(nil),
	}

	err := d.deleteOne(pod)
	assert.NotNil(err)

	pod.Pod.ObjectMeta.Name = "foo"
	err = d.deleteOne(pod)
	assert.NotNil(err)

	pod.Pod.ObjectMeta.Name = ""
	pod.Pod.ObjectMeta.Namespace = "bar"
	err = d.deleteOne(pod)
	assert.NotNil(err)

	obj.AssertExpectations(t)
}
