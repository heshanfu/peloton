package jobconfig

import (
	"errors"
	"fmt"
	"reflect"

	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/job"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/task"
	"github.com/hashicorp/go-multierror"
)

const (
	_updateNotSupported = "updating %s not supported"
	// Max retries on task failures.
	_maxTaskRetries = 100
)

var (
	errPortNameMissing    = errors.New("port name is missing")
	errPortEnvNameMissing = errors.New("env name is missing for dynamic port")
	errMaxInstancesTooBig = errors.New("job specified MaximumRunningInstances > InstanceCount")
	errMInInstancesTooBig = errors.New("job specified MinimumRunningInstances > MaximumRunningInstances")
)

// ValidateTaskConfig checks whether the task configs in a job config
// is missing or not, also validates port configs.
func ValidateTaskConfig(jobConfig *job.JobConfig, maxTasksPerJob uint32) error {
	return validateTaskConfigWithRange(jobConfig, maxTasksPerJob, 0, jobConfig.InstanceCount)
}

// ValidateUpdatedConfig validates the changes in the new config
func ValidateUpdatedConfig(oldConfig *job.JobConfig,
	newConfig *job.JobConfig,
	maxTasksPerJob uint32) error {
	errs := new(multierror.Error)
	// Should only update config for new instances,
	// existing config should remain the same,
	// except SLA and Description
	if oldConfig.Name != newConfig.Name {
		errs = multierror.Append(errs,
			fmt.Errorf(_updateNotSupported, "Name"))
	}

	if !reflect.DeepEqual(oldConfig.Labels, newConfig.Labels) {
		errs = multierror.Append(errs,
			fmt.Errorf(_updateNotSupported, "Labels"))
	}

	if oldConfig.OwningTeam != newConfig.OwningTeam {
		errs = multierror.Append(errs,
			fmt.Errorf(_updateNotSupported, "OwningTeam"))
	}

	if oldConfig.RespoolID.GetValue() != newConfig.RespoolID.GetValue() {
		errs = multierror.Append(errs,
			fmt.Errorf(_updateNotSupported, "RespoolID"))
	}

	if oldConfig.Type != newConfig.Type {
		errs = multierror.Append(errs,
			fmt.Errorf(_updateNotSupported, "Type"))
	}

	if !reflect.DeepEqual(oldConfig.LdapGroups, newConfig.LdapGroups) {
		errs = multierror.Append(errs,
			fmt.Errorf(_updateNotSupported, "LdapGroups"))
	}
	if !reflect.DeepEqual(oldConfig.DefaultConfig, newConfig.DefaultConfig) {
		errs = multierror.Append(errs,
			fmt.Errorf(_updateNotSupported, "DefaultConfig"))
	}

	if newConfig.InstanceCount < oldConfig.InstanceCount {
		errs = multierror.Append(errs,
			errors.New("new instance count can't be less"))
	}

	// existing instance config should not be updated
	for i := uint32(0); i < oldConfig.InstanceCount; i++ {
		// no instance config in new config, skip the comparison
		if _, ok := newConfig.InstanceConfig[i]; !ok {
			continue
		}
		// either oldConfig is nil, and newConfig is non-nil, or
		// both are not nil and the configs are different
		if !reflect.DeepEqual(oldConfig.InstanceConfig[i], newConfig.InstanceConfig[i]) {
			errs = multierror.Append(errs,
				errors.New("existing instance config can't be updated"))
			break
		}
	}

	// validate the task configs of new instances
	err := validateTaskConfigWithRange(newConfig,
		maxTasksPerJob,
		oldConfig.InstanceCount,
		newConfig.InstanceCount)
	if err != nil {
		errs = multierror.Append(err)
	}

	return errs.ErrorOrNil()
}

// validateTaskConfigWithRange validates jobConfig with instancesNumber within [from, to)
func validateTaskConfigWithRange(jobConfig *job.JobConfig, maxTasksPerJob uint32, from uint32, to uint32) error {
	// Check if each instance has a default or instance-specific config
	defaultConfig := jobConfig.GetDefaultConfig()
	if err := validatePortConfig(defaultConfig.GetPorts()); err != nil {
		return err
	}

	// Jobs with more than 100k tasks create large Cassandra partitions
	// of more than 100MB. These combined with Size tiered compaction strategy,
	// will trigger large partition summary files to be brought onto the heap and trigger GC.
	// GC trigger will cause read write latency spikes on Cassandra.
	// This was the root cause of the Peloton outage on 04-05-2018
	// Including this artificial limit for now till we change the data model
	// to prevent such large partitions. After changing the data model we can tweak
	// this limit from the job service config or decide to remove the limit altogether.
	if jobConfig.InstanceCount > maxTasksPerJob {
		err := fmt.Errorf("Requested tasks: %v for job is greater than supported: %v tasks/job",
			jobConfig.InstanceCount, maxTasksPerJob)
		return err
	}

	for i := from; i < to; i++ {
		taskConfig := jobConfig.GetInstanceConfig()[i]
		if taskConfig == nil && defaultConfig == nil {
			err := fmt.Errorf("missing task config for instance %v", i)
			return err
		}

		restartPolicy := defaultConfig.GetRestartPolicy()
		if taskConfig.GetRestartPolicy() != nil {
			restartPolicy = taskConfig.GetRestartPolicy()
		}
		if restartPolicy.GetMaxFailures() > _maxTaskRetries {
			restartPolicy.MaxFailures = _maxTaskRetries
		}

		// Validate port config
		if err := validatePortConfig(taskConfig.GetPorts()); err != nil {
			return err
		}

		// Validate command info
		cmd := defaultConfig.GetCommand()
		if taskConfig.GetCommand() != nil {
			cmd = taskConfig.GetCommand()
		}
		if cmd == nil {
			err := fmt.Errorf("missing command info for instance %v", i)
			return err
		}
	}

	// Validate sla max/min running instances wrt instanceCount
	instanceCount := jobConfig.InstanceCount
	maxRunningInstances := jobConfig.GetSLA().GetMaximumRunningInstances()
	if maxRunningInstances == 0 {
		maxRunningInstances = instanceCount
	} else if maxRunningInstances > instanceCount {
		return errMaxInstancesTooBig
	}
	minRunningInstances := jobConfig.GetSLA().GetMinimumRunningInstances()
	if minRunningInstances > maxRunningInstances {
		return errMaxInstancesTooBig
	}

	return nil
}

// validatePortConfig checks port name and port env name exists for dynamic port.
func validatePortConfig(portConfigs []*task.PortConfig) error {
	for _, port := range portConfigs {
		if len(port.GetName()) == 0 {
			return errPortNameMissing
		}
		if port.GetValue() == 0 && len(port.GetEnvName()) == 0 {
			return errPortEnvNameMissing
		}
	}
	return nil
}
