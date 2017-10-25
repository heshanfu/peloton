package storage

import (
	"github.com/uber-go/tally"
)

// Metrics is a struct for tracking all the general purpose counters that have relevance to the storage
// layer, i.e. how many jobs and tasks were created/deleted in the storage layer
type Metrics struct {
	JobCreate     tally.Counter
	JobCreateFail tally.Counter

	JobUpdate     tally.Counter
	JobUpdateFail tally.Counter

	JobGet      tally.Counter
	JobGetFail  tally.Counter
	JobNotFound tally.Counter

	JobQuery     tally.Counter
	JobQueryAll  tally.Counter
	JobQueryFail tally.Counter

	JobDelete     tally.Counter
	JobDeleteFail tally.Counter

	JobGetRuntime     tally.Counter
	JobGetRuntimeFail tally.Counter

	JobGetByStates     tally.Counter
	JobGetByStatesFail tally.Counter

	JobGetByRespoolID     tally.Counter
	JobGetByRespoolIDFail tally.Counter

	JobUpdateRuntime     tally.Counter
	JobUpdateRuntimeFail tally.Counter

	JobUpdateInfo     tally.Counter
	JobUpdateInfoFail tally.Counter

	TaskCreate     tally.Counter
	TaskCreateFail tally.Counter

	TaskGet      tally.Counter
	TaskGetFail  tally.Counter
	TaskNotFound tally.Counter

	TaskDelete     tally.Counter
	TaskDeleteFail tally.Counter

	TaskUpdate     tally.Counter
	TaskUpdateFail tally.Counter

	// resource pool metrics
	ResourcePoolCreate     tally.Counter
	ResourcePoolCreateFail tally.Counter

	ResourcePoolGet     tally.Counter
	ResourcePoolGetFail tally.Counter

	// FrameworkStore metrics
	FrameworkIDGet     tally.Counter
	FrameworkIDGetFail tally.Counter
	StreamIDGet        tally.Counter
	StreamIDGetFail    tally.Counter

	VolumeCreate     tally.Counter
	VolumeCreateFail tally.Counter
	VolumeUpdate     tally.Counter
	VolumeUpdateFail tally.Counter
	VolumeGet        tally.Counter
	VolumeGetFail    tally.Counter
	VolumeDelete     tally.Counter
	VolumeDeleteFail tally.Counter

	ReadFailure        tally.Counter
	WriteFailure       tally.Counter
	AlreadyExists      tally.Counter
	ReadTimeout        tally.Counter
	WriteTimeout       tally.Counter
	RequestUnavailable tally.Counter
	TooManyTimeouts    tally.Counter
	ConnUnavailable    tally.Counter
	SessionClosed      tally.Counter
	NoConnections      tally.Counter
	ConnectionClosed   tally.Counter
	NoStreams          tally.Counter
	NotTransient       tally.Counter
	CASNotApplied      tally.Counter
}

// NewMetrics returns a new Metrics struct, with all metrics initialized and rooted at the given tally.Scope
func NewMetrics(scope tally.Scope) Metrics {
	jobScope := scope.SubScope("job")
	jobSuccessScope := jobScope.Tagged(map[string]string{"type": "success"})
	jobFailScope := jobScope.Tagged(map[string]string{"type": "fail"})
	jobNotFoundScope := jobScope.Tagged(map[string]string{"type": "not_found"})

	jobRuntimeScope := scope.SubScope("job_runtime")
	jobRuntimeSuccessScope := jobRuntimeScope.Tagged(map[string]string{"type": "success"})
	jobRuntimeFailScope := jobRuntimeScope.Tagged(map[string]string{"type": "fail"})

	jobInfoScope := scope.SubScope("job_info")
	jobInfoSuccessScope := jobInfoScope.Tagged(map[string]string{"type": "success"})
	jobInfoFailScope := jobInfoScope.Tagged(map[string]string{"type": "fail"})

	taskScope := scope.SubScope("task")
	taskSuccessScope := taskScope.Tagged(map[string]string{"type": "success"})
	taskFailScope := taskScope.Tagged(map[string]string{"type": "fail"})
	taskNotFoundScope := taskScope.Tagged(map[string]string{"type": "not_found"})

	resourcePoolScope := scope.SubScope("resource_pool")
	resourcePoolSuccessScope := resourcePoolScope.Tagged(map[string]string{"type": "success"})
	resourcePoolFailScope := resourcePoolScope.Tagged(map[string]string{"type": "fail"})

	frameworkIDScope := scope.SubScope("framework_id")
	frameworkIDSuccessScope := frameworkIDScope.Tagged(map[string]string{"type": "success"})
	frameworkIDFailScope := frameworkIDScope.Tagged(map[string]string{"type": "fail"})

	streamIDScope := scope.SubScope("stream_id")
	streamIDSuccessScope := streamIDScope.Tagged(map[string]string{"type": "success"})
	streamIDFailScope := streamIDScope.Tagged(map[string]string{"type": "fail"})

	volumeScope := scope.SubScope("persistent_volume")
	volumeSuccessScope := volumeScope.Tagged(map[string]string{"type": "success"})
	volumeFailScope := volumeScope.Tagged(map[string]string{"type": "fail"})

	storageErrorScope := scope.SubScope("storage_error")

	metrics := Metrics{
		JobCreate:     jobSuccessScope.Counter("create"),
		JobCreateFail: jobFailScope.Counter("create"),
		JobDelete:     jobSuccessScope.Counter("delete"),
		JobDeleteFail: jobFailScope.Counter("delete"),
		JobGet:        jobSuccessScope.Counter("get"),
		JobGetFail:    jobFailScope.Counter("get"),
		JobUpdate:     jobSuccessScope.Counter("update"),
		JobUpdateFail: jobFailScope.Counter("update"),
		JobNotFound:   jobNotFoundScope.Counter("get"),

		JobQuery:     jobSuccessScope.Counter("query"),
		JobQueryAll:  jobSuccessScope.Counter("query_all"),
		JobQueryFail: jobFailScope.Counter("query"),

		JobGetRuntime:         jobRuntimeSuccessScope.Counter("get"),
		JobGetRuntimeFail:     jobRuntimeFailScope.Counter("get"),
		JobGetByStates:        jobRuntimeSuccessScope.Counter("get_job_by_state"),
		JobGetByStatesFail:    jobRuntimeFailScope.Counter("get_job_by_state"),
		JobGetByRespoolID:     jobRuntimeSuccessScope.Counter("get_job_by_respool_id"),
		JobGetByRespoolIDFail: jobRuntimeFailScope.Counter("get_job_by_respool_id"),
		JobUpdateRuntime:      jobRuntimeSuccessScope.Counter("update"),
		JobUpdateRuntimeFail:  jobRuntimeFailScope.Counter("update"),

		JobUpdateInfo:     jobInfoSuccessScope.Counter("update"),
		JobUpdateInfoFail: jobInfoFailScope.Counter("update"),

		TaskCreate:     taskSuccessScope.Counter("create"),
		TaskCreateFail: taskFailScope.Counter("create"),
		TaskGet:        taskSuccessScope.Counter("get"),
		TaskGetFail:    taskFailScope.Counter("get"),
		TaskDelete:     taskSuccessScope.Counter("delete"),
		TaskDeleteFail: taskFailScope.Counter("delete"),
		TaskUpdate:     taskSuccessScope.Counter("update"),
		TaskUpdateFail: taskFailScope.Counter("update"),
		TaskNotFound:   taskNotFoundScope.Counter("get"),

		ResourcePoolCreate:     resourcePoolSuccessScope.Counter("create"),
		ResourcePoolCreateFail: resourcePoolFailScope.Counter("create"),
		ResourcePoolGet:        resourcePoolSuccessScope.Counter("get"),
		ResourcePoolGetFail:    resourcePoolFailScope.Counter("get"),

		FrameworkIDGet:     frameworkIDSuccessScope.Counter("get"),
		FrameworkIDGetFail: frameworkIDFailScope.Counter("get"),

		StreamIDGet:     streamIDSuccessScope.Counter("get"),
		StreamIDGetFail: streamIDFailScope.Counter("get"),

		VolumeCreate:     volumeSuccessScope.Counter("create"),
		VolumeCreateFail: volumeFailScope.Counter("create"),
		VolumeGet:        volumeSuccessScope.Counter("get"),
		VolumeGetFail:    volumeFailScope.Counter("get"),
		VolumeUpdate:     volumeSuccessScope.Counter("update"),
		VolumeUpdateFail: volumeFailScope.Counter("update"),
		VolumeDelete:     volumeSuccessScope.Counter("delete"),
		VolumeDeleteFail: volumeFailScope.Counter("delete"),

		ReadFailure:        storageErrorScope.Counter("read_failure"),
		WriteFailure:       storageErrorScope.Counter("write_failure"),
		AlreadyExists:      storageErrorScope.Counter("already_exists"),
		ReadTimeout:        storageErrorScope.Counter("read_timeout"),
		WriteTimeout:       storageErrorScope.Counter("write_timeout"),
		RequestUnavailable: storageErrorScope.Counter("request_unavailable"),
		TooManyTimeouts:    storageErrorScope.Counter("too_many_timeouts"),
		ConnUnavailable:    storageErrorScope.Counter("conn_unavailable"),
		SessionClosed:      storageErrorScope.Counter("session_closed"),
		NoConnections:      storageErrorScope.Counter("no_connections"),
		ConnectionClosed:   storageErrorScope.Counter("connection_closed"),
		NoStreams:          storageErrorScope.Counter("no_streams"),
		NotTransient:       storageErrorScope.Counter("not_transient"),
		CASNotApplied:      storageErrorScope.Counter("cas_not_applied"),
	}

	return metrics
}
