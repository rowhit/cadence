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

package history

import (
	"fmt"
	"time"

	"github.com/uber-common/bark"
	m "github.com/uber/cadence/.gen/go/matching"
	workflow "github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/client/matching"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/logging"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/persistence"
)

type (
	timerQueueActiveProcessorImpl struct {
		shard                   ShardContext
		historyService          *historyEngineImpl
		cache                   *historyCache
		timerTaskFilter         timerTaskFilter
		now                     timeNow
		logger                  bark.Logger
		metricsClient           metrics.Client
		currentClusterName      string
		matchingClient          matching.Client
		timerGate               LocalTimerGate
		timerQueueProcessorBase *timerQueueProcessorBase
		timerQueueAckMgr        timerQueueAckMgr
	}
)

func newTimerQueueActiveProcessor(shard ShardContext, historyService *historyEngineImpl, matchingClient matching.Client, logger bark.Logger) *timerQueueActiveProcessorImpl {
	currentClusterName := shard.GetService().GetClusterMetadata().GetCurrentClusterName()
	timeNow := func() time.Time {
		return shard.GetCurrentTime(currentClusterName)
	}
	logger = logger.WithFields(bark.Fields{
		logging.TagWorkflowCluster: currentClusterName,
	})
	timerTaskFilter := func(timer *persistence.TimerTaskInfo) (bool, error) {
		return verifyActiveTask(shard, logger, timer.DomainID, timer)
	}

	timerQueueAckMgr := newTimerQueueAckMgr(metrics.TimerActiveQueueProcessorScope, shard, historyService.metricsClient, currentClusterName, logger)
	retryableMatchingClient := matching.NewRetryableClient(matchingClient, common.CreateMatchingRetryPolicy(),
		common.IsWhitelistServiceTransientError)
	processor := &timerQueueActiveProcessorImpl{
		shard:              shard,
		historyService:     historyService,
		cache:              historyService.historyCache,
		timerTaskFilter:    timerTaskFilter,
		now:                timeNow,
		logger:             logger,
		matchingClient:     retryableMatchingClient,
		metricsClient:      historyService.metricsClient,
		currentClusterName: currentClusterName,
		timerGate:          NewLocalTimerGate(),
		timerQueueProcessorBase: newTimerQueueProcessorBase(
			metrics.TimerActiveQueueProcessorScope,
			shard,
			historyService,
			timerQueueAckMgr,
			timeNow,
			shard.GetConfig().TimerProcessorMaxPollRPS,
			shard.GetConfig().TimerProcessorStartDelay,
			logger,
		),
		timerQueueAckMgr: timerQueueAckMgr,
	}
	processor.timerQueueProcessorBase.timerProcessor = processor
	return processor
}

func newTimerQueueFailoverProcessor(shard ShardContext, historyService *historyEngineImpl, domainID string, standbyClusterName string,
	minLevel time.Time, matchingClient matching.Client, logger bark.Logger) *timerQueueActiveProcessorImpl {
	clusterName := shard.GetService().GetClusterMetadata().GetCurrentClusterName()
	timeNow := func() time.Time {
		// should use current cluster's time when doing domain failover
		return shard.GetCurrentTime(clusterName)
	}
	logger = logger.WithFields(bark.Fields{
		logging.TagWorkflowCluster: clusterName,
		logging.TagDomainID:        domainID,
		logging.TagFailover:        "from: " + standbyClusterName,
	})
	timerTaskFilter := func(timer *persistence.TimerTaskInfo) (bool, error) {
		return verifyFailoverActiveTask(logger, domainID, timer.DomainID, timer)
	}

	timerQueueAckMgr := newTimerQueueFailoverAckMgr(shard, historyService.metricsClient, standbyClusterName, minLevel, logger)
	retryableMatchingClient := matching.NewRetryableClient(matchingClient, common.CreateMatchingRetryPolicy(),
		common.IsWhitelistServiceTransientError)
	processor := &timerQueueActiveProcessorImpl{
		shard:           shard,
		historyService:  historyService,
		cache:           historyService.historyCache,
		timerTaskFilter: timerTaskFilter,
		now:             timeNow,
		logger:          logger,
		metricsClient:   historyService.metricsClient,
		matchingClient:  retryableMatchingClient,
		timerGate:       NewLocalTimerGate(),
		timerQueueProcessorBase: newTimerQueueProcessorBase(
			metrics.TimerActiveQueueProcessorScope,
			shard,
			historyService,
			timerQueueAckMgr,
			timeNow,
			shard.GetConfig().TimerProcessorFailoverMaxPollRPS,
			shard.GetConfig().TimerProcessorFailoverStartDelay,
			logger,
		),
		timerQueueAckMgr: timerQueueAckMgr,
	}
	processor.timerQueueProcessorBase.timerProcessor = processor
	return processor
}

func (t *timerQueueActiveProcessorImpl) Start() {
	t.timerQueueProcessorBase.Start()
}

func (t *timerQueueActiveProcessorImpl) Stop() {
	t.timerGate.Close()
	t.timerQueueProcessorBase.Stop()
}

func (t *timerQueueActiveProcessorImpl) getTimerFiredCount() uint64 {
	return t.timerQueueProcessorBase.getTimerFiredCount()
}

func (t *timerQueueActiveProcessorImpl) getTimerGate() TimerGate {
	return t.timerGate
}

// NotifyNewTimers - Notify the processor about the new active timer events arrival.
// This should be called each time new timer events arrives, otherwise timers maybe fired unexpected.
func (t *timerQueueActiveProcessorImpl) notifyNewTimers(timerTasks []persistence.Task) {
	t.timerQueueProcessorBase.notifyNewTimers(timerTasks)
}

func (t *timerQueueActiveProcessorImpl) process(timerTask *persistence.TimerTaskInfo) error {
	ok, err := t.timerTaskFilter(timerTask)
	if err != nil {
		return err
	} else if !ok {
		t.timerQueueAckMgr.completeTimerTask(timerTask)
		t.logger.Debugf("Discarding timer: (%v, %v), for WorkflowID: %v, RunID: %v, Type: %v, EventID: %v, Error: %v",
			timerTask.TaskID, timerTask.VisibilityTimestamp, timerTask.WorkflowID, timerTask.RunID, timerTask.TaskType, timerTask.EventID, err)
		return nil
	}

	scope := metrics.TimerActiveQueueProcessorScope
	switch timerTask.TaskType {
	case persistence.TaskTypeUserTimer:
		scope = metrics.TimerActiveTaskUserTimerScope
		err = t.processExpiredUserTimer(timerTask)

	case persistence.TaskTypeActivityTimeout:
		scope = metrics.TimerActiveTaskActivityTimeoutScope
		err = t.processActivityTimeout(timerTask)

	case persistence.TaskTypeDecisionTimeout:
		scope = metrics.TimerActiveTaskDecisionTimeoutScope
		err = t.processDecisionTimeout(timerTask)

	case persistence.TaskTypeWorkflowTimeout:
		scope = metrics.TimerActiveTaskWorkflowTimeoutScope
		err = t.processWorkflowTimeout(timerTask)

	case persistence.TaskTypeRetryTimer:
		scope = metrics.TimerActiveTaskRetryTimerScope
		err = t.processRetryTimer(timerTask)

	case persistence.TaskTypeDeleteHistoryEvent:
		scope = metrics.TimerActiveTaskDeleteHistoryEvent
		err = t.timerQueueProcessorBase.processDeleteHistoryEvent(timerTask)
	}

	t.logger.Debugf("Processing timer: (%v, %v), for WorkflowID: %v, RunID: %v, Type: %v, EventID: %v, Error: %v",
		timerTask.TaskID, timerTask.VisibilityTimestamp, timerTask.WorkflowID, timerTask.RunID, timerTask.TaskType, timerTask.EventID, err)

	if err != nil {
		if _, ok := err.(*workflow.EntityNotExistsError); ok {
			// Timer could fire after the execution is deleted.
			// In which case just ignore the error so we can complete the timer task.
			t.timerQueueAckMgr.completeTimerTask(timerTask)
			err = nil
		}
		if err != nil {
			t.metricsClient.IncCounter(scope, metrics.TaskFailures)
		}
	} else {
		t.timerQueueAckMgr.completeTimerTask(timerTask)
	}

	return err
}

func (t *timerQueueActiveProcessorImpl) processExpiredUserTimer(task *persistence.TimerTaskInfo) (retError error) {
	t.metricsClient.IncCounter(metrics.TimerActiveTaskUserTimerScope, metrics.TaskRequests)
	sw := t.metricsClient.StartTimer(metrics.TimerActiveTaskUserTimerScope, metrics.TaskLatency)
	defer sw.Stop()

	context, release, err0 := t.cache.getOrCreateWorkflowExecution(t.timerQueueProcessorBase.getDomainIDAndWorkflowExecution(task))
	if err0 != nil {
		return err0
	}
	defer func() { release(retError) }()

Update_History_Loop:
	for attempt := 0; attempt < conditionalRetryCount; attempt++ {
		msBuilder, err := loadMutableStateForTimerTask(context, task, t.metricsClient, t.logger)
		if err != nil {
			return err
		} else if msBuilder == nil || !msBuilder.IsWorkflowExecutionRunning() {
			return nil
		}
		tBuilder := t.historyService.getTimerBuilder(&context.workflowExecution)

		var timerTasks []persistence.Task
		scheduleNewDecision := false

	ExpireUserTimers:
		for _, td := range tBuilder.GetUserTimers(msBuilder) {
			hasTimer, ti := tBuilder.GetUserTimer(td.TimerID)
			if !hasTimer {
				t.logger.Debugf("Failed to find in memory user timer: %s", td.TimerID)
				return fmt.Errorf("Failed to find in memory user timer: %s", td.TimerID)
			}

			if isExpired := tBuilder.IsTimerExpired(td, task.VisibilityTimestamp); isExpired {
				// Add TimerFired event to history.
				if msBuilder.AddTimerFiredEvent(ti.StartedID, ti.TimerID) == nil {
					return errFailedToAddTimerFiredEvent
				}

				scheduleNewDecision = !msBuilder.HasPendingDecisionTask()
			} else {
				// See if we have next timer in list to be created.
				if !td.TaskCreated {
					nextTask := tBuilder.createNewTask(td)
					timerTasks = []persistence.Task{nextTask}

					// Update the task ID tracking the corresponding timer task.
					ti.TaskID = TimerTaskStatusCreated
					msBuilder.UpdateUserTimer(ti.TimerID, ti)
					defer t.notifyNewTimers(timerTasks)
				}

				// Done!
				break ExpireUserTimers
			}
		}

		// We apply the update to execution using optimistic concurrency.  If it fails due to a conflict than reload
		// the history and try the operation again.
		err = t.updateWorkflowExecution(context, msBuilder, scheduleNewDecision, false, timerTasks, nil)
		if err != nil {
			if err == ErrConflict {
				continue Update_History_Loop
			}
		}
		return err
	}
	return ErrMaxAttemptsExceeded
}

func (t *timerQueueActiveProcessorImpl) processActivityTimeout(timerTask *persistence.TimerTaskInfo) (retError error) {
	t.metricsClient.IncCounter(metrics.TimerActiveTaskActivityTimeoutScope, metrics.TaskRequests)
	sw := t.metricsClient.StartTimer(metrics.TimerActiveTaskActivityTimeoutScope, metrics.TaskLatency)
	defer sw.Stop()

	context, release, err0 := t.cache.getOrCreateWorkflowExecution(t.timerQueueProcessorBase.getDomainIDAndWorkflowExecution(timerTask))
	if err0 != nil {
		return err0
	}
	defer func() { release(retError) }()
	referenceTime := t.now()

Update_History_Loop:
	for attempt := 0; attempt < conditionalRetryCount; attempt++ {
		msBuilder, err := loadMutableStateForTimerTask(context, timerTask, t.metricsClient, t.logger)
		if err != nil {
			return err
		} else if msBuilder == nil || !msBuilder.IsWorkflowExecutionRunning() {
			return nil
		}
		tBuilder := t.historyService.getTimerBuilder(&context.workflowExecution)

		ai, running := msBuilder.GetActivityInfo(timerTask.EventID)
		if running {
			// If current one is HB task then we may need to create the next heartbeat timer.  Clear the create flag for this
			// heartbeat timer so we can create it again if needed.
			// NOTE: When record activity HB comes in we only update last heartbeat timestamp, this is the place
			// where we create next timer task based on that new updated timestamp.
			isHeartBeatTask := timerTask.TimeoutType == int(workflow.TimeoutTypeHeartbeat)
			if isHeartBeatTask && ai.LastTimeoutVisibility <= timerTask.VisibilityTimestamp.Unix() {
				ai.TimerTaskStatus = ai.TimerTaskStatus &^ TimerTaskStatusCreatedHeartbeat
				msBuilder.UpdateActivity(ai)
			}

			// No need to check for attempt on the timer task.  ExpireActivityTimer logic below already checks if the
			// activity should be timedout and it will not let the timer expire for earlier attempts.  And creation of
			// duplicate timer task is protected by Created flag.
		}

		var timerTasks []persistence.Task
		updateHistory := false
		createNewTimer := false

	ExpireActivityTimers:
		for _, td := range tBuilder.GetActivityTimers(msBuilder) {
			ai, isRunning := msBuilder.GetActivityInfo(td.ActivityID)
			if !isRunning {
				//  We might have time out this activity already.
				continue ExpireActivityTimers
			}

			if isExpired := tBuilder.IsTimerExpired(td, referenceTime); isExpired {
				timeoutType := td.TimeoutType
				t.logger.Debugf("Activity TimeoutType: %v, scheduledID: %v, startedId: %v. \n",
					timeoutType, ai.ScheduleID, ai.StartedID)

				if td.Attempt < ai.Attempt && timeoutType != workflow.TimeoutTypeScheduleToClose {
					// retry could update ai.Attempt, and we should ignore further timeouts for previous attempt
					continue
				}

				if timeoutType != workflow.TimeoutTypeScheduleToStart {
					// ScheduleToStart (queue timeout) is not retriable. Instead of retry, customer should set larger
					// ScheduleToStart timeout.
					retryTask := msBuilder.CreateRetryTimer(ai, getTimeoutErrorReason(timeoutType))
					if retryTask != nil {
						timerTasks = append(timerTasks, retryTask)
						createNewTimer = true

						t.logger.Debugf("Ignore ActivityTimeout (%v) as retry is needed. New attempt: %v, retry backoff duration: %v.",
							timeoutType, ai.Attempt, retryTask.(*persistence.RetryTimerTask).VisibilityTimestamp.Sub(time.Now()))

						continue
					}
				}

				switch timeoutType {
				case workflow.TimeoutTypeScheduleToClose:
					{
						t.metricsClient.IncCounter(metrics.TimerActiveTaskActivityTimeoutScope, metrics.ScheduleToCloseTimeoutCounter)
						if msBuilder.AddActivityTaskTimedOutEvent(ai.ScheduleID, ai.StartedID, timeoutType, nil) == nil {
							return errFailedToAddTimeoutEvent
						}
						updateHistory = true
					}

				case workflow.TimeoutTypeStartToClose:
					{
						t.metricsClient.IncCounter(metrics.TimerActiveTaskActivityTimeoutScope, metrics.StartToCloseTimeoutCounter)
						if ai.StartedID != common.EmptyEventID {
							if msBuilder.AddActivityTaskTimedOutEvent(ai.ScheduleID, ai.StartedID, timeoutType, nil) == nil {
								return errFailedToAddTimeoutEvent
							}
							updateHistory = true
						}
					}

				case workflow.TimeoutTypeHeartbeat:
					{
						t.metricsClient.IncCounter(metrics.TimerActiveTaskActivityTimeoutScope, metrics.HeartbeatTimeoutCounter)
						if msBuilder.AddActivityTaskTimedOutEvent(ai.ScheduleID, ai.StartedID, timeoutType, ai.Details) == nil {
							return errFailedToAddTimeoutEvent
						}
						updateHistory = true
					}

				case workflow.TimeoutTypeScheduleToStart:
					{
						t.metricsClient.IncCounter(metrics.TimerActiveTaskActivityTimeoutScope, metrics.ScheduleToStartTimeoutCounter)
						if ai.StartedID == common.EmptyEventID {
							if msBuilder.AddActivityTaskTimedOutEvent(ai.ScheduleID, ai.StartedID, timeoutType, nil) == nil {
								return errFailedToAddTimeoutEvent
							}
							updateHistory = true
						}
					}
				}
			} else {
				// See if we have next timer in list to be created.
				// Create next timer task if we don't have one
				if !td.TaskCreated {
					nextTask := tBuilder.createNewTask(td)
					timerTasks = append(timerTasks, nextTask)
					at := nextTask.(*persistence.ActivityTimeoutTask)

					ai.TimerTaskStatus = ai.TimerTaskStatus | getActivityTimerStatus(workflow.TimeoutType(at.TimeoutType))
					// Use second resolution for setting LastTimeoutVisibility, which is used for deduping heartbeat timer creation
					ai.LastTimeoutVisibility = td.TimerSequenceID.VisibilityTimestamp.Unix()
					msBuilder.UpdateActivity(ai)
					createNewTimer = true

					t.logger.Debugf("%s: Adding Activity Timeout: with timeout: %v sec, ExpiryTime: %s, TimeoutType: %v, EventID: %v",
						time.Now(), td.TimeoutSec, at.VisibilityTimestamp, td.TimeoutType.String(), at.EventID)
				}

				// Done!
				break ExpireActivityTimers
			}
		}

		if updateHistory || createNewTimer {
			// We apply the update to execution using optimistic concurrency.  If it fails due to a conflict than reload
			// the history and try the operation again.
			scheduleNewDecision := updateHistory && !msBuilder.HasPendingDecisionTask()
			err := t.updateWorkflowExecution(context, msBuilder, scheduleNewDecision, false, timerTasks, nil)
			if err != nil {
				if err == ErrConflict {
					continue Update_History_Loop
				}
			}

			t.notifyNewTimers(timerTasks)
			return nil
		}

		return nil
	}
	return ErrMaxAttemptsExceeded
}

func (t *timerQueueActiveProcessorImpl) processDecisionTimeout(task *persistence.TimerTaskInfo) (retError error) {
	t.metricsClient.IncCounter(metrics.TimerActiveTaskDecisionTimeoutScope, metrics.TaskRequests)
	sw := t.metricsClient.StartTimer(metrics.TimerActiveTaskDecisionTimeoutScope, metrics.TaskLatency)
	defer sw.Stop()

	context, release, err0 := t.cache.getOrCreateWorkflowExecution(t.timerQueueProcessorBase.getDomainIDAndWorkflowExecution(task))
	if err0 != nil {
		return err0
	}
	defer func() { release(retError) }()

Update_History_Loop:
	for attempt := 0; attempt < conditionalRetryCount; attempt++ {
		msBuilder, err := loadMutableStateForTimerTask(context, task, t.metricsClient, t.logger)
		if err != nil {
			return err
		} else if msBuilder == nil || !msBuilder.IsWorkflowExecutionRunning() {
			return nil
		}

		scheduleID := task.EventID
		di, found := msBuilder.GetPendingDecision(scheduleID)
		if !found {
			logging.LogDuplicateTransferTaskEvent(t.logger, persistence.TaskTypeDecisionTimeout, task.TaskID, scheduleID)
			return nil
		}
		ok, err := verifyTaskVersion(t.shard, t.logger, task.DomainID, di.Version, task.Version, task)
		if err != nil {
			return err
		} else if !ok {
			return nil
		}

		scheduleNewDecision := false
		switch task.TimeoutType {
		case int(workflow.TimeoutTypeStartToClose):
			t.metricsClient.IncCounter(metrics.TimerActiveTaskDecisionTimeoutScope, metrics.StartToCloseTimeoutCounter)
			if di.Attempt == task.ScheduleAttempt {
				// Add a decision task timeout event.
				msBuilder.AddDecisionTaskTimedOutEvent(scheduleID, di.StartedID)
				scheduleNewDecision = true
			}
		case int(workflow.TimeoutTypeScheduleToStart):
			t.metricsClient.IncCounter(metrics.TimerActiveTaskDecisionTimeoutScope, metrics.ScheduleToStartTimeoutCounter)
			// decision schedule to start timeout only apply to sticky decision
			// check if scheduled decision still pending and not started yet
			if di.Attempt == task.ScheduleAttempt && di.StartedID == common.EmptyEventID && msBuilder.IsStickyTaskListEnabled() {
				timeoutEvent := msBuilder.AddDecisionTaskScheduleToStartTimeoutEvent(scheduleID)
				if timeoutEvent == nil {
					// Unable to add DecisionTaskTimedout event to history
					return &workflow.InternalServiceError{Message: "Unable to add DecisionTaskScheduleToStartTimeout event to history."}
				}

				// reschedule decision, which will be on its original task list
				scheduleNewDecision = true
			}
		}

		if scheduleNewDecision {
			// We apply the update to execution using optimistic concurrency.  If it fails due to a conflict than reload
			// the history and try the operation again.
			err := t.updateWorkflowExecution(context, msBuilder, scheduleNewDecision, false, nil, nil)
			if err != nil {
				if err == ErrConflict {
					continue Update_History_Loop
				}
			}
			return err
		}

		return nil

	}
	return ErrMaxAttemptsExceeded
}

func (t *timerQueueActiveProcessorImpl) processRetryTimer(task *persistence.TimerTaskInfo) error {
	t.metricsClient.IncCounter(metrics.TimerActiveTaskRetryTimerScope, metrics.TaskRequests)
	sw := t.metricsClient.StartTimer(metrics.TimerActiveTaskRetryTimerScope, metrics.TaskLatency)
	defer sw.Stop()

	processFn := func() error {
		context, release, err0 := t.cache.getOrCreateWorkflowExecution(t.timerQueueProcessorBase.getDomainIDAndWorkflowExecution(task))
		defer release(nil)
		if err0 != nil {
			return err0
		}
		msBuilder, err := loadMutableStateForTimerTask(context, task, t.metricsClient, t.logger)
		if err != nil {
			return err
		} else if msBuilder == nil || !msBuilder.IsWorkflowExecutionRunning() {
			return nil
		}

		// generate activity task
		scheduledID := task.EventID
		ai, running := msBuilder.GetActivityInfo(scheduledID)
		if !running || task.ScheduleAttempt < int64(ai.Attempt) {
			return nil
		}
		ok, err := verifyTaskVersion(t.shard, t.logger, task.DomainID, ai.Version, task.Version, task)
		if err != nil {
			return err
		} else if !ok {
			return nil
		}

		domainID := task.DomainID
		targetDomainID := domainID
		scheduledEvent, _ := msBuilder.GetActivityScheduledEvent(scheduledID)
		if scheduledEvent.ActivityTaskScheduledEventAttributes.Domain != nil {
			domainEntry, err := t.shard.GetDomainCache().GetDomain(scheduledEvent.ActivityTaskScheduledEventAttributes.GetDomain())
			if err != nil {
				return &workflow.InternalServiceError{Message: "Unable to re-schedule activity across domain."}
			}
			targetDomainID = domainEntry.GetInfo().ID
		}

		execution := workflow.WorkflowExecution{
			WorkflowId: common.StringPtr(task.WorkflowID),
			RunId:      common.StringPtr(task.RunID)}
		taskList := &workflow.TaskList{
			Name: &ai.TaskList,
		}
		scheduleToStartTimeout := ai.ScheduleToStartTimeout

		release(nil) // release earlier as we don't need the lock anymore
		err = t.matchingClient.AddActivityTask(nil, &m.AddActivityTaskRequest{
			DomainUUID:                    common.StringPtr(targetDomainID),
			SourceDomainUUID:              common.StringPtr(domainID),
			Execution:                     &execution,
			TaskList:                      taskList,
			ScheduleId:                    &scheduledID,
			ScheduleToStartTimeoutSeconds: common.Int32Ptr(scheduleToStartTimeout),
		})

		t.logger.Debugf("Adding ActivityTask for retry, WorkflowID: %v, RunID: %v, ScheduledID: %v, TaskList: %v, Attempt: %v, Err: %v",
			task.WorkflowID, task.RunID, scheduledID, taskList.GetName(), task.ScheduleAttempt, err)

		return err
	}

	for attempt := 0; attempt < conditionalRetryCount; attempt++ {
		if err := processFn(); err == nil {
			return nil
		}
	}

	return ErrMaxAttemptsExceeded
}

func (t *timerQueueActiveProcessorImpl) processWorkflowTimeout(task *persistence.TimerTaskInfo) (retError error) {
	t.metricsClient.IncCounter(metrics.TimerActiveTaskWorkflowTimeoutScope, metrics.TaskRequests)
	sw := t.metricsClient.StartTimer(metrics.TimerActiveTaskWorkflowTimeoutScope, metrics.TaskLatency)
	defer sw.Stop()

	context, release, err0 := t.cache.getOrCreateWorkflowExecution(t.timerQueueProcessorBase.getDomainIDAndWorkflowExecution(task))
	if err0 != nil {
		return err0
	}
	defer func() { release(retError) }()

Update_History_Loop:
	for attempt := 0; attempt < conditionalRetryCount; attempt++ {
		msBuilder, err := loadMutableStateForTimerTask(context, task, t.metricsClient, t.logger)
		if err != nil {
			return err
		} else if msBuilder == nil || !msBuilder.IsWorkflowExecutionRunning() {
			return nil
		}

		// do version check for global domain task
		if msBuilder.GetReplicationState() != nil {
			ok, err := verifyTaskVersion(t.shard, t.logger, task.DomainID, msBuilder.GetReplicationState().StartVersion, task.Version, task)
			if err != nil {
				return err
			} else if !ok {
				return nil
			}
		}

		if e := msBuilder.AddTimeoutWorkflowEvent(); e == nil {
			// If we failed to add the event that means the workflow is already completed.
			// we drop this timeout event.
			return nil
		}

		// We apply the update to execution using optimistic concurrency.  If it fails due to a conflict than reload
		// the history and try the operation again.
		err = t.updateWorkflowExecution(context, msBuilder, false, true, nil, nil)
		if err != nil {
			if err == ErrConflict {
				continue Update_History_Loop
			}
		}
		return err
	}
	return ErrMaxAttemptsExceeded
}

func (t *timerQueueActiveProcessorImpl) updateWorkflowExecution(
	context *workflowExecutionContext,
	msBuilder mutableState,
	scheduleNewDecision bool,
	createDeletionTask bool,
	timerTasks []persistence.Task,
	clearTimerTask persistence.Task,
) error {
	executionInfo := msBuilder.GetExecutionInfo()
	var transferTasks []persistence.Task
	var err error
	if scheduleNewDecision {
		// Schedule a new decision.
		transferTasks, timerTasks, err = context.scheduleNewDecision(transferTasks, timerTasks)
		if err != nil {
			return err
		}
	}

	if createDeletionTask {
		tBuilder := t.historyService.getTimerBuilder(&context.workflowExecution)
		tranT, timerT, err := t.historyService.getDeleteWorkflowTasks(executionInfo.DomainID, tBuilder)
		if err != nil {
			return nil
		}
		transferTasks = append(transferTasks, tranT)
		timerTasks = append(timerTasks, timerT)
	}

	// Generate a transaction ID for appending events to history
	transactionID, err1 := t.historyService.shard.GetNextTransferTaskID()
	if err1 != nil {
		return err1
	}

	err = context.updateWorkflowExecutionWithDeleteTask(transferTasks, timerTasks, clearTimerTask, transactionID)
	if err != nil {
		if isShardOwnershiptLostError(err) {
			// Shard is stolen.  Stop timer processing to reduce duplicates
			t.timerQueueProcessorBase.Stop()
			return err
		}

		// Check if the processing is blocked due to limit exceeded error and fail any outstanding decision to
		// unblock processing
		if err == ErrBufferedEventsLimitExceeded {
			context.clear()

			var err1 error
			// Reload workflow execution so we can apply the decision task failure event
			msBuilder, err1 = context.loadWorkflowExecution()
			if err1 != nil {
				return err1
			}

			if di, ok := msBuilder.GetInFlightDecisionTask(); ok {
				msBuilder.AddDecisionTaskFailedEvent(di.ScheduleID, di.StartedID,
					workflow.DecisionTaskFailedCauseForceCloseDecision, nil, identityHistoryService)

				var transT, timerT []persistence.Task
				transT, timerT, err1 = context.scheduleNewDecision(transT, timerT)
				if err1 != nil {
					return err1
				}

				// Generate a transaction ID for appending events to history
				transactionID, err1 := t.historyService.shard.GetNextTransferTaskID()
				if err1 != nil {
					return err1
				}
				err1 = context.updateWorkflowExecution(transT, timerT, transactionID)
				if err1 != nil {
					return err1
				}
			}

			return err
		}
	}

	t.notifyNewTimers(timerTasks)
	return err
}
