// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package scanner

import (
	"context"
	"time"

	"go.uber.org/cadence"
	"go.uber.org/cadence/activity"
	cclient "go.uber.org/cadence/client"
	"go.uber.org/cadence/workflow"

	"github.com/uber/cadence/common/log/tag"
	"github.com/uber/cadence/service/worker/scanner/executions"
	"github.com/uber/cadence/service/worker/scanner/history"
	"github.com/uber/cadence/service/worker/scanner/tasklist"
	"github.com/uber/cadence/service/worker/scanner/timers"
)

const (
	maxConcurrentActivityExecutionSize     = 10
	maxConcurrentDecisionTaskExecutionSize = 10
	infiniteDuration                       = 20 * 365 * 24 * time.Hour

	tlScannerWFID                 = "cadence-sys-tl-scanner"
	tlScannerWFTypeName           = "cadence-sys-tl-scanner-workflow"
	tlScannerTaskListName         = "cadence-sys-tl-scanner-tasklist-0"
	taskListScavengerActivityName = "cadence-sys-tl-scanner-scvg-activity"

	historyScannerWFID           = "cadence-sys-history-scanner"
	historyScannerWFTypeName     = "cadence-sys-history-scanner-workflow"
	historyScannerTaskListName   = "cadence-sys-history-scanner-tasklist-0"
	historyScavengerActivityName = "cadence-sys-history-scanner-scvg-activity"
)

var (
	tlScavengerHBInterval = 10 * time.Second

	activityRetryPolicy = cadence.RetryPolicy{
		InitialInterval:    10 * time.Second,
		BackoffCoefficient: 1.7,
		MaximumInterval:    5 * time.Minute,
		ExpirationInterval: infiniteDuration,
	}
	activityOptions = workflow.ActivityOptions{
		ScheduleToStartTimeout: 5 * time.Minute,
		StartToCloseTimeout:    infiniteDuration,
		HeartbeatTimeout:       5 * time.Minute,
		RetryPolicy:            &activityRetryPolicy,
	}
	tlScannerWFStartOptions = cclient.StartWorkflowOptions{
		ID:                           tlScannerWFID,
		TaskList:                     tlScannerTaskListName,
		ExecutionStartToCloseTimeout: 5 * 24 * time.Hour,
		WorkflowIDReusePolicy:        cclient.WorkflowIDReusePolicyAllowDuplicate,
		CronSchedule:                 "0 */12 * * *",
	}
	historyScannerWFStartOptions = cclient.StartWorkflowOptions{
		ID:                           historyScannerWFID,
		TaskList:                     historyScannerTaskListName,
		ExecutionStartToCloseTimeout: infiniteDuration,
		WorkflowIDReusePolicy:        cclient.WorkflowIDReusePolicyAllowDuplicate,
		CronSchedule:                 "0 */12 * * *",
	}
)

func init() {
	workflow.RegisterWithOptions(TaskListScannerWorkflow, workflow.RegisterOptions{Name: tlScannerWFTypeName})
	activity.RegisterWithOptions(TaskListScavengerActivity, activity.RegisterOptions{Name: taskListScavengerActivityName})

	workflow.RegisterWithOptions(HistoryScannerWorkflow, workflow.RegisterOptions{Name: historyScannerWFTypeName})
	activity.RegisterWithOptions(HistoryScavengerActivity, activity.RegisterOptions{Name: historyScavengerActivityName})

	workflow.RegisterWithOptions(executions.ConcreteScannerWorkflow, workflow.RegisterOptions{Name: executions.ConcreteExecutionsScannerWFTypeName})
	workflow.RegisterWithOptions(executions.CurrentScannerWorkflow, workflow.RegisterOptions{Name: executions.CurrentExecutionsScannerWFTypeName})
	workflow.RegisterWithOptions(executions.ConcreteFixerWorkflow, workflow.RegisterOptions{Name: executions.ConcreteExecutionsFixerWFTypeName})
	workflow.RegisterWithOptions(executions.CurrentFixerWorkflow, workflow.RegisterOptions{Name: executions.CurrentExecutionsFixerWFTypeName})
	workflow.RegisterWithOptions(timers.ScannerWorkflow, workflow.RegisterOptions{Name: timers.ScannerWFTypeName})
	workflow.RegisterWithOptions(timers.FixerWorkflow, workflow.RegisterOptions{Name: timers.FixerWFTypeName})
}

// TaskListScannerWorkflow is the workflow that runs the task-list scanner background daemon
func TaskListScannerWorkflow(
	ctx workflow.Context,
) error {

	future := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, activityOptions), taskListScavengerActivityName)
	return future.Get(ctx, nil)
}

// HistoryScannerWorkflow is the workflow that runs the history scanner background daemon
func HistoryScannerWorkflow(
	ctx workflow.Context,
) error {

	future := workflow.ExecuteActivity(
		workflow.WithActivityOptions(ctx, activityOptions),
		historyScavengerActivityName,
	)
	return future.Get(ctx, nil)
}

// HistoryScavengerActivity is the activity that runs history scavenger
func HistoryScavengerActivity(
	activityCtx context.Context,
) (history.ScavengerHeartbeatDetails, error) {

	ctx, err := getScannerContext(activityCtx)
	if err != nil {
		return history.ScavengerHeartbeatDetails{}, err
	}

	rps := ctx.cfg.ScannerPersistenceMaxQPS()
	res := ctx.resource

	hbd := history.ScavengerHeartbeatDetails{}
	if activity.HasHeartbeatDetails(activityCtx) {
		if err := activity.GetHeartbeatDetails(activityCtx, &hbd); err != nil {
			res.GetLogger().Error("Failed to recover from last heartbeat, start over from beginning", tag.Error(err))
		}
	}
	cache := res.GetDomainCache()
	scavenger := history.NewScavenger(
		res.GetHistoryManager(),
		rps,
		res.GetHistoryClient(),
		hbd,
		res.GetMetricsClient(),
		res.GetLogger(),
		ctx.cfg.MaxWorkflowRetentionInDays,
		cache,
	)
	return scavenger.Run(activityCtx)
}

// TaskListScavengerActivity is the activity that runs task list scavenger
func TaskListScavengerActivity(
	activityCtx context.Context,
) error {
	ctx, err := getScannerContext(activityCtx)
	if err != nil {
		return err
	}
	res := ctx.resource
	scavenger := tasklist.NewScavenger(
		activityCtx,
		res.GetTaskManager(),
		res.GetMetricsClient(),
		res.GetLogger(),
		&ctx.cfg.TaskListScannerOptions,
		res.GetDomainCache(),
	)

	res.GetLogger().Info("Starting task list scavenger")
	scavenger.Start()
	for scavenger.Alive() {
		activity.RecordHeartbeat(activityCtx)
		if activityCtx.Err() != nil {
			res.GetLogger().Info("activity context error, stopping scavenger", tag.Error(activityCtx.Err()))
			scavenger.Stop()
			return activityCtx.Err()
		}
		time.Sleep(tlScavengerHBInterval)
	}
	return nil
}
