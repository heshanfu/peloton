package config

import (
	"time"

	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgr"
	"code.uber.internal/infra/peloton/common/health"
	"code.uber.internal/infra/peloton/common/logging"
	"code.uber.internal/infra/peloton/common/metrics"
	"code.uber.internal/infra/peloton/hostmgr/mesos"
	"code.uber.internal/infra/peloton/leader"
	"code.uber.internal/infra/peloton/storage/config"
)

const (
	// Batch is the batch strategy
	Batch = PlacementStrategy("batch")
	// Mimir is the Mimir strategy
	Mimir = PlacementStrategy("mimir")
)

// Config holds all configs to run a placement engine.
type Config struct {
	Metrics      metrics.Config        `yaml:"metrics"`
	Placement    PlacementConfig       `yaml:"placement"`
	Election     leader.ElectionConfig `yaml:"election"`
	Mesos        mesos.Config          `yaml:"mesos"`
	Health       health.Config         `yaml:"health"`
	Storage      config.Config         `yaml:"storage"`
	SentryConfig logging.SentryConfig  `yaml:"sentry"`
}

// PlacementStrategy determines the placement strategy that the placement
// engine should use.
type PlacementStrategy string

// PlacementConfig is Placement engine specific config
type PlacementConfig struct {
	// HTTP port which hostmgr is listening on
	HTTPPort int `yaml:"http_port"`

	// GRPC port which hostmgr is listening on
	GRPCPort int `yaml:"grpc_port"`

	// TaskDequeueLimit is the max number of tasks to dequeue in a request
	TaskDequeueLimit int `yaml:"task_dequeue_limit"`

	// TaskDequeueTimeOut is th etimeout for the ready queue in resmgr
	TaskDequeueTimeOut int `yaml:"task_dequeue_timeout"`

	// OfferDequeueLimit is the max Number of HostOffers to dequeue in
	// a request
	OfferDequeueLimit int `yaml:"offer_dequeue_limit"`

	// MaxPlacementDuration is the max time duration to place tasks for a task
	// group.
	MaxPlacementDuration time.Duration `yaml:"max_placement_duration"`

	// The task type that the engine is responsible for.
	TaskType resmgr.TaskType `yaml:"task_type"`

	// If this is true the engine will fetch all tasks running on an offer
	// of the same task type as the task type that the coordinator is
	// responsible for.
	FetchOfferTasks bool `yaml:"fetch_offer_tasks"`

	// Strategy is the placement strategy that the engine should use.
	Strategy PlacementStrategy `yaml:"strategy"`

	// Concurrency is the maximal worker concurrency in the engine.
	Concurrency int `yaml:"concurrency"`

	// MaxRounds is maximal number of successful placements that a task can
	// have before it finally gets launched on the current best host for
	// the task. If max rounds for a task type is 0 it means there is no
	// limit on the maximal number of successful placement rounds it can go
	// through.
	MaxRounds MaxRoundsConfig `yaml:"max_rounds"`

	// MaxDurations is maximal time that a task can use being placed before
	// it finally gets launched on the current best host for the task.
	MaxDurations MaxDurationsConfig `yaml:"max_durations"`
}

// MaxRoundsConfig is the config of the maximal number of successful rounds
// that a task should go through before being launched.
type MaxRoundsConfig struct {
	Unknown   int `yaml:"unknown"`
	Batch     int `yaml:"batch"`
	Stateless int `yaml:"stateless"`
	Daemon    int `yaml:"daemon"`
	Stateful  int `yaml:"stateful"`
}

// Value returns the value of the config for the given task type.
func (c MaxRoundsConfig) Value(t resmgr.TaskType) int {
	switch t {
	case resmgr.TaskType_UNKNOWN:
		return c.Unknown
	case resmgr.TaskType_BATCH:
		return c.Batch
	case resmgr.TaskType_STATELESS:
		return c.Stateless
	case resmgr.TaskType_DAEMON:
		return c.Daemon
	case resmgr.TaskType_STATEFUL:
		return c.Stateful
	}
	return 0
}

// MaxDurationsConfig is the config the maximal placement duration of a task
// before it should be launched.
type MaxDurationsConfig struct {
	Unknown   time.Duration `yaml:"unknown"`
	Batch     time.Duration `yaml:"batch"`
	Stateless time.Duration `yaml:"stateless"`
	Daemon    time.Duration `yaml:"daemon"`
	Stateful  time.Duration `yaml:"stateful"`
}

// Value returns the value of the config for the given task type.
func (c MaxDurationsConfig) Value(t resmgr.TaskType) time.Duration {
	switch t {
	case resmgr.TaskType_UNKNOWN:
		return c.Unknown
	case resmgr.TaskType_BATCH:
		return c.Batch
	case resmgr.TaskType_STATELESS:
		return c.Stateless
	case resmgr.TaskType_DAEMON:
		return c.Daemon
	case resmgr.TaskType_STATEFUL:
		return c.Stateful
	}
	return 0
}

// Copy returns a deep copy of the config.
func (config *PlacementConfig) Copy() *PlacementConfig {
	copy := *config
	return &copy
}
