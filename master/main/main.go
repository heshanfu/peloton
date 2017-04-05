package main

import (
	"fmt"
	nethttp "net/http"
	"net/url"
	"os"
	"runtime"
	"time"

	"gopkg.in/alecthomas/kingpin.v2"

	"code.uber.internal/infra/peloton/common"
	"code.uber.internal/infra/peloton/common/config"
	"code.uber.internal/infra/peloton/common/logging"
	"code.uber.internal/infra/peloton/common/metrics"

	"code.uber.internal/infra/peloton/hostmgr"
	"code.uber.internal/infra/peloton/hostmgr/mesos"
	"code.uber.internal/infra/peloton/hostmgr/offer"
	"code.uber.internal/infra/peloton/hostmgr/reconcile"
	"code.uber.internal/infra/peloton/jobmgr/job"
	"code.uber.internal/infra/peloton/jobmgr/task"
	"code.uber.internal/infra/peloton/leader"
	"code.uber.internal/infra/peloton/master"
	master_task "code.uber.internal/infra/peloton/master/task"
	"code.uber.internal/infra/peloton/master/upgrade"
	"code.uber.internal/infra/peloton/placement"
	"code.uber.internal/infra/peloton/resmgr"
	"code.uber.internal/infra/peloton/resmgr/respool"
	resmgr_task "code.uber.internal/infra/peloton/resmgr/task"
	"code.uber.internal/infra/peloton/resmgr/taskqueue"
	resmgr_taskupdate "code.uber.internal/infra/peloton/resmgr/taskupdate"
	"code.uber.internal/infra/peloton/yarpc/encoding/mpb"
	"code.uber.internal/infra/peloton/yarpc/peer"
	"code.uber.internal/infra/peloton/yarpc/transport/mhttp"

	"code.uber.internal/infra/peloton/storage"
	"code.uber.internal/infra/peloton/storage/cassandra"
	"code.uber.internal/infra/peloton/storage/mysql"

	log "github.com/Sirupsen/logrus"
	"go.uber.org/yarpc"
	"go.uber.org/yarpc/transport"
	"go.uber.org/yarpc/transport/http"
)

var (
	version string
	app     = kingpin.New("peloton-master", "Peloton Master")

	debug = app.Flag(
		"debug",
		"enable debug mode (print full json responses)").
		Short('d').
		Default("false").
		Envar("ENABLE_DEBUG_LOGGING").
		Bool()

	configFiles = app.Flag(
		"config",
		"YAML framework configuration (can be provided multiple times "+
			"to merge configs)").
		Short('c').
		Required().
		ExistingFiles()

	env = app.Flag(
		"env",
		"environment (development will do no mesos master auto discovery) "+
			"(set $ENVIRONMENT to override)").
		Short('e').
		Default("development").
		Envar("ENVIRONMENT").
		Enum("development", "production")

	zkPath = app.Flag(
		"zk-path",
		"Zookeeper path (mesos.zk_host override) (set $MESOS_ZK_PATH to "+
			"override)").
		Envar("MESOS_ZK_PATH").
		String()

	dbHost = app.Flag(
		"db-host",
		"Database host (db.host override) (set $DB_HOST to override)").
		Envar("DB_HOST").
		String()

	taskDequeueLimit = app.Flag(
		"task-dequeue-limit",
		"Placement Engine task dequeue limit (placement.task_dequeue_limit "+
			"override) (set $PLACEMENT_TASK_DEQUEUE_LIMIT to override)").
		Envar("PLACEMENT_TASK_DEQUEUE_LIMIT").
		Int()

	electionZkServers = app.Flag(
		"election-zk-server",
		"Election Zookeeper servers. Specify multiple times for multiple "+
			"servers (election.zk_servers override) (set $ELECTION_ZK_SERVERS"+
			"to override)").
		Envar("ELECTION_ZK_SERVERS").
		Strings()

	port = app.Flag(
		"port",
		"Master port (master.port override) (set $PORT to override)").
		Envar("PORT").
		Int()

	offerHoldTime = app.Flag(
		"offer-hold",
		"Master offer time (master.offer_hold_time_sec override) "+
			"(set $OFFER_HOLD_TIME to override)").
		HintOptions("5s", "1m").
		Envar("OFFER_HOLD_TIME").
		Duration()

	offerPruningPeriod = app.Flag(
		"offer-pruning-period",
		"Master offer pruning period (master.offer_pruning_period_sec "+
			"override) (set $OFFER_PRUNING_PERIOD to override)").
		HintOptions("20s").
		Envar("OFFER_PRUNING_PERIOD").
		Duration()

	useCassandra = app.Flag(
		"use-cassandra", "Use cassandra storage implementation").
		Default("true").
		Envar("USE_CASSANDRA").
		Bool()

	cassandraHosts = app.Flag(
		"cassandra-hosts", "Cassandra hosts").
		Envar("CASSANDRA_HOSTS").
		Strings()
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

	initialLevel := log.InfoLevel
	if *debug {
		initialLevel = log.DebugLevel
	}
	log.SetLevel(initialLevel)

	log.Infof("Loading config from %v...", *configFiles)
	var cfg Config
	if err := config.Parse(&cfg, *configFiles...); err != nil {
		log.WithField("error", err).Fatal("Cannot parse yaml config")
	}

	// now, override any CLI flags in the loaded config.Config
	if *zkPath != "" {
		cfg.Mesos.ZkPath = *zkPath
	}
	if *dbHost != "" {
		cfg.Storage.MySQL.Host = *dbHost
	}
	if *taskDequeueLimit != 0 {
		cfg.Placement.TaskDequeueLimit = *taskDequeueLimit
	}
	if len(*electionZkServers) > 0 {
		cfg.Election.ZKServers = *electionZkServers
	}
	if *port != 0 {
		cfg.Master.Port = *port
	}
	if *offerHoldTime != 0 {
		cfg.Master.OfferHoldTimeSec = int(offerHoldTime.Seconds())
	}
	if *offerPruningPeriod != 0 {
		cfg.Master.OfferPruningPeriodSec = int(offerPruningPeriod.Seconds())
	}
	if !*useCassandra {
		cfg.Storage.UseCassandra = false
	}
	if *cassandraHosts != nil && len(*cassandraHosts) > 0 {
		if *useCassandra {
			cfg.Storage.Cassandra.CassandraConn.ContactPoints = *cassandraHosts
		}
	}

	log.WithField("config", cfg).Info("Loaded Peloton Master configuration")

	rootScope, scopeCloser, mux := metrics.InitMetricScope(
		&cfg.Metrics,
		common.PelotonMaster,
		metrics.TallyFlushInterval,
	)
	defer scopeCloser.Close()

	rootScope.Counter("boot").Inc(1)

	mux.HandleFunc(logging.LevelOverwrite, logging.LevelOverwriteHandler(initialLevel))

	var jobStore storage.JobStore
	var taskStore storage.TaskStore
	var frameworkStore storage.FrameworkInfoStore
	var respoolStore storage.ResourcePoolStore

	// This is mandatory until resmgr supports stapi, otherwise resmgr
	// will crash

	if !cfg.Storage.UseCassandra {
		// Connect to mysql DB
		if err := cfg.Storage.MySQL.Connect(); err != nil {
			log.Fatalf("Could not connect to database: %+v", err)
		}
		// Migrate DB if necessary
		if errs := cfg.Storage.MySQL.AutoMigrate(); errs != nil {
			log.Fatalf("Could not migrate database: %+v", errs)
		}

		// Initialize job and task stores
		store := mysql.NewStore(cfg.Storage.MySQL, rootScope)
		store.DB.SetMaxOpenConns(cfg.Master.DbWriteConcurrency)
		store.DB.SetMaxIdleConns(cfg.Master.DbWriteConcurrency)
		store.DB.SetConnMaxLifetime(cfg.Storage.MySQL.ConnLifeTime)

		jobStore = store
		taskStore = store
		frameworkStore = store
		// Initialize resmgr store
		respoolStore := mysql.NewResourcePoolStore(
			cfg.Storage.MySQL.Conn,
			rootScope,
		)
		respoolStore.DB.SetMaxOpenConns(cfg.Master.DbWriteConcurrency)
		respoolStore.DB.SetMaxIdleConns(cfg.Master.DbWriteConcurrency)
		respoolStore.DB.SetConnMaxLifetime(cfg.Storage.MySQL.ConnLifeTime)
	} else {
		log.Infof("cassandra Config: %v", cfg.Storage.Cassandra)
		if errs := cfg.Storage.Cassandra.AutoMigrate(); errs != nil {
			log.Fatalf("Could not migrate database: %+v", errs)
		}
		store, err := cassandra.NewStore(&cfg.Storage.Cassandra, rootScope)
		if err != nil {
			log.Fatalf("Could not create cassandra store: %+v", err)
		}
		jobStore = store
		taskStore = store
		frameworkStore = store
		respoolStore = store
	}
	// Initialize YARPC dispatcher with necessary inbounds and outbounds
	driver := mesos.InitSchedulerDriver(&cfg.Mesos, frameworkStore)

	// NOTE: we "mount" the YARPC endpoints under /yarpc, so we can
	// mux in other HTTP handlers
	inbounds := []transport.Inbound{
		http.NewInbound(
			fmt.Sprintf(":%d", cfg.Master.Port),
			http.Mux(common.PelotonEndpointPath, mux),
		),
	}

	mesosMasterLocation := cfg.Mesos.HostPort
	mesosMasterDetector, err := mesos.NewZKDetector(cfg.Mesos.ZkPath)
	if err != nil {
		log.Fatalf("Failed to initialize mesos master detector: %v", err)
	}

	mesosMasterLocation, err = mesosMasterDetector.GetMasterLocation()
	if err != nil {
		log.Fatalf("Failed to get mesos leading master location, err=%v", err)
	}
	log.Infof("Detected Mesos leading master location: %s", mesosMasterLocation)

	// Each master needs a Mesos inbound
	var mInbound = mhttp.NewInbound(rootScope, driver)
	inbounds = append(inbounds, mInbound)

	// TODO: update mesos url when leading mesos master changes
	mesosURL := fmt.Sprintf("http://%s%s", mesosMasterLocation, driver.Endpoint())

	mOutbounds := mhttp.NewOutbound(mesosURL)
	peerChooser, err := peer.NewSmartChooser(
		cfg.Election,
		rootScope,
		common.MasterRole,
	)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "role": common.MasterRole}).
			Fatal("Could not create smart peer chooser")
	}
	if err := peerChooser.Start(); err != nil {
		log.WithFields(log.Fields{"error": err, "role": common.MasterRole}).
			Fatal("Could not start smart peer chooser")
	}
	defer peerChooser.Stop()

	// The leaderUrl for pOutbound would be updated by leader election
	// NewLeaderCallBack once leader is elected
	pOutbound := http.NewChooserOutbound(
		peerChooser,
		&url.URL{Scheme: "http", Path: common.PelotonEndpointPath},
	)
	pOutbounds := transport.Outbounds{
		Unary: pOutbound,
	}
	outbounds := yarpc.Outbounds{
		common.MesosMaster:   mOutbounds,
		common.PelotonMaster: pOutbounds,
	}
	dispatcher := yarpc.NewDispatcher(yarpc.Config{
		Name:      common.PelotonMaster,
		Inbounds:  inbounds,
		Outbounds: outbounds,
	})

	// TODO: Refactor our storage interfaces to avoid passing both
	// jobstore and taskstore.

	// Initialize job manager related handlers
	job.InitServiceHandler(
		dispatcher,
		rootScope,
		jobStore,
		taskStore,
		common.PelotonMaster, // TODO: to be removed
	)
	task.InitServiceHandler(
		dispatcher,
		rootScope,
		jobStore,
		taskStore,
		common.PelotonMaster, // TODO: to be removed
	)

	upgrade.InitManager(dispatcher)

	// Initialize resource manager related handlers

	// Initialize resource pool service handler
	resmgrInbounds := []transport.Inbound{
		http.NewInbound(
			fmt.Sprintf(":%d", cfg.ResManager.Port),
			http.Mux(common.PelotonEndpointPath, nethttp.NewServeMux()),
		),
	}
	// create a separate dispatcher for resmgr so client can work with
	// both master and multi-app modes
	resmgrDispatcher := yarpc.NewDispatcher(yarpc.Config{
		Name:     common.PelotonResourceManager,
		Inbounds: resmgrInbounds,
	})

	// Initialize resource manager related service handlers
	respool.InitServiceHandler(resmgrDispatcher, rootScope, respoolStore)
	taskqueue.InitServiceHandler(dispatcher, rootScope, jobStore, taskStore)
	resmgr_task.InitScheduler(cfg.ResManager.TaskSchedulingPeriod)
	resmgr.InitServiceHandler(dispatcher, rootScope)

	// Initialize host manager related handlers

	// Init the managers driven by the mesos callbacks.
	// They are driven by the leader who will subscribe to
	// mesos callbacks
	mesos.InitManager(dispatcher, &cfg.Mesos, frameworkStore)
	mesosClient := mpb.New(
		dispatcher.ClientConfig(common.MesosMaster),
		cfg.Mesos.Encoding,
	)
	offer.InitEventHandler(
		dispatcher,
		rootScope,
		time.Duration(cfg.Master.OfferHoldTimeSec)*time.Second,
		time.Duration(cfg.Master.OfferPruningPeriodSec)*time.Second,
		mesosClient,
	)

	master_task.InitTaskStateManager(
		dispatcher,
		cfg.Master.TaskUpdateBufferSize,
		cfg.Master.TaskUpdateAckConcurrency,
		common.PelotonMaster,
		rootScope)

	// Init host manager service handler
	hostmgr.InitServiceHandler(
		dispatcher,
		rootScope,
		mesosClient,
		driver,
	)

	// Start master dispatch loop
	if err := dispatcher.Start(); err != nil {
		log.Fatalf("Could not start rpc server: %v", err)
	}
	log.Infof("Started Peloton master on port %v", cfg.Master.Port)

	// Init task status update
	task.InitTaskStatusUpdate(
		dispatcher,
		common.PelotonMaster,
		taskStore,
		rootScope,
	)

	// Start resmgr dispatch loop
	if err := resmgrDispatcher.Start(); err != nil {
		log.Fatalf("Could not start rpc server: %v", err)
	}
	log.Infof("Started Resource Manager on port %v", cfg.ResManager.Port)
	resmgr_taskupdate.InitServiceHandler(dispatcher)

	reconcile.InitTaskReconciler(
		mesosClient,
		rootScope,
		driver,
		jobStore,
		taskStore,
		cfg.HostManager.TaskReconcilerConfig,
	)

	server := master.NewServer(
		cfg.Master.Port,
		mesosMasterDetector,
		mInbound,
		mOutbounds,
	)
	candidate, err := leader.NewCandidate(
		cfg.Election,
		rootScope,
		common.MasterRole,
		server)
	if err != nil {
		log.Fatalf("Unable to create leader candidate: %v", err)
	}
	err = candidate.Start()
	if err != nil {
		log.Fatalf("Unable to start leader candidate: %v", err)
	}
	defer candidate.Stop()

	// Initialize and start placement engine
	placementEngine := placement.New(
		dispatcher,
		rootScope,
		&cfg.Placement,
		common.PelotonMaster,
		common.PelotonMaster,
	)
	placementEngine.Start()
	defer placementEngine.Stop()

	task.InitTaskLauncher(
		dispatcher,
		common.PelotonMaster,
		common.PelotonMaster,
		taskStore,
		&cfg.JobManager,
		rootScope,
	)
	task.GetLauncher().Start()
	defer task.GetLauncher().Stop()

	select {}
}
