package launcher

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/mock/gomock"
	"github.com/pborman/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/uber-go/tally"

	mesos "code.uber.internal/infra/peloton/.gen/mesos/v1"
	"code.uber.internal/infra/peloton/.gen/peloton/api/job"
	"code.uber.internal/infra/peloton/.gen/peloton/api/peloton"
	"code.uber.internal/infra/peloton/.gen/peloton/api/task"
	"code.uber.internal/infra/peloton/.gen/peloton/api/volume"
	"code.uber.internal/infra/peloton/.gen/peloton/private/hostmgr/hostsvc"
	host_mocks "code.uber.internal/infra/peloton/.gen/peloton/private/hostmgr/hostsvc/mocks"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgr"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc"
	res_mocks "code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc/mocks"

	"code.uber.internal/infra/peloton/common/backoff"
	store_mocks "code.uber.internal/infra/peloton/storage/mocks"
	"code.uber.internal/infra/peloton/util"
)

const (
	taskIDFmt   = "testjob-%d-%s"
	testJobName = "testjob"
	testPort    = uint32(100)
)

var (
	_defaultResourceConfig = task.ResourceConfig{
		CpuLimit:    10,
		MemLimitMb:  10,
		DiskLimitMb: 10,
		FdLimit:     10,
	}
	_jobID = uuid.NewUUID().String()
	_sla   = &job.SlaConfig{
		Preemptible: false,
	}
	_jobConfig = &job.JobConfig{
		Name:          _jobID,
		Sla:           _sla,
		InstanceCount: 1,
	}
	lock = sync.RWMutex{}
)

func createTestTask(instanceID int) *task.TaskInfo {
	var tid = fmt.Sprintf(taskIDFmt, instanceID, uuid.NewUUID().String())

	return &task.TaskInfo{
		JobId: &peloton.JobID{
			Value: testJobName,
		},
		InstanceId: uint32(instanceID),
		Config: &task.TaskConfig{
			Name:     testJobName,
			Resource: &_defaultResourceConfig,
			Ports: []*task.PortConfig{
				{
					Name:    "port",
					EnvName: "PORT",
				},
			},
		},
		Runtime: &task.RuntimeInfo{
			MesosTaskId: &mesos.TaskID{
				Value: &tid,
			},
		},
	}
}

func createResources(defaultMultiplier float64) []*mesos.Resource {
	values := map[string]float64{
		"cpus": defaultMultiplier * _defaultResourceConfig.CpuLimit,
		"mem":  defaultMultiplier * _defaultResourceConfig.MemLimitMb,
		"disk": defaultMultiplier * _defaultResourceConfig.DiskLimitMb,
		"gpus": defaultMultiplier * _defaultResourceConfig.GpuLimit,
	}
	return util.CreateMesosScalarResources(values, "*")
}

func createHostOffer(hostID int, resources []*mesos.Resource) *hostsvc.HostOffer {
	agentID := fmt.Sprintf("agent-%d", hostID)
	return &hostsvc.HostOffer{
		Hostname: fmt.Sprintf("hostname-%d", hostID),
		AgentId: &mesos.AgentID{
			Value: &agentID,
		},
		Resources: resources,
	}
}

// This test ensures that multiple placements returned from resmgr can be properly placed by hostmgr
func TestMultipleTasksPlaced(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRes := res_mocks.NewMockResourceManagerServiceYARPCClient(ctrl)
	mockHostMgr := host_mocks.NewMockInternalHostServiceYARPCClient(ctrl)
	mockTaskStore := store_mocks.NewMockTaskStore(ctrl)
	testScope := tally.NewTestScope("", map[string]string{})
	metrics := NewMetrics(testScope)
	taskLauncher := launcher{
		hostMgrClient: mockHostMgr,
		taskStore:     mockTaskStore,
		metrics:       metrics,
		retryPolicy:   backoff.NewRetryPolicy(5, 15*time.Millisecond),
	}

	// generate 25 test tasks
	numTasks := 1
	testTasks := make([]*task.TaskInfo, numTasks)
	taskIDs := make([]*peloton.TaskID, numTasks)
	placements := make([]*resmgr.Placement, numTasks)
	taskConfigs := make(map[string]*task.TaskConfig)
	taskIds := make(map[string]*peloton.TaskID)
	for i := 0; i < numTasks; i++ {
		tmp := createTestTask(i)
		taskID := &peloton.TaskID{
			Value: tmp.JobId.Value + "-" + fmt.Sprint(tmp.InstanceId),
		}
		taskIDs[i] = taskID
		testTasks[i] = tmp
		taskConfigs[tmp.GetRuntime().GetMesosTaskId().GetValue()] = tmp.Config
		taskIds[taskID.Value] = taskID
	}

	// generate 1 host offer, each can hold 1 tasks.
	numHostOffers := 1
	rs := createResources(1)
	var hostOffers []*hostsvc.HostOffer
	for i := 0; i < numHostOffers; i++ {
		hostOffers = append(hostOffers, createHostOffer(i, rs))
	}

	// Generate Placements per host offer
	for i := 0; i < numHostOffers; i++ {
		p := createPlacements(testTasks[i], hostOffers[i])
		placements[i] = p
	}

	// Capture LaunchTasks calls
	hostsLaunchedOn := make(map[string]bool)
	launchedTasks := make(map[string]*task.TaskConfig)

	updatedRuntime := proto.Clone(testTasks[0].Runtime).(*task.RuntimeInfo)
	updatedRuntime.Host = hostOffers[0].Hostname
	updatedRuntime.AgentID = hostOffers[0].GetAgentId()
	updatedRuntime.Ports = make(map[string]uint32)
	updatedRuntime.Ports["port"] = testPort
	updatedRuntime.State = task.TaskState_LAUNCHED
	updatedRuntime.Reason = "REASON_UPDATE_OFFER"
	updatedRuntime.Message = "Add hostname and ports"

	gomock.InOrder(
		// Mock Task Store GetTaskByID
		mockTaskStore.EXPECT().GetTaskByID(gomock.Any(), taskIDs[0].Value).Return(testTasks[0], nil),

		// Mock Task Store UpdateTask
		mockTaskStore.EXPECT().UpdateTaskRuntime(gomock.Any(), testTasks[0].JobId, testTasks[0].InstanceId, updatedRuntime).Return(nil),

		// Mock LaunchTasks call.
		mockHostMgr.EXPECT().
			LaunchTasks(
				gomock.Any(),
				gomock.Any()).
			Do(func(_ context.Context, reqBody interface{}) {
				// No need to unmarksnal output: empty means success.
				// Capture call since we don't know ordering of tasks.
				lock.Lock()
				defer lock.Unlock()
				req := reqBody.(*hostsvc.LaunchTasksRequest)
				hostsLaunchedOn[req.Hostname] = true
				for _, lt := range req.Tasks {
					launchedTasks[lt.TaskId.GetValue()] = lt.Config
				}
			}).
			Return(&hostsvc.LaunchTasksResponse{}, nil).
			Times(1),
	)

	taskLauncher.ProcessPlacements(context.Background(), mockRes, placements)

	time.Sleep(1 * time.Second)
	expectedLaunchedHosts := map[string]bool{
		"hostname-0": true,
	}
	lock.Lock()
	defer lock.Unlock()
	assert.Equal(t, expectedLaunchedHosts, hostsLaunchedOn)
	assert.Equal(t, taskConfigs, launchedTasks)
}

// This test ensures that tasks got rescheduled when launched got invalid offer resp.
func TestLaunchTasksWithInvalidOfferResponse(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRes := res_mocks.NewMockResourceManagerServiceYARPCClient(ctrl)
	mockHostMgr := host_mocks.NewMockInternalHostServiceYARPCClient(ctrl)
	mockJobStore := store_mocks.NewMockJobStore(ctrl)
	mockTaskStore := store_mocks.NewMockTaskStore(ctrl)
	testScope := tally.NewTestScope("", map[string]string{})
	metrics := NewMetrics(testScope)
	taskLauncher := launcher{
		hostMgrClient: mockHostMgr,
		jobStore:      mockJobStore,
		taskStore:     mockTaskStore,
		metrics:       metrics,
		retryPolicy:   backoff.NewRetryPolicy(5, 15*time.Millisecond),
	}

	numTasks := 1
	testTasks := make([]*task.TaskInfo, numTasks)
	taskIDs := make([]*peloton.TaskID, numTasks)
	placements := make([]*resmgr.Placement, numTasks)
	taskConfigs := make(map[string]*task.TaskConfig)
	taskIds := make(map[string]*peloton.TaskID)
	for i := 0; i < numTasks; i++ {
		tmp := createTestTask(i)
		taskID := &peloton.TaskID{
			Value: tmp.JobId.Value + "-" + fmt.Sprint(tmp.InstanceId),
		}
		taskIDs[i] = taskID
		testTasks[i] = tmp
		taskConfigs[tmp.GetRuntime().GetMesosTaskId().GetValue()] = tmp.Config
		taskIds[taskID.Value] = taskID
	}

	// generate 1 host offer, each can hold 1 tasks.
	numHostOffers := 1
	rs := createResources(1)
	var hostOffers []*hostsvc.HostOffer
	for i := 0; i < numHostOffers; i++ {
		hostOffers = append(hostOffers, createHostOffer(i, rs))
	}

	// Generate Placements per host offer
	for i := 0; i < numHostOffers; i++ {
		p := createPlacements(testTasks[i], hostOffers[i])
		placements[i] = p
	}

	// Capture LaunchTasks calls
	hostsLaunchedOn := make(map[string]bool)
	launchedTasks := make(map[string]*task.TaskConfig)

	updatedRuntime := proto.Clone(testTasks[0].Runtime).(*task.RuntimeInfo)
	updatedRuntime.Host = hostOffers[0].Hostname
	updatedRuntime.AgentID = hostOffers[0].GetAgentId()
	updatedRuntime.Ports = make(map[string]uint32)
	updatedRuntime.Ports["port"] = testPort
	updatedRuntime.State = task.TaskState_LAUNCHED
	updatedRuntime.Reason = "REASON_UPDATE_OFFER"
	updatedRuntime.Message = "Add hostname and ports"

	gomock.InOrder(
		// Mock Task Store GetTaskByID
		mockTaskStore.EXPECT().GetTaskByID(gomock.Any(), taskIDs[0].Value).Return(testTasks[0], nil),

		// Mock Task Store UpdateTask
		mockTaskStore.EXPECT().UpdateTaskRuntime(gomock.Any(), testTasks[0].JobId, testTasks[0].InstanceId, updatedRuntime).Return(nil),

		// Mock LaunchTasks call.
		mockHostMgr.EXPECT().
			LaunchTasks(
				gomock.Any(),
				gomock.Any()).
			Do(func(_ context.Context, reqBody interface{}) {
				// No need to unmarksnal output: empty means success.
				// Capture call since we don't know ordering of tasks.
				lock.Lock()
				defer lock.Unlock()
				req := reqBody.(*hostsvc.LaunchTasksRequest)
				hostsLaunchedOn[req.Hostname] = true
				for _, lt := range req.Tasks {
					launchedTasks[lt.TaskId.GetValue()] = lt.Config
				}
			}).
			Return(&hostsvc.LaunchTasksResponse{
				Error: &hostsvc.LaunchTasksResponse_Error{
					InvalidOffers: &hostsvc.InvalidOffers{
						Message: "invalid offer failure",
					},
				},
			}, nil).
			Times(1),

		mockTaskStore.EXPECT().UpdateTaskRuntime(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil),

		mockJobStore.EXPECT().GetJobConfig(gomock.Any(), testTasks[0].JobId).Return(_jobConfig, nil),

		mockRes.EXPECT().EnqueueGangs(
			gomock.Any(),
			gomock.Any(),
		).Return(&resmgrsvc.EnqueueGangsResponse{}, nil),
	)

	taskLauncher.ProcessPlacements(context.Background(), mockRes, placements)

	time.Sleep(1 * time.Second)
	expectedLaunchedHosts := map[string]bool{
		"hostname-0": true,
	}
	lock.Lock()
	defer lock.Unlock()
	assert.Equal(t, expectedLaunchedHosts, hostsLaunchedOn)
	assert.Equal(t, taskConfigs, launchedTasks)
	assert.Equal(
		t,
		int64(0),
		testScope.Snapshot().Counters()["launch_tasks.retry+"].Value())
}

func TestLaunchTasksRetryWithError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRes := res_mocks.NewMockResourceManagerServiceYARPCClient(ctrl)
	mockHostMgr := host_mocks.NewMockInternalHostServiceYARPCClient(ctrl)
	mockJobStore := store_mocks.NewMockJobStore(ctrl)
	mockTaskStore := store_mocks.NewMockTaskStore(ctrl)
	testScope := tally.NewTestScope("", map[string]string{})
	metrics := NewMetrics(testScope)
	taskLauncher := launcher{
		hostMgrClient: mockHostMgr,
		jobStore:      mockJobStore,
		taskStore:     mockTaskStore,
		metrics:       metrics,
		retryPolicy:   backoff.NewRetryPolicy(5, 15*time.Millisecond),
	}

	numTasks := 1
	testTasks := make([]*task.TaskInfo, numTasks)
	taskIDs := make([]*peloton.TaskID, numTasks)
	placements := make([]*resmgr.Placement, numTasks)
	taskConfigs := make(map[string]*task.TaskConfig)
	taskIds := make(map[string]*peloton.TaskID)
	for i := 0; i < numTasks; i++ {
		tmp := createTestTask(i)
		taskID := &peloton.TaskID{
			Value: tmp.JobId.Value + "-" + fmt.Sprint(tmp.InstanceId),
		}
		taskIDs[i] = taskID
		testTasks[i] = tmp
		taskConfigs[tmp.GetRuntime().GetMesosTaskId().GetValue()] = tmp.Config
		taskIds[taskID.Value] = taskID
	}

	// generate 1 host offer, each can hold 1 tasks.
	numHostOffers := 1
	rs := createResources(1)
	var hostOffers []*hostsvc.HostOffer
	for i := 0; i < numHostOffers; i++ {
		hostOffers = append(hostOffers, createHostOffer(i, rs))
	}

	// Generate Placements per host offer
	for i := 0; i < numHostOffers; i++ {
		p := createPlacements(testTasks[i], hostOffers[i])
		placements[i] = p
	}

	// Capture LaunchTasks calls
	hostsLaunchedOn := make(map[string]bool)
	launchedTasks := make(map[string]*task.TaskConfig)

	updatedRuntime := proto.Clone(testTasks[0].Runtime).(*task.RuntimeInfo)
	updatedRuntime.Host = hostOffers[0].Hostname
	updatedRuntime.AgentID = hostOffers[0].GetAgentId()
	updatedRuntime.Ports = make(map[string]uint32)
	updatedRuntime.Ports["port"] = testPort
	updatedRuntime.State = task.TaskState_LAUNCHED
	updatedRuntime.Reason = "REASON_UPDATE_OFFER"
	updatedRuntime.Message = "Add hostname and ports"

	gomock.InOrder(
		// Mock Task Store GetTaskByID
		mockTaskStore.EXPECT().GetTaskByID(gomock.Any(), taskIDs[0].Value).Return(testTasks[0], nil),

		// Mock Task Store UpdateTask
		mockTaskStore.EXPECT().UpdateTaskRuntime(gomock.Any(), testTasks[0].JobId, testTasks[0].InstanceId, updatedRuntime).Return(nil),

		// Mock LaunchTasks call.
		mockHostMgr.EXPECT().
			LaunchTasks(
				gomock.Any(),
				gomock.Any()).
			Do(func(_ context.Context, reqBody interface{}) {
				// No need to unmarksnal output: empty means success.
				// Capture call since we don't know ordering of tasks.
				lock.Lock()
				defer lock.Unlock()
				req := reqBody.(*hostsvc.LaunchTasksRequest)
				hostsLaunchedOn[req.Hostname] = true
				for _, lt := range req.Tasks {
					launchedTasks[lt.TaskId.GetValue()] = lt.Config
				}
			}).
			Return(&hostsvc.LaunchTasksResponse{
				Error: &hostsvc.LaunchTasksResponse_Error{
					InvalidArgument: &hostsvc.InvalidArgument{
						Message: "invalid argument failure",
					},
				},
			}, nil).
			Times(5),

		// Mock ReleaseOffer call.
		mockHostMgr.EXPECT().ReleaseHostOffers(gomock.Any(), gomock.Any()).
			Return(&hostsvc.ReleaseHostOffersResponse{}, nil),

		mockTaskStore.EXPECT().UpdateTaskRuntime(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil),

		mockJobStore.EXPECT().GetJobConfig(gomock.Any(), testTasks[0].JobId).Return(_jobConfig, nil),

		// Mock EnqueueGangs call.
		mockRes.EXPECT().EnqueueGangs(
			gomock.Any(),
			gomock.Any(),
		).Return(&resmgrsvc.EnqueueGangsResponse{}, nil),
	)

	taskLauncher.ProcessPlacements(context.Background(), mockRes, placements)

	time.Sleep(1 * time.Second)
	expectedLaunchedHosts := map[string]bool{
		"hostname-0": true,
	}
	lock.Lock()
	defer lock.Unlock()
	assert.Equal(t, expectedLaunchedHosts, hostsLaunchedOn)
	assert.Equal(t, taskConfigs, launchedTasks)
	assert.Equal(
		t,
		int64(4),
		testScope.Snapshot().Counters()["launch_tasks.retry+"].Value())
}

func TestLaunchStatefulTask(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRes := res_mocks.NewMockResourceManagerServiceYARPCClient(ctrl)
	mockHostMgr := host_mocks.NewMockInternalHostServiceYARPCClient(ctrl)
	mockTaskStore := store_mocks.NewMockTaskStore(ctrl)
	mockVolumeStore := store_mocks.NewMockPersistentVolumeStore(ctrl)

	testScope := tally.NewTestScope("", map[string]string{})
	metrics := NewMetrics(testScope)
	taskLauncher := launcher{
		hostMgrClient: mockHostMgr,
		taskStore:     mockTaskStore,
		volumeStore:   mockVolumeStore,
		metrics:       metrics,
		retryPolicy:   backoff.NewRetryPolicy(5, 15*time.Millisecond),
	}

	numTasks := 1
	testTasks := make([]*task.TaskInfo, numTasks)
	taskIDs := make([]*peloton.TaskID, numTasks)
	placements := make([]*resmgr.Placement, numTasks)
	taskConfigs := make(map[string]*task.TaskConfig)
	taskIds := make(map[string]*peloton.TaskID)
	for i := 0; i < numTasks; i++ {
		tmp := createTestTask(i)
		tmp.GetConfig().Volume = &task.PersistentVolumeConfig{
			ContainerPath: "testpath",
			SizeMB:        10,
		}
		taskID := &peloton.TaskID{
			Value: tmp.JobId.Value + "-" + fmt.Sprint(tmp.InstanceId),
		}
		taskIDs[i] = taskID
		testTasks[i] = tmp
		taskConfigs[tmp.GetRuntime().GetMesosTaskId().GetValue()] = tmp.Config
		taskIds[taskID.Value] = taskID
	}

	// generate 1 host offer, each can hold 1 tasks.
	numHostOffers := 1
	rs := createResources(1)
	var hostOffers []*hostsvc.HostOffer
	for i := 0; i < numHostOffers; i++ {
		hostOffers = append(hostOffers, createHostOffer(i, rs))
	}

	// Generate Placements per host offer
	for i := 0; i < numHostOffers; i++ {
		p := createPlacements(testTasks[i], hostOffers[i])
		p.Type = resmgr.TaskType_STATEFUL
		placements[i] = p
	}

	// Capture OfferOperation calls
	hostsLaunchedOn := make(map[string]bool)

	volumeInfo := &volume.PersistentVolumeInfo{}
	gomock.InOrder(
		// Mock Task Store GetTaskByID
		mockTaskStore.EXPECT().GetTaskByID(gomock.Any(), taskIDs[0].Value).Return(testTasks[0], nil),

		// Mock Task Store UpdateTask
		mockTaskStore.EXPECT().UpdateTaskRuntime(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil),

		mockVolumeStore.EXPECT().
			GetPersistentVolume(gomock.Any(), gomock.Any()).
			Return(volumeInfo, nil),

		// Mock OfferOperation call.
		mockHostMgr.EXPECT().
			OfferOperations(
				gomock.Any(),
				gomock.Any()).
			Do(func(_ context.Context, reqBody interface{}) {
				// No need to unmarksnal output: empty means success.
				// Capture call since we don't know ordering of tasks.
				lock.Lock()
				defer lock.Unlock()
				req := reqBody.(*hostsvc.OfferOperationsRequest)
				hostsLaunchedOn[req.Hostname] = true
				assert.Equal(t, 3, len(req.Operations))
			}).
			Return(&hostsvc.OfferOperationsResponse{}, nil).
			Times(1),
	)

	taskLauncher.ProcessPlacements(context.Background(), mockRes, placements)

	time.Sleep(1 * time.Second)
	expectedLaunchedHosts := map[string]bool{
		"hostname-0": true,
	}
	lock.Lock()
	defer lock.Unlock()
	assert.Equal(t, expectedLaunchedHosts, hostsLaunchedOn)
}

func TestLaunchStatefulTaskLaunchWithVolume(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRes := res_mocks.NewMockResourceManagerServiceYARPCClient(ctrl)
	mockHostMgr := host_mocks.NewMockInternalHostServiceYARPCClient(ctrl)
	mockTaskStore := store_mocks.NewMockTaskStore(ctrl)
	mockVolumeStore := store_mocks.NewMockPersistentVolumeStore(ctrl)

	testScope := tally.NewTestScope("", map[string]string{})
	metrics := NewMetrics(testScope)
	taskLauncher := launcher{
		hostMgrClient: mockHostMgr,
		taskStore:     mockTaskStore,
		volumeStore:   mockVolumeStore,
		metrics:       metrics,
		retryPolicy:   backoff.NewRetryPolicy(5, 15*time.Millisecond),
	}

	numTasks := 1
	testTasks := make([]*task.TaskInfo, numTasks)
	taskIDs := make([]*peloton.TaskID, numTasks)
	placements := make([]*resmgr.Placement, numTasks)
	taskConfigs := make(map[string]*task.TaskConfig)
	taskIds := make(map[string]*peloton.TaskID)
	for i := 0; i < numTasks; i++ {
		tmp := createTestTask(i)
		tmp.GetConfig().Volume = &task.PersistentVolumeConfig{
			ContainerPath: "testpath",
			SizeMB:        10,
		}
		taskID := &peloton.TaskID{
			Value: tmp.JobId.Value + "-" + fmt.Sprint(tmp.InstanceId),
		}
		taskIDs[i] = taskID
		testTasks[i] = tmp
		taskConfigs[tmp.GetRuntime().GetMesosTaskId().GetValue()] = tmp.Config
		taskIds[taskID.Value] = taskID
	}

	// generate 1 host offer, each can hold 1 tasks.
	numHostOffers := 1
	rs := createResources(1)
	var hostOffers []*hostsvc.HostOffer
	for i := 0; i < numHostOffers; i++ {
		hostOffers = append(hostOffers, createHostOffer(i, rs))
	}

	// Generate Placements per host offer
	for i := 0; i < numHostOffers; i++ {
		p := createPlacements(testTasks[i], hostOffers[i])
		p.Type = resmgr.TaskType_STATEFUL
		placements[i] = p
	}

	// Capture OfferOperation calls
	hostsLaunchedOn := make(map[string]bool)

	volumeInfo := &volume.PersistentVolumeInfo{
		State: volume.VolumeState_CREATED,
	}
	gomock.InOrder(
		// Mock Task Store GetTaskByID
		mockTaskStore.EXPECT().GetTaskByID(gomock.Any(), taskIDs[0].Value).Return(testTasks[0], nil),

		// Mock Task Store UpdateTask
		mockTaskStore.EXPECT().UpdateTaskRuntime(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil),

		mockVolumeStore.EXPECT().
			GetPersistentVolume(gomock.Any(), gomock.Any()).
			Return(volumeInfo, nil),

		// Mock OfferOperation call.
		mockHostMgr.EXPECT().
			OfferOperations(
				gomock.Any(),
				gomock.Any()).
			Do(func(_ context.Context, reqBody interface{}) {
				// No need to unmarksnal output: empty means success.
				// Capture call since we don't know ordering of tasks.
				lock.Lock()
				defer lock.Unlock()
				req := reqBody.(*hostsvc.OfferOperationsRequest)
				hostsLaunchedOn[req.Hostname] = true
				assert.Equal(t, 1, len(req.Operations))
			}).
			Return(&hostsvc.OfferOperationsResponse{}, nil).
			Times(1),
	)

	taskLauncher.ProcessPlacements(context.Background(), mockRes, placements)

	time.Sleep(1 * time.Second)
	expectedLaunchedHosts := map[string]bool{
		"hostname-0": true,
	}
	lock.Lock()
	defer lock.Unlock()
	assert.Equal(t, expectedLaunchedHosts, hostsLaunchedOn)
}

func TestProcessPlacementsWithNoTasksReleasesOffers(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockHostMgr := host_mocks.NewMockInternalHostServiceYARPCClient(ctrl)
	taskLauncher := launcher{
		hostMgrClient: mockHostMgr,
		metrics:       NewMetrics(tally.NoopScope),
		retryPolicy:   backoff.NewRetryPolicy(5, 15*time.Millisecond),
	}

	// Mock OfferOperation call.
	mockHostMgr.EXPECT().ReleaseHostOffers(gomock.Any(), &hostsvc.ReleaseHostOffersRequest{
		HostOffers: []*hostsvc.HostOffer{{
			Hostname: "hostname-0",
			AgentId:  &mesos.AgentID{},
		}},
	}).
		Return(&hostsvc.ReleaseHostOffersResponse{}, nil)

	taskLauncher.ProcessPlacements(context.Background(), nil, []*resmgr.Placement{{
		Hostname: "hostname-0",
		AgentId:  &mesos.AgentID{},
	}})

	time.Sleep(1 * time.Second)
}

func TestLaunchStatefulTaskLaunchWithReservedResourceDirectly(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockHostMgr := host_mocks.NewMockInternalHostServiceYARPCClient(ctrl)
	mockTaskStore := store_mocks.NewMockTaskStore(ctrl)
	mockVolumeStore := store_mocks.NewMockPersistentVolumeStore(ctrl)

	testScope := tally.NewTestScope("", map[string]string{})
	metrics := NewMetrics(testScope)
	taskLauncher := launcher{
		hostMgrClient: mockHostMgr,
		taskStore:     mockTaskStore,
		volumeStore:   mockVolumeStore,
		metrics:       metrics,
		retryPolicy:   backoff.NewRetryPolicy(5, 15*time.Millisecond),
	}

	numTasks := 1
	testTasks := make([]*task.TaskInfo, numTasks)
	taskIDs := make([]*peloton.TaskID, numTasks)
	placements := make([]*resmgr.Placement, numTasks)
	taskConfigs := make(map[string]*task.TaskConfig)
	taskIds := make(map[string]*peloton.TaskID)
	var testTask *task.TaskInfo
	for i := 0; i < numTasks; i++ {
		tmp := createTestTask(i)
		tmp.GetConfig().Volume = &task.PersistentVolumeConfig{
			ContainerPath: "testpath",
			SizeMB:        10,
		}
		tmp.GetRuntime().Host = "hostname-0"
		testTask = tmp
		taskID := &peloton.TaskID{
			Value: tmp.JobId.Value + "-" + fmt.Sprint(tmp.InstanceId),
		}
		taskIDs[i] = taskID
		testTasks[i] = tmp
		taskConfigs[tmp.GetRuntime().GetMesosTaskId().GetValue()] = tmp.Config
		taskIds[taskID.Value] = taskID
	}

	// generate 1 host offer, each can hold 1 tasks.
	numHostOffers := 1
	rs := createResources(1)
	var hostOffers []*hostsvc.HostOffer
	for i := 0; i < numHostOffers; i++ {
		hostOffers = append(hostOffers, createHostOffer(i, rs))
	}

	// Generate Placements per host offer
	for i := 0; i < numHostOffers; i++ {
		p := createPlacements(testTasks[i], hostOffers[i])
		p.Type = resmgr.TaskType_STATEFUL
		placements[i] = p
	}

	// Capture OfferOperation calls
	hostsLaunchedOn := make(map[string]bool)

	gomock.InOrder(
		// Mock Task Store GetTaskByID
		mockTaskStore.EXPECT().GetTaskByID(gomock.Any(), taskIDs[0].Value).Return(testTasks[0], nil),

		// Mock Task Store UpdateTask
		mockTaskStore.EXPECT().UpdateTaskRuntime(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil),

		// Mock OfferOperation call.
		mockHostMgr.EXPECT().
			OfferOperations(
				gomock.Any(),
				gomock.Any()).
			Do(func(_ context.Context, reqBody interface{}) {
				// No need to unmarksnal output: empty means success.
				// Capture call since we don't know ordering of tasks.
				lock.Lock()
				defer lock.Unlock()
				req := reqBody.(*hostsvc.OfferOperationsRequest)
				hostsLaunchedOn[req.Hostname] = true
				assert.Equal(t, 1, len(req.Operations))
			}).
			Return(&hostsvc.OfferOperationsResponse{}, nil).
			Times(1),
	)

	err := taskLauncher.LaunchTaskWithReservedResource(context.Background(), testTask)

	assert.NoError(t, err)
	expectedLaunchedHosts := map[string]bool{
		"hostname-0": true,
	}
	lock.Lock()
	defer lock.Unlock()
	assert.Equal(t, expectedLaunchedHosts, hostsLaunchedOn)
}

func TestLaunchStatefulTaskLaunchWithReservedResourceWithDBReadErr(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockHostMgr := host_mocks.NewMockInternalHostServiceYARPCClient(ctrl)
	mockTaskStore := store_mocks.NewMockTaskStore(ctrl)
	mockVolumeStore := store_mocks.NewMockPersistentVolumeStore(ctrl)

	testScope := tally.NewTestScope("", map[string]string{})
	metrics := NewMetrics(testScope)
	taskLauncher := launcher{
		hostMgrClient: mockHostMgr,
		taskStore:     mockTaskStore,
		volumeStore:   mockVolumeStore,
		metrics:       metrics,
		retryPolicy:   backoff.NewRetryPolicy(5, 15*time.Millisecond),
	}

	numTasks := 1
	testTasks := make([]*task.TaskInfo, numTasks)
	taskIDs := make([]*peloton.TaskID, numTasks)
	placements := make([]*resmgr.Placement, numTasks)
	taskConfigs := make(map[string]*task.TaskConfig)
	taskIds := make(map[string]*peloton.TaskID)
	var testTask *task.TaskInfo
	for i := 0; i < numTasks; i++ {
		tmp := createTestTask(i)
		tmp.GetConfig().Volume = &task.PersistentVolumeConfig{
			ContainerPath: "testpath",
			SizeMB:        10,
		}
		tmp.GetRuntime().Host = "hostname-0"
		testTask = tmp
		taskID := &peloton.TaskID{
			Value: tmp.JobId.Value + "-" + fmt.Sprint(tmp.InstanceId),
		}
		taskIDs[i] = taskID
		testTasks[i] = tmp
		taskConfigs[tmp.GetRuntime().GetMesosTaskId().GetValue()] = tmp.Config
		taskIds[taskID.Value] = taskID
	}

	// generate 1 host offer, each can hold 1 tasks.
	numHostOffers := 1
	rs := createResources(1)
	var hostOffers []*hostsvc.HostOffer
	for i := 0; i < numHostOffers; i++ {
		hostOffers = append(hostOffers, createHostOffer(i, rs))
	}

	// Generate Placements per host offer
	for i := 0; i < numHostOffers; i++ {
		p := createPlacements(testTasks[i], hostOffers[i])
		p.Type = resmgr.TaskType_STATEFUL
		placements[i] = p
	}

	gomock.InOrder(
		// Mock Task Store GetTaskByID
		mockTaskStore.EXPECT().GetTaskByID(gomock.Any(), taskIDs[0].Value).
			Return(testTasks[0], fmt.Errorf("fake db read error")),
	)

	err := taskLauncher.LaunchTaskWithReservedResource(context.Background(), testTask)
	assert.Equal(t, err, errEmptyTasks)
}

// createPlacements creates the placement
func createPlacements(t *task.TaskInfo,
	hostOffer *hostsvc.HostOffer) *resmgr.Placement {
	TasksIds := make([]*peloton.TaskID, 1)

	taskID := &peloton.TaskID{
		Value: t.JobId.Value + "-" + fmt.Sprint(t.InstanceId),
	}
	TasksIds[0] = taskID
	placement := &resmgr.Placement{
		AgentId:  hostOffer.AgentId,
		Hostname: hostOffer.Hostname,
		Tasks:    TasksIds,
		Ports:    []uint32{testPort},
	}

	return placement
}
