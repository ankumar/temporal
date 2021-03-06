// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
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

package parentclosepolicy

import (
	"context"
	"time"

	"go.temporal.io/temporal"
	commonpb "go.temporal.io/temporal-proto/common"
	"go.temporal.io/temporal-proto/serviceerror"
	"go.temporal.io/temporal-proto/workflowservice"
	"go.temporal.io/temporal/activity"
	"go.temporal.io/temporal/workflow"

	"github.com/temporalio/temporal/.gen/proto/historyservice"
	"github.com/temporalio/temporal/common/log"
	"github.com/temporalio/temporal/common/log/tag"
	"github.com/temporalio/temporal/common/metrics"
)

const (
	processorContextKey = "processorContext"
	// processorTaskListName is the tasklist name
	processorTaskListName = "temporal-sys-processor-parent-close-policy"
	// processorWFTypeName is the workflow type
	processorWFTypeName   = "temporal-sys-parent-close-policy-workflow"
	processorActivityName = "temporal-sys-parent-close-policy-activity"
	infiniteDuration      = 20 * 365 * 24 * time.Hour
	processorChannelName  = "ParentClosePolicyProcessorChannelName"
)

type (
	// RequestDetail defines detail of each workflow to process
	RequestDetail struct {
		WorkflowID string
		RunID      string
		Policy     commonpb.ParentClosePolicy
	}

	// Request defines the request for parent close policy
	Request struct {
		Executions  []RequestDetail
		Namespace   string
		NamespaceID string
	}
)

var (
	retryPolicy = temporal.RetryPolicy{
		InitialInterval:    10 * time.Second,
		BackoffCoefficient: 1.7,
		MaximumInterval:    5 * time.Minute,
	}

	activityOptions = workflow.ActivityOptions{
		ScheduleToStartTimeout: time.Minute,
		StartToCloseTimeout:    5 * time.Minute,
		RetryPolicy:            &retryPolicy,
	}
)

// ProcessorWorkflow is the workflow that performs actions for ParentClosePolicy
func ProcessorWorkflow(ctx workflow.Context) error {
	requestCh := workflow.GetSignalChannel(ctx, processorChannelName)
	for {
		var request Request
		if !requestCh.ReceiveAsync(&request) {
			// no more request
			break
		}

		opt := workflow.WithActivityOptions(ctx, activityOptions)
		_ = workflow.ExecuteActivity(opt, processorActivityName, request).Get(ctx, nil)
	}
	return nil
}

// ProcessorActivity is activity for processing batch operation
func ProcessorActivity(ctx context.Context, request Request) error {
	processor := ctx.Value(processorContextKey).(*Processor)
	client := processor.clientBean.GetHistoryClient()
	for _, execution := range request.Executions {
		var err error
		switch execution.Policy {
		case commonpb.ParentClosePolicy_Abandon:
			//no-op
			continue
		case commonpb.ParentClosePolicy_Terminate:
			_, err = client.TerminateWorkflowExecution(nil, &historyservice.TerminateWorkflowExecutionRequest{
				NamespaceId: request.NamespaceID,
				TerminateRequest: &workflowservice.TerminateWorkflowExecutionRequest{
					Namespace: request.Namespace,
					WorkflowExecution: &commonpb.WorkflowExecution{
						WorkflowId: execution.WorkflowID,
						RunId:      execution.RunID,
					},
					Reason:   "by parent close policy",
					Identity: processorWFTypeName,
				},
			})
		case commonpb.ParentClosePolicy_RequestCancel:
			_, err = client.RequestCancelWorkflowExecution(nil, &historyservice.RequestCancelWorkflowExecutionRequest{
				NamespaceId: request.NamespaceID,
				CancelRequest: &workflowservice.RequestCancelWorkflowExecutionRequest{
					Namespace: request.Namespace,
					WorkflowExecution: &commonpb.WorkflowExecution{
						WorkflowId: execution.WorkflowID,
						RunId:      execution.RunID,
					},
					Identity: processorWFTypeName,
				},
			})
		}

		if err != nil {
			if _, ok := err.(*serviceerror.NotFound); ok {
				err = nil
			}
		}

		if err != nil {
			processor.metricsClient.IncCounter(metrics.ParentClosePolicyProcessorScope, metrics.ParentClosePolicyProcessorFailures)
			getActivityLogger(ctx).Error("failed to process parent close policy", tag.Error(err))
			return err
		}
		processor.metricsClient.IncCounter(metrics.ParentClosePolicyProcessorScope, metrics.ParentClosePolicyProcessorSuccess)
	}
	return nil
}

func getActivityLogger(ctx context.Context) log.Logger {
	processor := ctx.Value(processorContextKey).(*Processor)
	wfInfo := activity.GetInfo(ctx)
	return processor.logger.WithTags(
		tag.WorkflowID(wfInfo.WorkflowExecution.ID),
		tag.WorkflowRunID(wfInfo.WorkflowExecution.RunID),
		tag.WorkflowNamespace(wfInfo.WorkflowNamespace),
	)
}
