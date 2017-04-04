package main

import (
	"fmt"
	"net/url"
	"os"
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
	"code.uber.internal/infra/peloton/leader"
	"code.uber.internal/infra/peloton/master/task"
	"code.uber.internal/infra/peloton/yarpc/encoding/mpb"
	"code.uber.internal/infra/peloton/yarpc/peer"
	"code.uber.internal/infra/peloton/yarpc/transport/mhttp"

	"go.uber.org/yarpc"
	"go.uber.org/yarpc/transport"
	"go.uber.org/yarpc/transport/http"

	"code.uber.internal/infra/peloton/storage/stores"
	log "github.com/Sirupsen/logrus"
)

var (
	version string
	app     = kingpin.New("peloton-hostmgr", "Peloton Host Manager")

	debug = app.Flag(
		"debug", "enable debug mode (print full json responses)").
		Short('d').
		Default("false").
		Envar("ENABLE_DEBUG_LOGGING").
		Bool()

	configFiles = app.Flag(
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

	hostmgrPort = app.Flag(
		"port", "Host manager port (hostmgr.port override) (set $PORT to override)").
		Envar("PORT").
		Int()

	zkPath = app.Flag(
		"zk-path",
		"Zookeeper path (mesos.zk_host override) (set $MESOS_ZK_PATH to override)").
		Envar("MESOS_ZK_PATH").
		String()

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
	app.Version(version)
	app.HelpFlag.Short('h')
	kingpin.MustParse(app.Parse(os.Args[1:]))

	log.SetFormatter(&log.JSONFormatter{})

	initialLevel := log.InfoLevel
	if *debug {
		initialLevel = log.DebugLevel
	}
	log.SetLevel(initialLevel)

	log.WithField("files", *configFiles).Info("Loading host manager config")
	var cfg Config
	if err := config.Parse(&cfg, *configFiles...); err != nil {
		log.WithField("error", err).Fatal("Cannot parse yaml config")
	}

	// now, override any CLI flags in the loaded config.Config
	if *hostmgrPort != 0 {
		cfg.HostManager.Port = *hostmgrPort
	}

	if *zkPath != "" {
		cfg.Mesos.ZkPath = *zkPath
	}

	if *dbHost != "" {
		cfg.Storage.MySQL.Host = *dbHost
	}

	if len(*electionZkServers) > 0 {
		cfg.Election.ZKServers = *electionZkServers
	}

	if !*useCassandra {
		cfg.Storage.UseCassandra = false
	}

	if *cassandraHosts != nil && len(*cassandraHosts) > 0 {
		cfg.Storage.Cassandra.CassandraConn.ContactPoints = *cassandraHosts
	}

	log.WithField("config", cfg).Info("Loaded host manager config")

	rootScope, scopeCloser, mux := metrics.InitMetricScope(
		&cfg.Metrics,
		common.PelotonHostManager,
		metrics.TallyFlushInterval,
	)
	defer scopeCloser.Close()

	mux.HandleFunc(
		logging.LevelOverwrite,
		logging.LevelOverwriteHandler(initialLevel))

	// NOTE: we "mount" the YARPC endpoints under /yarpc, so we can
	// mux in other HTTP handlers
	inbounds := []transport.Inbound{
		http.NewInbound(
			fmt.Sprintf(":%d", cfg.HostManager.Port),
			http.Mux(common.PelotonEndpointPath, mux),
		),
	}

	mesosMasterDetector, err := mesos.NewZKDetector(cfg.Mesos.ZkPath)
	if err != nil {
		log.Fatalf("Failed to initialize mesos master detector: %v", err)
	}

	// NOTE: we start the server immediately even if no leader has been
	// detected yet.

	rootScope.Counter("boot").Inc(1)

	jobStore, taskStore, _, frameworkInfoStore := stores.CreateStores(&cfg.Storage, rootScope)

	// Initialize YARPC dispatcher with necessary inbounds and outbounds
	driver := mesos.InitSchedulerDriver(&cfg.Mesos, frameworkInfoStore)

	// Active host manager needs a Mesos inbound
	var mInbound = mhttp.NewInbound(rootScope, driver)
	inbounds = append(inbounds, mInbound)

	// TODO: update mesos url when leading mesos master changes
	mOutbound := mhttp.NewOutbound(mesosMasterDetector, driver.Endpoint())

	// all leader discovery metrics share a scope (and will be tagged
	// with role={role})
	discoveryScope := rootScope.SubScope("discovery")
	// setup the discovery service to detect resmgr leaders and
	// configure the YARPC Peer dynamically
	resmgrPeerChooser, err := peer.NewSmartChooser(
		cfg.Election,
		discoveryScope,
		common.ResourceManagerRole,
	)
	if err != nil {
		log.WithFields(log.Fields{"error": err, "role": common.ResourceManagerRole}).
			Fatal("Could not create smart peer chooser")
	}
	if err := resmgrPeerChooser.Start(); err != nil {
		log.WithFields(log.Fields{"error": err, "role": common.ResourceManagerRole}).
			Fatal("Could not start smart peer chooser")
	}
	defer resmgrPeerChooser.Stop()
	resmgrOutbound := http.NewChooserOutbound(
		resmgrPeerChooser,
		&url.URL{
			Scheme: "http",
			Path:   common.PelotonEndpointPath,
		})

	outbounds := yarpc.Outbounds{
		common.MesosMaster: mOutbound,
		common.PelotonResourceManager: transport.Outbounds{
			Unary: resmgrOutbound,
		},
	}

	dispatcher := yarpc.NewDispatcher(yarpc.Config{
		Name:      common.PelotonHostManager,
		Inbounds:  inbounds,
		Outbounds: outbounds,
	})

	// Init the managers driven by the mesos callbacks.
	// They are driven by the leader who will subscribe to
	// Mesos callbacks
	// NOTE: This blocks us to move all Mesos related logic into
	// hostmgr.Server because mesosClient uses dispatcher...
	mesosClient := mpb.New(
		dispatcher.ClientConfig(common.MesosMaster),
		cfg.Mesos.Encoding,
	)
	mesos.InitManager(dispatcher, &cfg.Mesos, frameworkInfoStore)

	log.WithFields(log.Fields{
		"port":     cfg.HostManager.Port,
		"url_path": common.PelotonEndpointPath,
	}).Info("HostService initialized")

	offer.InitEventHandler(
		dispatcher,
		rootScope,
		time.Duration(cfg.HostManager.OfferHoldTimeSec)*time.Second,
		time.Duration(cfg.HostManager.OfferPruningPeriodSec)*time.Second,
		mesosClient,
	)

	// Init service handler.
	hostmgr.InitServiceHandler(
		dispatcher,
		rootScope,
		mesosClient,
		driver,
	)

	// Initializing TaskStateManager will start to record task status
	// update back to storage.  TODO(zhitao): This is
	// temporary. Eventually we should create proper API protocol for
	// `WaitTaskStatusUpdate` and allow RM/JM to retrieve this
	// separately.
	task.InitTaskStateManager(
		dispatcher,
		cfg.HostManager.TaskUpdateBufferSize,
		cfg.HostManager.TaskUpdateAckConcurrency,
		common.PelotonResourceManager,
		rootScope,
	)

	reconcile.InitTaskReconciler(
		mesosClient,
		rootScope,
		driver,
		jobStore,
		taskStore,
		cfg.HostManager.TaskReconcilerConfig,
	)

	server := hostmgr.NewServer(
		rootScope,
		cfg.HostManager.Port,
		mesosMasterDetector,
		mInbound,
		mOutbound,
	)

	candidate, err := leader.NewCandidate(
		cfg.Election,
		rootScope.SubScope("election"),
		common.HostManagerRole,
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

	select {}
}
