package placement

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/uber-go/tally"

	"go.uber.org/yarpc"

	mesosv1 "code.uber.internal/infra/peloton/.gen/mesos/v1"
	"code.uber.internal/infra/peloton/.gen/peloton/api/peloton"
	"code.uber.internal/infra/peloton/.gen/peloton/api/task"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgr"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc"

	"code.uber.internal/infra/peloton/common"
	"code.uber.internal/infra/peloton/jobmgr/cached"
	"code.uber.internal/infra/peloton/jobmgr/goalstate"
	"code.uber.internal/infra/peloton/jobmgr/task/launcher"
	"code.uber.internal/infra/peloton/util"
)

// Config is placement processor specific config
type Config struct {
	// PlacementDequeueLimit is the limit which placement processor get the
	// placements
	PlacementDequeueLimit int `yaml:"placement_dequeue_limit"`

	// GetPlacementsTimeout is the timeout value for placement processor to
	// call GetPlacements
	GetPlacementsTimeout int `yaml:"get_placements_timeout_ms"`
}

// Processor defines the interface of placement processor
// which dequeues placed tasks and sends them to host manager via task launcher
type Processor interface {
	// Start starts the placement processor goroutines
	Start() error
	// Stop stops the placement processor goroutines
	Stop() error
}

// launcher implements the Launcher interface
type processor struct {
	resMgrClient    resmgrsvc.ResourceManagerServiceYARPCClient
	jobFactory      cached.JobFactory
	goalStateDriver goalstate.Driver
	taskLauncher    launcher.Launcher
	running         int32
	config          *Config
	metrics         *Metrics
}

const (
	// Time out for the function to time out
	_rpcTimeout = 10 * time.Second
	// maxRetryCount is the maximum number of times a transient error from the DB will be retried.
	// This is a safety mechanism to avoid placement engine getting stuck in
	// retrying errors wrongly classified as transient errors.
	maxRetryCount = 1000
)

// InitProcessor initializes placement processor
func InitProcessor(
	d *yarpc.Dispatcher,
	resMgrClientName string,
	jobFactory cached.JobFactory,
	goalStateDriver goalstate.Driver,
	taskLauncher launcher.Launcher,
	config *Config,
	parent tally.Scope,
) Processor {
	return &processor{
		resMgrClient:    resmgrsvc.NewResourceManagerServiceYARPCClient(d.ClientConfig(resMgrClientName)),
		jobFactory:      jobFactory,
		goalStateDriver: goalStateDriver,
		taskLauncher:    taskLauncher,
		config:          config,
		metrics:         NewMetrics(parent.SubScope("jobmgr").SubScope("task")),
	}
}

// Start starts Processor
func (p *processor) Start() error {
	if p.isRunning() {
		// already running
		log.Warn("placement processor is already running, no action will be performed")
		return nil
	}

	log.Info("starting placement processor")
	atomic.StoreInt32(&p.running, 1)
	go func() {
		for p.isRunning() {
			placements, err := p.getPlacements()
			if err != nil {
				log.WithError(err).Error("jobmgr failed to dequeue placements")
				continue
			}

			if !p.isRunning() {
				// placement dequeued but not processed
				log.WithField("placements", placements).
					Warn("ignoring placement after dequeue due to lost leadership")
				break
			}

			if len(placements) == 0 {
				// log a debug to make it not verbose
				log.Debug("No placements")
				continue
			}

			ctx := context.Background()

			// Getting and launching placements in different go routine
			log.WithField("placements", placements).Debug("Start processing placements")
			for _, placement := range placements {
				go p.ProcessPlacement(ctx, placement)
			}
		}
	}()
	log.Info("placement processor started")
	return nil
}

func (p *processor) ProcessPlacement(ctx context.Context, placement *resmgr.Placement) {
	tasks := placement.GetTasks()
	taskInfos, err := p.taskLauncher.GetLaunchableTasks(
		ctx,
		placement.GetTasks(),
		placement.GetHostname(),
		placement.GetAgentId(),
		placement.GetPorts())
	if err != nil {
		err = p.taskLauncher.TryReturnOffers(ctx, err, placement)
		if err != nil {
			log.WithError(err).WithFields(log.Fields{
				"placement":   placement,
				"tasks_total": len(tasks),
			}).Error("Failed to get launchable tasks")
		}
		return
	}

	for taskID, taskInfo := range taskInfos {
		id, instanceID, err := util.ParseTaskID(taskID)
		if err != nil {
			continue
		}
		jobID := &peloton.JobID{Value: id}
		runtime := taskInfo.GetRuntime()
		cachedJob := p.jobFactory.AddJob(jobID)
		cachedTask := cachedJob.AddTask(uint32(instanceID))

		cachedRuntime, err := cachedTask.GetRunTime(ctx)
		if err != nil {
			log.WithError(err).
				WithField("task_id", taskID).
				Error("cannot fetch task runtime")
			continue
		}

		if cachedRuntime.GetGoalState() == task.TaskState_KILLED {
			if cachedRuntime.GetState() != task.TaskState_KILLED {
				// Received placement for task which needs to be killed, retry killing the task.
				p.goalStateDriver.EnqueueTask(jobID, uint32(instanceID), time.Now())
			}

			// Skip launching of deleted tasks.
			log.WithField("task_id", taskID).Info("skipping launch of killed task")
			delete(taskInfos, taskID)
		} else {
			retry := 0
			for retry < maxRetryCount {
				err = cachedJob.UpdateTasks(ctx, map[uint32]*task.RuntimeInfo{uint32(instanceID): runtime}, cached.UpdateCacheAndDB)
				if err == nil {
					taskInfo.Runtime, _ = cachedTask.GetRunTime(ctx)
					break
				}
				if common.IsTransientError(err) {
					// TBD add a max retry to bail out after a few retries.
					log.WithError(err).WithFields(log.Fields{
						"job_id":      id,
						"instance_id": instanceID,
					}).Warn("retrying update task runtime on transient error")
				} else {
					log.WithError(err).WithFields(log.Fields{
						"job_id":      id,
						"instance_id": instanceID,
					}).Error("cannot process placement due to non-transient db error")
					delete(taskInfos, taskID)
					break
				}
				retry++
			}
		}
	}

	if len(taskInfos) == 0 {
		// nothing to launch
		return
	}

	launchableTasks := launcher.CreateLaunchableTasks(taskInfos)
	if err = p.taskLauncher.ProcessPlacement(ctx, launchableTasks, placement); err != nil {
		if err = p.enqueueTasks(ctx, taskInfos); err != nil {
			var taskIDs []string
			for taskID := range taskInfos {
				taskIDs = append(taskIDs, taskID)
			}
			log.WithError(err).WithFields(log.Fields{
				"task_ids":    taskIDs,
				"tasks_total": len(taskInfos),
			}).Error("failed to enqueue tasks back to resmgr")
		}
		return
	}

	// Finally, enqueue tasks into goalstate
	for id := range taskInfos {
		jobID, instanceID, err := util.ParseTaskID(id)
		if err != nil {
			log.WithError(err).
				WithField("task_id", id).
				Error("failed to parse the task id in placement processor")
			continue
		}

		p.goalStateDriver.EnqueueTask(
			&peloton.JobID{Value: jobID},
			uint32(instanceID),
			time.Now())

		cachedJob := p.jobFactory.AddJob(&peloton.JobID{Value: jobID})
		goalstate.EnqueueJobWithDefaultDelay(
			&peloton.JobID{Value: jobID},
			p.goalStateDriver,
			cachedJob)
	}
}

func (p *processor) getPlacements() ([]*resmgr.Placement, error) {
	ctx, cancelFunc := context.WithTimeout(context.Background(), _rpcTimeout)
	defer cancelFunc()

	request := &resmgrsvc.GetPlacementsRequest{
		Limit:   uint32(p.config.PlacementDequeueLimit),
		Timeout: uint32(p.config.GetPlacementsTimeout),
	}

	callStart := time.Now()
	response, err := p.resMgrClient.GetPlacements(ctx, request)
	callDuration := time.Since(callStart)

	if err != nil {
		p.metrics.GetPlacementFail.Inc(1)
		return nil, err
	}

	if response.GetError() != nil {
		p.metrics.GetPlacementFail.Inc(1)
		return nil, errors.New(response.GetError().String())
	}

	if len(response.GetPlacements()) != 0 {
		log.WithFields(log.Fields{
			"num_placements": len(response.Placements),
			"duration":       callDuration.Seconds(),
		}).Info("GetPlacements")
	}

	// TODO: turn getplacement metric into gauge so we can
	//       get the current get_placements counts
	p.metrics.GetPlacement.Inc(int64(len(response.GetPlacements())))
	p.metrics.GetPlacementsCallDuration.Record(callDuration)
	return response.GetPlacements(), nil
}

// enqueueTask enqueues given task to resmgr to launch again.
func (p *processor) enqueueTasks(ctx context.Context, tasks map[string]*task.TaskInfo) error {
	if len(tasks) == 0 {
		return nil
	}

	var err error
	for _, t := range tasks {
		runtime := util.RegenerateMesosTaskID(t.JobId, t.InstanceId, t.GetRuntime().GetMesosTaskId())
		runtime.Message = "Regenerate placement"
		runtime.Reason = "REASON_HOST_REJECT_OFFER"
		runtime.AgentID = &mesosv1.AgentID{}
		runtime.Ports = make(map[string]uint32)
		retry := 0
		for retry < maxRetryCount {
			cachedJob := p.jobFactory.AddJob(t.JobId)
			err = cachedJob.UpdateTasks(
				ctx,
				map[uint32]*task.RuntimeInfo{uint32(t.InstanceId): runtime},
				cached.UpdateCacheAndDB,
			)
			if err == nil {
				p.goalStateDriver.EnqueueTask(t.JobId, t.InstanceId, time.Now())
				goalstate.EnqueueJobWithDefaultDelay(
					t.JobId, p.goalStateDriver, cachedJob)
				break
			}
			if common.IsTransientError(err) {
				log.WithError(err).WithFields(log.Fields{
					"job_id":      t.JobId,
					"instance_id": t.InstanceId,
				}).Warn("retrying update task runtime on transient error")
			} else {
				return err
			}
			retry++
		}
	}
	return err
}

func (p *processor) isRunning() bool {
	running := atomic.LoadInt32(&p.running)
	return running == 1
}

// Stop stops placement processor
func (p *processor) Stop() error {
	if !(p.isRunning()) {
		log.Warn("placement processor is already stopped, no action will be performed")
		return nil
	}

	atomic.StoreInt32(&p.running, 0)
	log.Info("placement processor stopped")
	return nil
}
