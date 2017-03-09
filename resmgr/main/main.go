package main

import (
	"fmt"
	"os"
	"runtime"

	"code.uber.internal/infra/peloton/common"
	"code.uber.internal/infra/peloton/common/config"
	"code.uber.internal/infra/peloton/common/metrics"

	"code.uber.internal/infra/peloton/leader"
	"code.uber.internal/infra/peloton/resmgr"
	"code.uber.internal/infra/peloton/resmgr/respool"
	"code.uber.internal/infra/peloton/resmgr/task"
	"code.uber.internal/infra/peloton/resmgr/taskqueue"
	"code.uber.internal/infra/peloton/resmgr/taskupdate"
	"code.uber.internal/infra/peloton/storage/mysql"
	log "github.com/Sirupsen/logrus"
	"go.uber.org/yarpc"
	"go.uber.org/yarpc/transport"
	"go.uber.org/yarpc/transport/http"
	"gopkg.in/alecthomas/kingpin.v2"
)

var (
	version string
	app     = kingpin.New("peloton-resmgr", "Peloton Resource Manager")

	debug = app.Flag(
		"debug", "enable debug mode (print full json responses)").
		Short('d').
		Default("false").
		Envar("ENABLE_DEBUG_LOGGING").
		Bool()

	cfgFiles = app.Flag(
		"config",
		"YAML config files (can be provided multiple times to merge configs)").
		Short('c').
		Required().
		ExistingFiles()

	dbHost = app.Flag(
		"db-host",
		"Database host (db.host override) (set $DB_HOST to override)").
		Envar("DB_HOST").
		String()

	electionZkServers = app.Flag(
		"election-zk-server",
		"Election Zookeeper servers. Specify multiple times for multiple servers "+
			"(election.zk_servers override) (set $ELECTION_ZK_SERVERS to override)").
		Envar("ELECTION_ZK_SERVERS").
		Strings()

	resmgrPort = app.Flag(
		"port", "Resource manager port (resmgr.port override) (set $PORT to override)").
		Envar("PORT").
		Int()
)

func main() {
	// After go 1.5 the GOMAXPROCS is default to # of CPUs
	// As we need to do quite some DB writes, set the GOMAXPROCS to
	// 2 * NumCPUs
	runtime.GOMAXPROCS(runtime.NumCPU() * 2)
	app.Version(version)
	app.HelpFlag.Short('h')
	kingpin.MustParse(app.Parse(os.Args[1:]))

	log.SetFormatter(&log.JSONFormatter{})
	if *debug {
		log.SetLevel(log.DebugLevel)
	}

	log.WithField("files", *cfgFiles).Info("Loading Resource Manager config")
	var cfg Config
	if err := config.Parse(&cfg, *cfgFiles...); err != nil {
		log.WithField("error", err).Fatal("Cannot parse yaml config")
	}

	// now, override any CLI flags in the loaded config.Config
	if *dbHost != "" {
		cfg.Storage.MySQL.Host = *dbHost
	}

	if len(*electionZkServers) > 0 {
		cfg.Election.ZKServers = *electionZkServers
	}

	if *resmgrPort != 0 {
		cfg.ResManager.Port = *resmgrPort
	}
	log.WithField("config", cfg).Info("Loaded Resource Manager config")

	rootScope, scopeCloser, mux := metrics.InitMetricScope(
		&cfg.Metrics,
		common.PelotonResourceManager,
		metrics.TallyFlushInterval,
	)
	defer scopeCloser.Close()
	rootScope.Counter("boot").Inc(1)

	// Connect to mysql DB
	if err := cfg.Storage.MySQL.Connect(); err != nil {
		log.Fatalf("Could not connect to database: %+v", err)
	}
	// Migrate DB if necessary
	if errs := cfg.Storage.MySQL.AutoMigrate(); errs != nil {
		log.Fatalf("Could not migrate database: %+v", errs)
	}

	// Initialize resmgr store
	respoolStore := mysql.NewResourcePoolStore(cfg.Storage.MySQL.Conn, rootScope)
	respoolStore.DB.SetMaxOpenConns(cfg.ResManager.DbWriteConcurrency)
	respoolStore.DB.SetMaxIdleConns(cfg.ResManager.DbWriteConcurrency)
	respoolStore.DB.SetConnMaxLifetime(cfg.Storage.MySQL.ConnLifeTime)

	// Initialize job and task stores
	jobStore := mysql.NewJobStore(cfg.Storage.MySQL, rootScope)
	jobStore.DB.SetMaxOpenConns(cfg.ResManager.DbWriteConcurrency)
	jobStore.DB.SetMaxIdleConns(cfg.ResManager.DbWriteConcurrency)
	jobStore.DB.SetConnMaxLifetime(cfg.Storage.MySQL.ConnLifeTime)

	// NOTE: we "mount" the YARPC endpoints under /yarpc, so we can
	// mux in other HTTP handlers
	inbounds := []transport.Inbound{
		http.NewInbound(
			fmt.Sprintf(":%d", cfg.ResManager.Port),
			http.Mux(common.PelotonEndpointPath, mux),
		),
	}

	dispatcher := yarpc.NewDispatcher(yarpc.Config{
		Name:     common.PelotonResourceManager,
		Inbounds: inbounds,
	})

	// Initialize service handlers
	respool.InitServiceHandler(dispatcher, rootScope, respoolStore)
	taskqueue.InitServiceHandler(dispatcher, rootScope, jobStore, jobStore)
	task.InitScheduler(cfg.ResManager.TaskSchedulingPeriod)

	taskupdate.InitServiceHandler(dispatcher)
	server := resmgr.NewServer(cfg.ResManager.Port)
	candidate, err := leader.NewCandidate(
		cfg.Election,
		rootScope,
		common.ResourceManagerRole,
		server,
	)

	if err != nil {
		log.Fatalf("Unable to create leader candidate: %v", err)
	}
	err = candidate.Start()
	if err != nil {
		log.Fatalf("Unable to start leader candidate: %v", err)
	}
	defer candidate.Stop()

	// Start dispatch loop
	if err := dispatcher.Start(); err != nil {
		log.Fatalf("Could not start rpc server: %v", err)
	}
	log.Infof("Started Resource Manager on port %v", cfg.ResManager.Port)

	select {}
}
