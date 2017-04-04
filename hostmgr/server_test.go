package hostmgr

import (
	"errors"
	"testing"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/suite"
	"github.com/uber-go/tally"

	hm_mocks "code.uber.internal/infra/peloton/hostmgr/mesos/mocks"
	"code.uber.internal/infra/peloton/hostmgr/offer"
	offer_mocks "code.uber.internal/infra/peloton/hostmgr/offer/mocks"
	"code.uber.internal/infra/peloton/hostmgr/reconcile"
	reconcile_mocks "code.uber.internal/infra/peloton/hostmgr/reconcile/mocks"
	mhttp_mocks "code.uber.internal/infra/peloton/yarpc/transport/mhttp/mocks"
)

const (
	_ID   = "test-id"
	_role = "test-role"

	_hostPort = "1.2.3.4:5"
)

var (
	errFoo = errors.New("test")
)

type ServerTestSuite struct {
	suite.Suite

	ctrl *gomock.Controller

	testScope tally.TestScope

	eventHandler *offer_mocks.MockEventHandler
	reconciler   *reconcile_mocks.MockTaskReconciler
	detector     *hm_mocks.MockMasterDetector
	mInbound     *mhttp_mocks.MockInbound

	server *Server
}

func (suite *ServerTestSuite) SetupTest() {
	suite.ctrl = gomock.NewController(suite.T())
	suite.testScope = tally.NewTestScope("", map[string]string{})
	suite.eventHandler = offer_mocks.NewMockEventHandler(suite.ctrl)
	suite.reconciler = reconcile_mocks.NewMockTaskReconciler(suite.ctrl)
	suite.detector = hm_mocks.NewMockMasterDetector(suite.ctrl)
	suite.mInbound = mhttp_mocks.NewMockInbound(suite.ctrl)

	suite.server = &Server{
		ID:   _ID,
		role: _role,

		getOfferEventHandler: func() offer.EventHandler {
			return suite.eventHandler
		},

		getTaskReconciler: func() reconcile.TaskReconciler {
			return suite.reconciler
		},

		mesosDetector: suite.detector,
		mesosInbound:  suite.mInbound,
		// Add outbound when we need it.

		minBackoff: _minBackoff,
		maxBackoff: _maxBackoff,

		metrics: NewMetrics(suite.testScope),
	}
}

func (suite *ServerTestSuite) TearDownTest() {
	log.Debug("tearing down")
}

// Tests that if unelected and things are stopped, doing nothing.
func (suite *ServerTestSuite) TestUnelectedNoOp() {
	suite.server.elected.Store(false)
	suite.server.handlersRunning.Store(false)
	gomock.InOrder(
		suite.mInbound.EXPECT().IsRunning().Return(false).Times(2),
	)
	suite.server.ensureStateRound()
	suite.ctrl.Finish()
	suite.Zero(suite.server.currentBackoffNano.Load())
	suite.False(suite.server.elected.Load())
	suite.False(suite.server.handlersRunning.Load())
}

// Tests that if unelected but seeing connection, calling stop on them.
func (suite *ServerTestSuite) TestUnelectedStopConnection() {
	suite.server.elected.Store(false)
	suite.server.handlersRunning.Store(false)
	gomock.InOrder(
		suite.mInbound.EXPECT().IsRunning().Return(true),
		suite.mInbound.EXPECT().Stop(),
		suite.mInbound.EXPECT().IsRunning().Return(false),
	)
	suite.server.ensureStateRound()
	suite.ctrl.Finish()
	suite.Zero(suite.server.currentBackoffNano.Load())
	suite.False(suite.server.elected.Load())
	suite.False(suite.server.handlersRunning.Load())
}

// Tests that if unelected but seeing handlers running, calling stop on them.
func (suite *ServerTestSuite) TestUnelectedStopHandler() {
	suite.server.elected.Store(false)
	suite.server.handlersRunning.Store(true)
	gomock.InOrder(
		suite.mInbound.EXPECT().IsRunning().Return(false),
		suite.reconciler.EXPECT().Stop(),
		suite.eventHandler.EXPECT().Stop(),
		suite.mInbound.EXPECT().IsRunning().Return(false),
	)
	suite.server.ensureStateRound()
	suite.ctrl.Finish()
	suite.Zero(suite.server.currentBackoffNano.Load())
	suite.False(suite.server.elected.Load())
	suite.False(suite.server.handlersRunning.Load())
}

// Tests that if unelected but seeing handlers and connection, stop both.
func (suite *ServerTestSuite) TestUnelectedStopConnectionAndHandler() {
	suite.server.elected.Store(false)
	suite.server.handlersRunning.Store(true)
	gomock.InOrder(
		suite.mInbound.EXPECT().IsRunning().Return(true),
		suite.mInbound.EXPECT().Stop(),
		suite.reconciler.EXPECT().Stop(),
		suite.eventHandler.EXPECT().Stop(),
		suite.mInbound.EXPECT().IsRunning().Return(false),
	)
	suite.server.ensureStateRound()
	suite.ctrl.Finish()
	suite.Zero(suite.server.currentBackoffNano.Load())
	suite.False(suite.server.elected.Load())
	suite.False(suite.server.handlersRunning.Load())
}

// Tests that if election and things are running, doing nothing.
func (suite *ServerTestSuite) TestElectedNoOp() {
	suite.server.elected.Store(true)
	suite.server.handlersRunning.Store(true)
	gomock.InOrder(
		suite.mInbound.EXPECT().IsRunning().Return(true).AnyTimes(),
	)
	suite.server.ensureStateRound()
	suite.ctrl.Finish()
	suite.Zero(suite.server.currentBackoffNano.Load())
	suite.True(suite.server.elected.Load())
	suite.True(suite.server.handlersRunning.Load())
}

// Tests that if elected but seeing stopped connection, restart it.
func (suite *ServerTestSuite) TestElectedRestartConnection() {
	suite.server.elected.Store(true)
	suite.server.handlersRunning.Store(true)
	gomock.InOrder(
		// Initial check for Mesos connection.
		suite.mInbound.EXPECT().IsRunning().Return(false),

		// Stop handlers.
		suite.reconciler.EXPECT().Stop(),
		suite.eventHandler.EXPECT().Stop(),

		// Detect leader and start loop successfully.
		suite.detector.EXPECT().HostPort().Return(_hostPort),
		suite.mInbound.
			EXPECT().
			StartMesosLoop(gomock.Eq(_hostPort)).
			Return(nil, nil),

		// Connected, now start handlers.
		suite.mInbound.EXPECT().IsRunning().Return(true),
		suite.reconciler.EXPECT().Start(),
		suite.eventHandler.EXPECT().Start(),

		// Last check for connected, used in gauge reporting.
		suite.mInbound.EXPECT().IsRunning().Return(true),
	)
	suite.server.ensureStateRound()
	suite.ctrl.Finish()
	suite.Zero(suite.server.currentBackoffNano.Load())
	suite.True(suite.server.elected.Load())
	suite.True(suite.server.handlersRunning.Load())
}

// Tests that if elected but seeing stopped handlers, restart.
func (suite *ServerTestSuite) TestElectedRestartHandlers() {
	suite.server.elected.Store(true)
	suite.server.handlersRunning.Store(false)
	gomock.InOrder(
		suite.mInbound.EXPECT().IsRunning().Return(true).Times(2),
		suite.reconciler.EXPECT().Start(),
		suite.eventHandler.EXPECT().Start(),
		suite.mInbound.EXPECT().IsRunning().Return(true),
	)
	suite.server.ensureStateRound()
	suite.ctrl.Finish()
	suite.Zero(suite.server.currentBackoffNano.Load())
	suite.True(suite.server.elected.Load())
	suite.True(suite.server.handlersRunning.Load())
}

// Tests that if elected but seeing stopped handlers and connection,
// restart both.
func (suite *ServerTestSuite) TestElectedRestartConnectionAndHandler() {
	suite.server.elected.Store(true)
	suite.server.handlersRunning.Store(false)
	gomock.InOrder(
		// Initial check for Mesos connection.
		suite.mInbound.EXPECT().IsRunning().Return(false),

		// Detect leader and start loop successfully.
		suite.detector.EXPECT().HostPort().Return(_hostPort),
		suite.mInbound.
			EXPECT().
			StartMesosLoop(gomock.Eq(_hostPort)).
			Return(nil, nil),

		// Connected, now start handlers.
		suite.mInbound.EXPECT().IsRunning().Return(true),
		suite.reconciler.EXPECT().Start(),
		suite.eventHandler.EXPECT().Start(),

		// Last check for connected, used in gauge reporting.
		suite.mInbound.EXPECT().IsRunning().Return(true),
	)
	suite.server.ensureStateRound()
	suite.ctrl.Finish()
	suite.Zero(suite.server.currentBackoffNano.Load())
	suite.True(suite.server.elected.Load())
	suite.True(suite.server.handlersRunning.Load())
}

// Tests that Mesos connection failure triggers a backoff.
func (suite *ServerTestSuite) TestBackoffOnMesosConnectFailure() {
	suite.server.elected.Store(true)
	suite.server.handlersRunning.Store(false)

	lower := time.Now()
	upper := lower.Add(suite.server.minBackoff * 2)

	gomock.InOrder(
		// Initial check for Mesos connection.
		suite.mInbound.
			EXPECT().
			IsRunning().
			Return(false),

		// Detector returns a real host.
		suite.detector.
			EXPECT().
			HostPort().
			Return(_hostPort),

		// StartMesosLoop returns an error.
		suite.mInbound.
			EXPECT().
			StartMesosLoop(gomock.Eq(_hostPort)).
			Return(nil, errFoo),

		// For stats gathering.
		suite.mInbound.
			EXPECT().
			IsRunning().
			Return(false).
			Times(2),
	)
	suite.server.ensureStateRound()
	suite.ctrl.Finish()
	suite.Equal(
		suite.server.minBackoff.Nanoseconds(),
		suite.server.currentBackoffNano.Load())
	suite.True(lower.UnixNano() < suite.server.backoffUntilNano.Load())
	suite.True(upper.UnixNano() > suite.server.backoffUntilNano.Load())
}

// Tests that backoff doubles on another failure.
func (suite *ServerTestSuite) TestDoubleBackoff() {
	now := time.Now()
	lower := now.Add(suite.server.minBackoff * 2)
	upper := lower.Add(suite.server.minBackoff * 2)

	suite.server.elected.Store(true)
	suite.server.handlersRunning.Store(false)
	suite.server.currentBackoffNano.Store(
		suite.server.minBackoff.Nanoseconds())
	suite.server.backoffUntilNano.Store(now.UnixNano())

	gomock.InOrder(
		// Initial check for Mesos connection.
		suite.mInbound.
			EXPECT().
			IsRunning().
			Return(false),

		// Detector returns a real host.
		suite.detector.
			EXPECT().
			HostPort().
			Return(_hostPort),

		// StartMesosLoop returns an error.
		suite.mInbound.
			EXPECT().
			StartMesosLoop(gomock.Eq(_hostPort)).
			Return(nil, errFoo),

		// For stats gathering.
		suite.mInbound.
			EXPECT().
			IsRunning().
			Return(false).
			Times(2),
	)
	suite.server.ensureStateRound()
	suite.ctrl.Finish()
	suite.Equal(
		suite.server.minBackoff.Nanoseconds()*2,
		suite.server.currentBackoffNano.Load())
	suite.True(lower.UnixNano() < suite.server.backoffUntilNano.Load())
	suite.True(upper.UnixNano() > suite.server.backoffUntilNano.Load())
}

// Tests that backoff caps at maximum.
func (suite *ServerTestSuite) TestMaxBackoff() {
	now := time.Now()
	lower := now.Add(suite.server.maxBackoff)
	upper := lower.Add(suite.server.minBackoff)

	suite.server.elected.Store(true)
	suite.server.handlersRunning.Store(false)
	suite.server.currentBackoffNano.Store(
		suite.server.maxBackoff.Nanoseconds() - 1)
	suite.server.backoffUntilNano.Store(now.UnixNano())

	gomock.InOrder(
		// Initial check for Mesos connection.
		suite.mInbound.
			EXPECT().
			IsRunning().
			Return(false),

		// Detector returns a real host.
		suite.detector.
			EXPECT().
			HostPort().
			Return(_hostPort),

		// StartMesosLoop returns an error.
		suite.mInbound.
			EXPECT().
			StartMesosLoop(gomock.Eq(_hostPort)).
			Return(nil, errFoo),

		// For stats gathering.
		suite.mInbound.
			EXPECT().
			IsRunning().
			Return(false).
			Times(2),
	)
	suite.server.ensureStateRound()
	suite.ctrl.Finish()
	suite.Equal(
		suite.server.maxBackoff.Nanoseconds(),
		suite.server.currentBackoffNano.Load())
	suite.True(lower.UnixNano() < suite.server.backoffUntilNano.Load())
	suite.True(upper.UnixNano() > suite.server.backoffUntilNano.Load())
}

// Tests that we do not perform connection withinn backoff window.
func (suite *ServerTestSuite) TestEffectiveBackoff() {
	now := time.Now()
	future := now.Add(suite.server.minBackoff)

	suite.server.elected.Store(true)
	suite.server.handlersRunning.Store(false)
	suite.server.currentBackoffNano.Store(
		suite.server.minBackoff.Nanoseconds())
	suite.server.backoffUntilNano.Store(future.UnixNano())

	gomock.InOrder(
		// Initial check for Mesos connection.
		suite.mInbound.
			EXPECT().
			IsRunning().
			Return(false),

		// For stats gathering.
		suite.mInbound.
			EXPECT().
			IsRunning().
			Return(false).
			Times(2),
	)
	suite.server.ensureStateRound()
	suite.ctrl.Finish()
	suite.Equal(
		suite.server.minBackoff.Nanoseconds(),
		suite.server.currentBackoffNano.Load())
	suite.Equal(future.UnixNano(), suite.server.backoffUntilNano.Load())
}

func TestServerTestSuite(t *testing.T) {
	suite.Run(t, new(ServerTestSuite))
}
