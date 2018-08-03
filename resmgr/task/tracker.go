package task

import (
	"context"
	"sync"
	"time"

	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/peloton"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/task"
	"code.uber.internal/infra/peloton/.gen/peloton/private/hostmgr/hostsvc"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgr"

	"code.uber.internal/infra/peloton/common/eventstream"
	"code.uber.internal/infra/peloton/resmgr/respool"
	"code.uber.internal/infra/peloton/resmgr/scalar"
	"code.uber.internal/infra/peloton/util"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/uber-go/tally"
)

// Tracker is the interface for resource manager to
// track all the tasks
type Tracker interface {

	// AddTask adds the task to state machine
	AddTask(
		t *resmgr.Task,
		handler *eventstream.Handler,
		respool respool.ResPool,
		config *Config) error

	// GetTask gets the RM task for taskID
	GetTask(t *peloton.TaskID) *RMTask

	// Sets the hostname where the task is placed.
	SetPlacement(t *peloton.TaskID, hostname string)

	// SetPlacementHost Sets the hostname for the placement
	SetPlacementHost(placement *resmgr.Placement, hostname string)

	// DeleteTask deletes the task from the map
	DeleteTask(t *peloton.TaskID)

	// MarkItDone marks the task done and add back those
	// resources to respool
	MarkItDone(taskID *peloton.TaskID, mesosTaskID string) error

	// MarkItInvalid marks the task done and invalidate them
	// in to respool by that they can be removed from the queue
	MarkItInvalid(taskID *peloton.TaskID, mesosTaskID string) error

	// TasksByHosts returns all tasks of the given type running on the given hosts.
	TasksByHosts(hosts []string, taskType resmgr.TaskType) map[string][]*RMTask

	// AddResources adds the task resources to respool
	AddResources(taskID *peloton.TaskID) error

	// GetSize returns the number of the tasks in tracker
	GetSize() int64

	// Clear cleans the tracker with all the tasks
	Clear()

	// GetActiveTasks returns task states map
	GetActiveTasks(jobID string, respoolID string, states []string) map[string][]*RMTask

	// UpdateCounters updates the counters for each state
	UpdateCounters(from task.TaskState, to task.TaskState)
}

// tracker is the rmtask tracker
// map[taskid]*rmtask
type tracker struct {
	lock sync.RWMutex

	// Map of peloton task ID to the resource manager task
	tasks map[string]*RMTask

	// Maps hostname -> task type -> task id -> rm task
	placements map[string]map[resmgr.TaskType]map[string]*RMTask

	parentScope tally.Scope
	metrics     *Metrics

	// mutex for the state counters
	cMutex sync.Mutex
	// map of task state to the count of tasks in the tracker
	counters map[task.TaskState]float64

	// host manager client
	hostMgrClient hostsvc.InternalHostServiceYARPCClient
}

// singleton object
var rmtracker *tracker

// InitTaskTracker initialize the task tracker
func InitTaskTracker(parent tally.Scope, config *Config, hostMgrClient hostsvc.InternalHostServiceYARPCClient) {
	if rmtracker != nil {
		log.Info("Resource Manager Tracker is already initialized")
		return
	}
	rmtracker = &tracker{
		tasks:         make(map[string]*RMTask),
		placements:    map[string]map[resmgr.TaskType]map[string]*RMTask{},
		metrics:       NewMetrics(parent.SubScope("tracker")),
		parentScope:   parent,
		counters:      make(map[task.TaskState]float64),
		hostMgrClient: hostMgrClient,
	}

	// Checking placement back off is enabled , if yes then initialize
	// policy factory. Explicitly checking, anything related to
	// back off policies should come inside this code path.
	if config.EnablePlacementBackoff {
		err := InitPolicyFactory()
		if err != nil {
			log.Error("Error initializing back off policy")
		}
	}
	log.Info("Resource Manager Tracker is initialized")
}

// GetTracker gets the singleton object of the tracker
func GetTracker() Tracker {
	if rmtracker == nil {
		log.Fatal("Tracker is not initialized")
	}
	return rmtracker
}

// AddTask adds task to resmgr task tracker
func (tr *tracker) AddTask(
	t *resmgr.Task,
	handler *eventstream.Handler,
	respool respool.ResPool,
	config *Config) error {

	rmTask, err := CreateRMTask(t, handler, respool,
		NewTransitionObserver(WithTallyRecorder(tr.parentScope)), config)
	if err != nil {
		return err
	}

	tr.lock.Lock()
	defer tr.lock.Unlock()

	tr.tasks[rmTask.task.Id.Value] = rmTask
	if rmTask.task.Hostname != "" {
		tr.setPlacement(rmTask.task.Id, rmTask.task.Hostname)
	}
	tr.metrics.TasksCountInTracker.Update(float64(tr.GetSize()))
	return nil
}

// GetTask gets the RM task for taskID
// this locks the tracker and get the Task
func (tr *tracker) GetTask(t *peloton.TaskID) *RMTask {
	tr.lock.RLock()
	defer tr.lock.RUnlock()
	return tr.getTask(t)
}

// getTask gets the RM task for taskID
// this method is not protected, we need to lock tracker
// before we use this
func (tr *tracker) getTask(t *peloton.TaskID) *RMTask {
	if rmTask, ok := tr.tasks[t.Value]; ok {
		return rmTask
	}
	return nil
}

func (tr *tracker) setPlacement(t *peloton.TaskID, hostname string) {
	rmTask, ok := tr.tasks[t.Value]
	if !ok {
		return
	}
	tr.clearPlacement(rmTask)
	rmTask.task.Hostname = hostname
	if _, exists := tr.placements[hostname]; !exists {
		tr.placements[hostname] = map[resmgr.TaskType]map[string]*RMTask{}
	}
	if _, exists := tr.placements[hostname][rmTask.task.Type]; !exists {
		tr.placements[hostname][rmTask.task.Type] = map[string]*RMTask{}
	}
	if _, exists := tr.placements[hostname][rmTask.task.Type][t.Value]; !exists {
		tr.placements[hostname][rmTask.task.Type][t.Value] = rmTask
	}
}

// clearPlacement will remove the task from the placements map.
func (tr *tracker) clearPlacement(rmTask *RMTask) {
	if rmTask.task.Hostname == "" {
		return
	}
	delete(tr.placements[rmTask.task.Hostname][rmTask.task.Type], rmTask.task.Id.Value)
	if len(tr.placements[rmTask.task.Hostname][rmTask.task.Type]) == 0 {
		delete(tr.placements[rmTask.task.Hostname], rmTask.task.Type)
	}
	if len(tr.placements[rmTask.task.Hostname]) == 0 {
		delete(tr.placements, rmTask.task.Hostname)
		log.WithField("host", rmTask.task.Hostname).Debug("No tasks on host")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := tr.hostMgrClient.MarkHostDrained(ctx, &hostsvc.MarkHostDrainedRequest{
			Hostname: rmTask.task.Hostname,
		})
		if err != nil {
			log.Warnf(err.Error(), "Failed to 'down' some hosts")
		}
	}
}

// SetPlacement will set the hostname that the task is currently placed on.
func (tr *tracker) SetPlacement(t *peloton.TaskID, hostname string) {
	tr.lock.Lock()
	defer tr.lock.Unlock()
	tr.setPlacement(t, hostname)
}

// SetPlacementHost will set the hostname that the task is currently placed on.
func (tr *tracker) SetPlacementHost(placement *resmgr.Placement, hostname string) {
	tr.lock.Lock()
	defer tr.lock.Unlock()
	for _, t := range placement.GetTasks() {
		tr.setPlacement(t, hostname)
	}
}

// DeleteTask deletes the task from the map after
// locking the tracker , this is interface call
func (tr *tracker) DeleteTask(t *peloton.TaskID) {
	tr.lock.Lock()
	defer tr.lock.Unlock()
	tr.deleteTask(t)
}

// deleteTask deletes the task from the map
// this method is not protected, we need to lock tracker
// before we use this.
func (tr *tracker) deleteTask(t *peloton.TaskID) {
	if rmTask, exists := tr.tasks[t.Value]; exists {
		tr.clearPlacement(rmTask)
	}
	delete(tr.tasks, t.Value)
	tr.metrics.TasksCountInTracker.Update(float64(tr.GetSize()))
}

// MarkItDone updates the resources in resmgr and removes the task
// from the tracker
func (tr *tracker) MarkItDone(
	tID *peloton.TaskID,
	mesosTaskID string) error {
	tr.lock.Lock()
	defer tr.lock.Unlock()

	t := tr.getTask(tID)
	if t == nil {
		return errors.Errorf("task %s is not in tracker", tID)
	}
	return tr.markItDone(t, mesosTaskID)
}

// MarkItInvalid marks the task done and invalidate them
// in to respool by that they can be removed from the queue
func (tr *tracker) MarkItInvalid(tID *peloton.TaskID, mesosTaskID string) error {
	tr.lock.Lock()
	defer tr.lock.Unlock()

	t := tr.getTask(tID)
	if t == nil {
		return errors.Errorf("task %s is not in tracker", tID)
	}

	// remove the tracker
	err := tr.markItDone(t, mesosTaskID)
	if err != nil {
		return err
	}

	// We only need to invalidate tasks if they are in PENDING or
	// INITIALIZED STATE as Pending queue only will have these tasks
	if t.GetCurrentState() == task.TaskState_PENDING ||
		t.GetCurrentState() == task.TaskState_INITIALIZED {
		t.respool.AddInvalidTask(tID)
	}

	return nil
}

// tracker needs to be locked before calling this.
func (tr *tracker) markItDone(t *RMTask, mesosTaskID string) error {
	// Checking mesos ID again if thats not changed
	tID := t.Task().GetId()

	// Checking mesos ID again if that has not changed
	if *t.Task().TaskId.Value != mesosTaskID {
		return errors.Errorf("for task %s: mesos id %s in tracker is different id %s from event",
			tID.Value, *t.Task().TaskId.Value, mesosTaskID)
	}

	// We need to skip the tasks from resource counting which are in pending and
	// and initialized state
	if !(t.GetCurrentState().String() == task.TaskState_PENDING.String() ||
		t.GetCurrentState().String() == task.TaskState_INITIALIZED.String()) {
		err := t.respool.SubtractFromAllocation(scalar.GetTaskAllocation(t.Task()))
		if err != nil {
			return errors.Errorf("failed update task %s ", tID)
		}
	}

	// stop the state machine
	t.StateMachine().Terminate()

	log.WithField("task_id", tID.Value).Info("Deleting the task from Tracker")
	tr.deleteTask(tID)
	return nil
}

// TasksByHosts returns all tasks of the given type running on the given hosts.
func (tr *tracker) TasksByHosts(hosts []string, taskType resmgr.TaskType) map[string][]*RMTask {
	result := map[string][]*RMTask{}
	var types []resmgr.TaskType
	if taskType == resmgr.TaskType_UNKNOWN {
		for t := range resmgr.TaskType_name {
			types = append(types, resmgr.TaskType(t))
		}
	} else {
		types = append(types, taskType)
	}
	for _, hostname := range hosts {
		for _, tType := range types {
			for _, rmTask := range tr.placements[hostname][tType] {
				result[hostname] = append(result[hostname], rmTask)
			}
		}
	}
	return result
}

// AddResources adds the task resources to respool
func (tr *tracker) AddResources(
	tID *peloton.TaskID) error {
	rmTask := tr.GetTask(tID)
	if rmTask == nil {
		return errors.Errorf("rmTask %s is not in tracker", tID)
	}
	res := scalar.ConvertToResmgrResource(rmTask.Task().GetResource())
	err := rmTask.respool.AddToAllocation(scalar.GetTaskAllocation(rmTask.Task()))
	if err != nil {
		return errors.Errorf("Not able to add resources for "+
			"rmTask %s for respool %s ", tID, rmTask.respool.Name())
	}
	log.WithFields(log.Fields{
		"respool_id": rmTask.respool.ID(),
		"resources":  res,
	}).Debug("Added resources to Respool")
	return nil
}

// GetSize gets the number of tasks in tracker
func (tr *tracker) GetSize() int64 {
	return int64(len(tr.tasks))
}

// Clear cleans the tracker with all the existing tasks
func (tr *tracker) Clear() {
	tr.lock.Lock()
	defer tr.lock.Unlock()

	// Cleaning the tasks
	for k := range tr.tasks {
		delete(tr.tasks, k)
	}
	// Cleaning the placements
	for k := range tr.placements {
		delete(tr.placements, k)
	}
}

// GetActiveTasks returns task to states map, if jobID or respoolID is provided,
// only tasks for that job or respool will be returned
func (tr *tracker) GetActiveTasks(
	jobID string,
	respoolID string,
	states []string) map[string][]*RMTask {
	taskStates := make(map[string][]*RMTask)

	tr.lock.RLock()
	defer tr.lock.RUnlock()

	for _, t := range tr.tasks {
		// filter by jobID
		if jobID != "" && t.Task().GetJobId().GetValue() != jobID {
			continue
		}

		// filter by resource pool ID
		if respoolID != "" && t.Respool().ID() != respoolID {
			continue
		}

		taskState := t.GetCurrentState().String()
		// filter by task states
		if len(states) > 0 && !util.Contains(states, taskState) {
			continue
		}

		taskStates[taskState] = append(taskStates[taskState], t)
	}
	return taskStates
}

// UpdateCounters updates the counters for each state. This can be called from
// multiple goroutines.
func (tr *tracker) UpdateCounters(from task.TaskState, to task.TaskState) {
	tr.cMutex.Lock()
	defer tr.cMutex.Unlock()

	// Reducing the count from state
	if val, ok := tr.counters[from]; ok {
		if val > 0 {
			tr.counters[from] = val - 1
		}
	}

	// Incrementing the state counter to +1
	if val, ok := tr.counters[to]; ok {
		tr.counters[to] = val + 1
	} else {
		tr.counters[to] = 1
	}

	// publishing all the counters
	for state, gauge := range tr.metrics.TaskStatesGauge {
		gauge.Update(tr.counters[state])
	}
}
