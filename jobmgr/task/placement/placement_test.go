package placement

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/pborman/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/uber-go/tally"

	mesos "code.uber.internal/infra/peloton/.gen/mesos/v1"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/job"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/peloton"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/task"
	"code.uber.internal/infra/peloton/.gen/peloton/private/hostmgr/hostsvc"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgr"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc"
	res_mocks "code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc/mocks"

	"code.uber.internal/infra/peloton/jobmgr/cached"
	cachedmocks "code.uber.internal/infra/peloton/jobmgr/cached/mocks"
	goalstatemocks "code.uber.internal/infra/peloton/jobmgr/goalstate/mocks"
	launcher_mocks "code.uber.internal/infra/peloton/jobmgr/task/launcher/mocks"
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
		SLA:           _sla,
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
			State:     task.TaskState_PENDING,
			GoalState: task.TaskState_SUCCEEDED,
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
func TestMultipleTasksPlacements(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRes := res_mocks.NewMockResourceManagerServiceYARPCClient(ctrl)
	testScope := tally.NewTestScope("", map[string]string{})
	metrics := NewMetrics(testScope)
	pp := processor{
		config: &Config{
			PlacementDequeueLimit: 100,
		},
		resMgrClient: mockRes,
		metrics:      metrics,
	}

	// generate 25 test tasks
	numTasks := 25
	testTasks := make([]*task.TaskInfo, numTasks)
	placements := make([]*resmgr.Placement, numTasks)
	for i := 0; i < numTasks; i++ {
		tmp := createTestTask(i)
		testTasks[i] = tmp
	}

	// generate 25 host offer, each can hold 1 tasks.
	numHostOffers := numTasks
	rs := createResources(float64(numHostOffers))
	var hostOffers []*hostsvc.HostOffer
	for i := 0; i < numHostOffers; i++ {
		hostOffers = append(hostOffers, createHostOffer(i, rs))
	}

	// Generate Placements per host offer
	for i := 0; i < numHostOffers; i++ {
		p := createPlacements(testTasks[i], hostOffers[i])
		placements[i] = p
	}

	gomock.InOrder(
		mockRes.EXPECT().
			GetPlacements(
				gomock.Any(),
				gomock.Any()).
			Return(&resmgrsvc.GetPlacementsResponse{Placements: placements}, nil),
	)

	gPlacements, err := pp.getPlacements()

	if err != nil {
		assert.Error(t, err)
	}
	assert.Equal(t, placements, gPlacements)
}

// This test ensures placement engine, one start can dequeue placements, and
// then call launcher to launch the placements.
func TestTaskPlacementNoError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRes := res_mocks.NewMockResourceManagerServiceYARPCClient(ctrl)
	mockTaskLauncher := launcher_mocks.NewMockLauncher(ctrl)
	jobFactory := cachedmocks.NewMockJobFactory(ctrl)
	cachedJob := cachedmocks.NewMockJob(ctrl)
	cachedTask := cachedmocks.NewMockTask(ctrl)
	goalStateDriver := goalstatemocks.NewMockDriver(ctrl)
	testScope := tally.NewTestScope("", map[string]string{})
	metrics := NewMetrics(testScope)

	pp := processor{
		config: &Config{
			PlacementDequeueLimit: 100,
		},
		resMgrClient:    mockRes,
		metrics:         metrics,
		taskLauncher:    mockTaskLauncher,
		jobFactory:      jobFactory,
		goalStateDriver: goalStateDriver,
	}

	testTask := createTestTask(0) // taskinfo
	rs := createResources(float64(1))
	hostOffer := createHostOffer(0, rs)
	p := createPlacements(testTask, hostOffer)

	taskID := &peloton.TaskID{
		Value: testTask.JobId.Value + "-" + fmt.Sprint(testTask.InstanceId),
	}
	taskInfo := make(map[string]*task.TaskInfo)
	taskInfo[taskID.Value] = testTask

	gomock.InOrder(
		mockTaskLauncher.EXPECT().
			GetLaunchableTasks(gomock.Any(), p.Tasks, p.Hostname, p.AgentId, p.Ports).Return(taskInfo, nil),
		jobFactory.EXPECT().
			AddJob(testTask.JobId).Return(cachedJob),
		cachedJob.EXPECT().
			AddTask(uint32(0)).Return(cachedTask),
		cachedTask.EXPECT().
			GetRunTime(gomock.Any()).Return(testTask.Runtime, nil),
		cachedJob.EXPECT().
			UpdateTasks(gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).Return(nil),
		cachedTask.EXPECT().
			GetRunTime(gomock.Any()).Return(testTask.Runtime, nil),
		mockTaskLauncher.EXPECT().
			CreateLaunchableTasks(gomock.Any(), gomock.Any()).Return(nil, nil),
		mockTaskLauncher.EXPECT().
			ProcessPlacement(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil),
		goalStateDriver.EXPECT().
			EnqueueTask(testTask.JobId, testTask.InstanceId, gomock.Any()).Return(),
		jobFactory.EXPECT().
			AddJob(testTask.JobId).Return(cachedJob),
		cachedJob.EXPECT().GetJobType().Return(job.JobType_BATCH),
		goalStateDriver.EXPECT().
			JobRuntimeDuration(job.JobType_BATCH).
			Return(1*time.Second),
		goalStateDriver.EXPECT().
			EnqueueJob(testTask.JobId, gomock.Any()).Return(),
	)

	pp.ProcessPlacement(context.Background(), p)
}

func TestTaskPlacementGetTaskError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRes := res_mocks.NewMockResourceManagerServiceYARPCClient(ctrl)
	mockTaskLauncher := launcher_mocks.NewMockLauncher(ctrl)
	testScope := tally.NewTestScope("", map[string]string{})
	metrics := NewMetrics(testScope)
	pp := processor{
		config: &Config{
			PlacementDequeueLimit: 100,
		},
		resMgrClient: mockRes,
		metrics:      metrics,
		taskLauncher: mockTaskLauncher,
	}

	testTask := createTestTask(0) // taskinfo
	rs := createResources(float64(1))
	hostOffer := createHostOffer(0, rs)
	p := createPlacements(testTask, hostOffer)

	taskID := &peloton.TaskID{
		Value: testTask.JobId.Value + "-" + fmt.Sprint(testTask.InstanceId),
	}
	taskInfo := make(map[string]*task.TaskInfo)
	taskInfo[taskID.Value] = testTask

	gomock.InOrder(
		mockTaskLauncher.EXPECT().
			GetLaunchableTasks(gomock.Any(), p.Tasks, p.Hostname, p.AgentId, p.Ports).Return(taskInfo, fmt.Errorf("fake launch error")),
		mockTaskLauncher.EXPECT().
			TryReturnOffers(gomock.Any(), gomock.Any(), p).Return(nil),
	)

	pp.ProcessPlacement(context.Background(), p)
}

func TestTaskPlacementKilledTask(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRes := res_mocks.NewMockResourceManagerServiceYARPCClient(ctrl)
	mockTaskLauncher := launcher_mocks.NewMockLauncher(ctrl)
	jobFactory := cachedmocks.NewMockJobFactory(ctrl)
	cachedJob := cachedmocks.NewMockJob(ctrl)
	cachedTask := cachedmocks.NewMockTask(ctrl)
	goalStateDriver := goalstatemocks.NewMockDriver(ctrl)
	testScope := tally.NewTestScope("", map[string]string{})
	metrics := NewMetrics(testScope)

	pp := processor{
		config: &Config{
			PlacementDequeueLimit: 100,
		},
		resMgrClient:    mockRes,
		metrics:         metrics,
		taskLauncher:    mockTaskLauncher,
		jobFactory:      jobFactory,
		goalStateDriver: goalStateDriver,
	}

	testTask := createTestTask(0) // taskinfo
	rs := createResources(float64(1))
	hostOffer := createHostOffer(0, rs)
	p := createPlacements(testTask, hostOffer)
	testTask.Runtime.GoalState = task.TaskState_KILLED
	testTask.Runtime.State = task.TaskState_KILLED

	taskID := &peloton.TaskID{
		Value: testTask.JobId.Value + "-" + fmt.Sprint(testTask.InstanceId),
	}
	taskInfo := make(map[string]*task.TaskInfo)
	taskInfo[taskID.Value] = testTask

	gomock.InOrder(
		mockTaskLauncher.EXPECT().
			GetLaunchableTasks(gomock.Any(), p.Tasks, p.Hostname, p.AgentId, p.Ports).Return(taskInfo, nil),
		jobFactory.EXPECT().
			AddJob(testTask.JobId).Return(cachedJob),
		cachedJob.EXPECT().
			AddTask(uint32(0)).Return(cachedTask),
		cachedTask.EXPECT().
			GetRunTime(gomock.Any()).Return(testTask.Runtime, nil),
	)

	pp.ProcessPlacement(context.Background(), p)
}

func TestTaskPlacementKilledRunningTask(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRes := res_mocks.NewMockResourceManagerServiceYARPCClient(ctrl)
	mockTaskLauncher := launcher_mocks.NewMockLauncher(ctrl)
	jobFactory := cachedmocks.NewMockJobFactory(ctrl)
	cachedJob := cachedmocks.NewMockJob(ctrl)
	cachedTask := cachedmocks.NewMockTask(ctrl)
	goalStateDriver := goalstatemocks.NewMockDriver(ctrl)
	testScope := tally.NewTestScope("", map[string]string{})
	metrics := NewMetrics(testScope)

	pp := processor{
		config: &Config{
			PlacementDequeueLimit: 100,
		},
		resMgrClient:    mockRes,
		metrics:         metrics,
		taskLauncher:    mockTaskLauncher,
		jobFactory:      jobFactory,
		goalStateDriver: goalStateDriver,
	}

	testTask := createTestTask(0) // taskinfo
	rs := createResources(float64(1))
	hostOffer := createHostOffer(0, rs)
	p := createPlacements(testTask, hostOffer)
	testTask.Runtime.GoalState = task.TaskState_KILLED

	taskID := &peloton.TaskID{
		Value: testTask.JobId.Value + "-" + fmt.Sprint(testTask.InstanceId),
	}
	taskInfo := make(map[string]*task.TaskInfo)
	taskInfo[taskID.Value] = testTask

	expectedRuntime := make(map[uint32]*task.RuntimeInfo)
	expectedRuntime[testTask.InstanceId] = testTask.Runtime

	gomock.InOrder(
		mockTaskLauncher.EXPECT().
			GetLaunchableTasks(gomock.Any(), p.Tasks, p.Hostname, p.AgentId, p.Ports).Return(taskInfo, nil),
		jobFactory.EXPECT().
			AddJob(testTask.JobId).Return(cachedJob),
		cachedJob.EXPECT().
			AddTask(uint32(0)).Return(cachedTask),
		cachedTask.EXPECT().
			GetRunTime(gomock.Any()).Return(testTask.Runtime, nil),
		goalStateDriver.EXPECT().
			EnqueueTask(testTask.JobId, gomock.Any(), gomock.Any()).Return(),
	)

	pp.ProcessPlacement(context.Background(), p)
}

func TestTaskPlacementDBError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRes := res_mocks.NewMockResourceManagerServiceYARPCClient(ctrl)
	mockTaskLauncher := launcher_mocks.NewMockLauncher(ctrl)
	jobFactory := cachedmocks.NewMockJobFactory(ctrl)
	cachedJob := cachedmocks.NewMockJob(ctrl)
	cachedTask := cachedmocks.NewMockTask(ctrl)
	goalStateDriver := goalstatemocks.NewMockDriver(ctrl)
	testScope := tally.NewTestScope("", map[string]string{})
	metrics := NewMetrics(testScope)

	pp := processor{
		config: &Config{
			PlacementDequeueLimit: 100,
		},
		resMgrClient:    mockRes,
		metrics:         metrics,
		taskLauncher:    mockTaskLauncher,
		jobFactory:      jobFactory,
		goalStateDriver: goalStateDriver,
	}

	testTask := createTestTask(0) // taskinfo
	rs := createResources(float64(1))
	hostOffer := createHostOffer(0, rs)
	p := createPlacements(testTask, hostOffer)

	taskID := &peloton.TaskID{
		Value: testTask.JobId.Value + "-" + fmt.Sprint(testTask.InstanceId),
	}
	taskInfo := make(map[string]*task.TaskInfo)
	taskInfo[taskID.Value] = testTask

	gomock.InOrder(
		mockTaskLauncher.EXPECT().
			GetLaunchableTasks(gomock.Any(), p.Tasks, p.Hostname, p.AgentId, p.Ports).Return(taskInfo, nil),
		jobFactory.EXPECT().
			AddJob(testTask.JobId).Return(cachedJob),
		cachedJob.EXPECT().
			AddTask(uint32(0)).Return(cachedTask),
		cachedTask.EXPECT().
			GetRunTime(gomock.Any()).Return(testTask.Runtime, nil),
		cachedJob.EXPECT().
			UpdateTasks(gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).Return(fmt.Errorf("fake db error")),
	)

	pp.ProcessPlacement(context.Background(), p)
}

func TestTaskPlacementError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRes := res_mocks.NewMockResourceManagerServiceYARPCClient(ctrl)
	mockTaskLauncher := launcher_mocks.NewMockLauncher(ctrl)
	jobFactory := cachedmocks.NewMockJobFactory(ctrl)
	cachedJob := cachedmocks.NewMockJob(ctrl)
	cachedTask := cachedmocks.NewMockTask(ctrl)
	goalStateDriver := goalstatemocks.NewMockDriver(ctrl)
	testScope := tally.NewTestScope("", map[string]string{})
	metrics := NewMetrics(testScope)

	pp := processor{
		config: &Config{
			PlacementDequeueLimit: 100,
		},
		resMgrClient:    mockRes,
		metrics:         metrics,
		taskLauncher:    mockTaskLauncher,
		jobFactory:      jobFactory,
		goalStateDriver: goalStateDriver,
	}

	testTask := createTestTask(0) // taskinfo
	rs := createResources(float64(1))
	hostOffer := createHostOffer(0, rs)
	p := createPlacements(testTask, hostOffer)

	taskID := &peloton.TaskID{
		Value: testTask.JobId.Value + "-" + fmt.Sprint(testTask.InstanceId),
	}
	taskInfo := make(map[string]*task.TaskInfo)
	taskInfo[taskID.Value] = testTask

	gomock.InOrder(
		mockTaskLauncher.EXPECT().
			GetLaunchableTasks(gomock.Any(), p.Tasks, p.Hostname, p.AgentId, p.Ports).Return(taskInfo, nil),
		jobFactory.EXPECT().
			AddJob(testTask.JobId).Return(cachedJob),
		cachedJob.EXPECT().
			AddTask(uint32(0)).Return(cachedTask),
		cachedTask.EXPECT().
			GetRunTime(gomock.Any()).Return(testTask.Runtime, nil),
		cachedJob.EXPECT().
			UpdateTasks(gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).Return(nil),
		cachedTask.EXPECT().
			GetRunTime(gomock.Any()).Return(testTask.Runtime, nil),
		mockTaskLauncher.EXPECT().
			CreateLaunchableTasks(gomock.Any(), gomock.Any()).Return(nil, nil),
		mockTaskLauncher.EXPECT().
			ProcessPlacement(gomock.Any(), gomock.Any(), gomock.Any()).Return(fmt.Errorf("fake launch error")),
		jobFactory.EXPECT().
			AddJob(testTask.JobId).Return(cachedJob),
		cachedJob.EXPECT().
			UpdateTasks(gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).Return(nil),
		goalStateDriver.EXPECT().
			EnqueueTask(testTask.JobId, testTask.InstanceId, gomock.Any()).Return(),
		cachedJob.EXPECT().GetJobType().Return(job.JobType_BATCH),
		goalStateDriver.EXPECT().
			JobRuntimeDuration(job.JobType_BATCH).
			Return(1*time.Second),
		goalStateDriver.EXPECT().
			EnqueueJob(testTask.JobId, gomock.Any()).Return(),
	)

	pp.ProcessPlacement(context.Background(), p)
}

func TestTaskPlacementPlacementResMgrError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRes := res_mocks.NewMockResourceManagerServiceYARPCClient(ctrl)
	mockTaskLauncher := launcher_mocks.NewMockLauncher(ctrl)
	jobFactory := cachedmocks.NewMockJobFactory(ctrl)
	cachedJob := cachedmocks.NewMockJob(ctrl)
	cachedTask := cachedmocks.NewMockTask(ctrl)
	goalStateDriver := goalstatemocks.NewMockDriver(ctrl)
	testScope := tally.NewTestScope("", map[string]string{})
	metrics := NewMetrics(testScope)

	pp := processor{
		config: &Config{
			PlacementDequeueLimit: 100,
		},
		resMgrClient:    mockRes,
		metrics:         metrics,
		taskLauncher:    mockTaskLauncher,
		jobFactory:      jobFactory,
		goalStateDriver: goalStateDriver,
	}

	testTask := createTestTask(0) // taskinfo
	rs := createResources(float64(1))
	hostOffer := createHostOffer(0, rs)
	p := createPlacements(testTask, hostOffer)

	taskID := &peloton.TaskID{
		Value: testTask.JobId.Value + "-" + fmt.Sprint(testTask.InstanceId),
	}
	taskInfo := make(map[string]*task.TaskInfo)
	taskInfo[taskID.Value] = testTask

	gomock.InOrder(
		mockTaskLauncher.EXPECT().
			GetLaunchableTasks(gomock.Any(), p.Tasks, p.Hostname, p.AgentId, p.Ports).Return(taskInfo, nil),
		jobFactory.EXPECT().
			AddJob(testTask.JobId).Return(cachedJob),
		cachedJob.EXPECT().
			AddTask(uint32(0)).Return(cachedTask),
		cachedTask.EXPECT().
			GetRunTime(gomock.Any()).Return(testTask.Runtime, nil),
		cachedJob.EXPECT().
			UpdateTasks(gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).Return(nil),
		cachedTask.EXPECT().
			GetRunTime(gomock.Any()).Return(testTask.Runtime, nil),
		mockTaskLauncher.EXPECT().
			CreateLaunchableTasks(gomock.Any(), gomock.Any()).Return(nil, nil),
		mockTaskLauncher.EXPECT().
			ProcessPlacement(gomock.Any(), gomock.Any(), gomock.Any()).Return(fmt.Errorf("fake launch error")),
		jobFactory.EXPECT().
			AddJob(testTask.JobId).Return(cachedJob),
		cachedJob.EXPECT().
			UpdateTasks(gomock.Any(), gomock.Any(), cached.UpdateCacheAndDB).Return(fmt.Errorf("fake db error")),
	)

	pp.ProcessPlacement(context.Background(), p)
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
