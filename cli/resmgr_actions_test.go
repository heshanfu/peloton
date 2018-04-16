package cli

import (
	"context"
	"fmt"
	"testing"

	"code.uber.internal/infra/peloton/.gen/peloton/api/task"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc"
	res_mocks "code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc/mocks"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/suite"
)

type resmgrActionsTestSuite struct {
	suite.Suite
	mockCtrl *gomock.Controller
	mockRes  *res_mocks.MockResourceManagerServiceYARPCClient
	ctx      context.Context
}

func TestResmgrActions(t *testing.T) {
	suite.Run(t, new(resmgrActionsTestSuite))
}

func (suite *resmgrActionsTestSuite) SetupSuite() {
	suite.mockCtrl = gomock.NewController(suite.T())
	suite.mockRes = res_mocks.NewMockResourceManagerServiceYARPCClient(suite.mockCtrl)
	suite.ctx = context.Background()
}

func (suite *resmgrActionsTestSuite) TearDownSuite() {
	suite.mockCtrl.Finish()
	suite.ctx.Done()
}

func (suite *resmgrActionsTestSuite) TestClient_GetActiveTasks() {
	c := Client{
		Debug:        false,
		resMgrClient: suite.mockRes,
		dispatcher:   nil,
		ctx:          suite.ctx,
	}

	pendingTasks := &resmgrsvc.GetActiveTasksResponse_TaskEntries{
		TaskEntry: make([]*resmgrsvc.GetActiveTasksResponse_TaskEntry, 1),
	}
	runningTasks := &resmgrsvc.GetActiveTasksResponse_TaskEntries{
		TaskEntry: make([]*resmgrsvc.GetActiveTasksResponse_TaskEntry, 2),
	}

	taskEntries := make(map[string]*resmgrsvc.GetActiveTasksResponse_TaskEntries)
	taskEntries[task.TaskState_PENDING.String()] = pendingTasks
	taskEntries[task.TaskState_RUNNING.String()] = runningTasks

	resp := &resmgrsvc.GetActiveTasksResponse{
		TasksByState: taskEntries,
	}

	suite.mockRes.EXPECT().
		GetActiveTasks(gomock.Any(), gomock.Any()).
		Return(resp, nil)

	err := c.ResMgrGetActiveTasks("", "", "")
	suite.NoError(err)

	suite.mockRes.EXPECT().
		GetActiveTasks(gomock.Any(), gomock.Any()).
		Return(nil, fmt.Errorf("fake res error"))

	err = c.ResMgrGetActiveTasks("", "", "")
	suite.Error(err)
}

func (suite *resmgrActionsTestSuite) TestClient_GetPendingTasks() {
	c := Client{
		Debug:        false,
		resMgrClient: suite.mockRes,
		dispatcher:   nil,
		ctx:          suite.ctx,
	}

	pGangs := make(map[string]*resmgrsvc.
		GetPendingTasksResponse_PendingGangs, 2)

	var pGang1 []*resmgrsvc.GetPendingTasksResponse_PendingGang
	pGang1 = append(
		pGang1,
		&resmgrsvc.GetPendingTasksResponse_PendingGang{
			TaskIDs: []string{"job-1-1", "job-1-2"},
		},
	)
	var pGang2 []*resmgrsvc.GetPendingTasksResponse_PendingGang
	pGang2 = append(
		pGang2,
		&resmgrsvc.GetPendingTasksResponse_PendingGang{
			TaskIDs: []string{"job-2-1", "job-2-2"},
		},
	)
	pGangs["pending"] = &resmgrsvc.GetPendingTasksResponse_PendingGangs{
		PendingGangs: pGang1,
	}
	pGangs["non-preemptible"] = &resmgrsvc.GetPendingTasksResponse_PendingGangs{
		PendingGangs: pGang1,
	}

	resp := &resmgrsvc.GetPendingTasksResponse{
		PendingGangsByQueue: pGangs,
	}

	suite.mockRes.EXPECT().
		GetPendingTasks(gomock.Any(), gomock.Any()).
		Return(resp, nil)

	err := c.ResMgrGetPendingTasks("respool-1", 10)
	suite.NoError(err)

	suite.mockRes.EXPECT().
		GetPendingTasks(gomock.Any(), gomock.Any()).
		Return(nil, fmt.Errorf("fake res error"))

	err = c.ResMgrGetPendingTasks("respool-1", 10)
	suite.Error(err)
}
