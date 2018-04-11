package recovery

import (
	"context"
	"fmt"
	"sync"
	"testing"

	pb_job "code.uber.internal/infra/peloton/.gen/peloton/api/job"
	"code.uber.internal/infra/peloton/.gen/peloton/api/peloton"
	pb_task "code.uber.internal/infra/peloton/.gen/peloton/api/task"

	"code.uber.internal/infra/peloton/storage/cassandra"
	store_mocks "code.uber.internal/infra/peloton/storage/mocks"

	"github.com/golang/mock/gomock"
	"github.com/pborman/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/uber-go/tally"
)

var (
	csStore              *cassandra.Store
	pendingJobID         *peloton.JobID
	runningJobID         *peloton.JobID
	receivedPendingJobID []string
)

var mutex = &sync.Mutex{}

func init() {
	conf := cassandra.MigrateForTest()
	var err error
	csStore, err = cassandra.NewStore(conf, tally.NoopScope)
	if err != nil {
		log.Fatal(err)
	}
}

func createJob(ctx context.Context, state pb_job.JobState, goalState pb_job.JobState) (*peloton.JobID, error) {
	var jobID = &peloton.JobID{Value: uuid.New()}
	var sla = pb_job.SlaConfig{
		Priority:                22,
		MaximumRunningInstances: 3,
		Preemptible:             false,
	}
	var taskConfig = pb_task.TaskConfig{
		Resource: &pb_task.ResourceConfig{
			CpuLimit:    0.8,
			MemLimitMb:  800,
			DiskLimitMb: 1500,
		},
	}
	var jobConfig = pb_job.JobConfig{
		Name:          "TestValidatorWithStore",
		OwningTeam:    "team6",
		LdapGroups:    []string{"money", "team6", "gsg9"},
		Sla:           &sla,
		DefaultConfig: &taskConfig,
		InstanceCount: 2,
	}

	err := csStore.CreateJob(ctx, jobID, &jobConfig, "gsg9")
	if err != nil {
		return nil, err
	}

	jobRuntime, err := csStore.GetJobRuntime(ctx, jobID)
	if err != nil {
		return nil, err
	}

	jobRuntime.State = state
	jobRuntime.GoalState = goalState
	err = csStore.UpdateJobRuntime(ctx, jobID, jobRuntime)
	if err != nil {
		return nil, err
	}

	return jobID, nil
}

func recoverPendingTask(ctx context.Context, jobID string, jobConfig *pb_job.JobConfig, jobRuntime *pb_job.RuntimeInfo, batch TasksBatch, errChan chan<- error) {
	var err error

	if jobID != pendingJobID.GetValue() {
		err = fmt.Errorf("Got the wrong job id")
	}
	errChan <- err
	return
}

func recoverRunningTask(ctx context.Context, jobID string, jobConfig *pb_job.JobConfig, jobRuntime *pb_job.RuntimeInfo, batch TasksBatch, errChan chan<- error) {
	var err error

	if jobID != runningJobID.GetValue() {
		err = fmt.Errorf("Got the wrong job id")
	}
	errChan <- err
	return
}

func recoverAllTask(ctx context.Context, jobID string, jobConfig *pb_job.JobConfig, jobRuntime *pb_job.RuntimeInfo, batch TasksBatch, errChan chan<- error) {
	mutex.Lock()
	receivedPendingJobID = append(receivedPendingJobID, jobID)
	mutex.Unlock()
	return
}

func TestJobRecoveryWithStore(t *testing.T) {
	var err error
	var jobStatesPending = []pb_job.JobState{
		pb_job.JobState_PENDING,
	}
	var jobStatesRunning = []pb_job.JobState{
		pb_job.JobState_RUNNING,
	}
	var jobStatesAll = []pb_job.JobState{
		pb_job.JobState_PENDING,
		pb_job.JobState_RUNNING,
		pb_job.JobState_FAILED,
	}

	ctx := context.Background()

	pendingJobID, err = createJob(ctx, pb_job.JobState_PENDING, pb_job.JobState_SUCCEEDED)
	assert.NoError(t, err)

	runningJobID, err = createJob(ctx, pb_job.JobState_RUNNING, pb_job.JobState_SUCCEEDED)
	assert.NoError(t, err)

	// this job should not be recovered
	_, err = createJob(ctx, pb_job.JobState_FAILED, pb_job.JobState_SUCCEEDED)
	assert.NoError(t, err)

	// this job should not be recovered
	_, err = createJob(ctx, pb_job.JobState_FAILED, pb_job.JobState_UNKNOWN)
	assert.NoError(t, err)

	err = RecoverJobsByState(ctx, csStore, jobStatesPending, recoverPendingTask)
	assert.NoError(t, err)
	err = RecoverJobsByState(ctx, csStore, jobStatesRunning, recoverRunningTask)
	assert.NoError(t, err)
	err = RecoverJobsByState(ctx, csStore, jobStatesAll, recoverAllTask)
	assert.NoError(t, err)
	assert.Equal(t, 2, len(receivedPendingJobID))
}

func TestRecoveryAfterJobDelete(t *testing.T) {
	var err error
	var jobStatesPending = []pb_job.JobState{
		pb_job.JobState_PENDING,
	}
	var jobRuntime = pb_job.RuntimeInfo{
		State: pb_job.JobState_PENDING,
	}
	var jobConfig = pb_job.JobConfig{
		Name:          "TestValidatorWithStore",
		OwningTeam:    "team6",
		LdapGroups:    []string{"money", "team6", "gsg9"},
		InstanceCount: 2,
	}
	var missingJobID = &peloton.JobID{Value: uuid.New()}

	ctrl := gomock.NewController(t)
	ctx := context.Background()
	mockJobStore := store_mocks.NewMockJobStore(ctrl)

	// recoverJobsBatch should pass even if there is no job_id present in job_runtime
	// it should just skip over to a new job. This test is specifically to test the
	// corner case where you deleted a job from job_runtime table but the materialized view
	// created on this table never got updated (this can happen because MV is
	// an experimental feature not supported by Cassandra)

	// mock GetJobsByStates to return missingJobID present in MV but
	// absent from job_runtime
	jobIDs := []peloton.JobID{*missingJobID, *pendingJobID}
	mockJobStore.EXPECT().
		GetJobsByStates(ctx, jobStatesPending).
		Return(jobIDs, nil).
		AnyTimes()

	mockJobStore.EXPECT().
		GetJobRuntime(ctx, missingJobID).
		Return(nil, fmt.Errorf("Cannot find job wth jobID %v", missingJobID.GetValue())).
		AnyTimes()

	mockJobStore.EXPECT().
		GetJobRuntime(ctx, pendingJobID).
		Return(&jobRuntime, nil).
		AnyTimes()

	mockJobStore.EXPECT().
		GetJobConfig(ctx, pendingJobID).
		Return(&jobConfig, nil).
		AnyTimes()

	err = RecoverJobsByState(ctx, mockJobStore, jobStatesPending, recoverPendingTask)
	assert.NoError(t, err)

	mockJobStore.EXPECT().
		GetJobsByStates(ctx, jobStatesPending).
		Return([]peloton.JobID{}, nil).
		AnyTimes()
	err = RecoverJobsByState(ctx, mockJobStore, jobStatesPending, recoverPendingTask)
	assert.NoError(t, err)
}
