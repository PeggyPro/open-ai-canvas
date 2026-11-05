package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"yingce/backend/internal/model"
	"yingce/backend/internal/repository"
)

type taskTerminalRepositoryStub struct {
	task             *model.Task
	taskError        error
	terminalCalls    int
	terminalError    error
	terminalConflict bool
}

func (r *taskTerminalRepositoryStub) Task(string) (*model.Task, error) {
	if r.taskError != nil {
		return nil, r.taskError
	}
	if r.task == nil {
		return nil, errors.New("task not found")
	}
	copy := *r.task
	return &copy, nil
}

func (r *taskTerminalRepositoryStub) UpdateTaskTerminalDiagnostic(task *model.Task, completedAt time.Time) (bool, error) {
	r.terminalCalls++
	if r.terminalConflict {
		return false, nil
	}
	if r.terminalError != nil {
		return false, r.terminalError
	}
	if r.task != nil {
		*r.task = *task
		r.task.CompletedAt = &completedAt
	}
	return true, nil
}

func (r *taskTerminalRepositoryStub) UpdateTaskExecutionDiagnostic(_ string, _ string, diagnosticJSON string) error {
	if r.task != nil {
		r.task.ExecutionDiagnosticJSON = diagnosticJSON
		var diagnostic model.TaskExecutionDiagnostic
		if err := json.Unmarshal([]byte(diagnosticJSON), &diagnostic); err == nil {
			r.task.Diagnostic = &diagnostic
		}
	}
	return nil
}

func TestTaskTerminalConflictDoesNotRefundOrFinalize(t *testing.T) {
	repo := &taskTerminalRepositoryStub{terminalConflict: true}
	billing, replay := &taskTerminalBillingStub{}, &taskTerminalReplayStub{}
	logger := &taskTerminalLoggerStub{}
	c := newTaskTerminalCoordinatorForTest(repo, billing, replay, logger, &taskTerminalOutputStub{})
	task := &model.Task{ID: "task", LeaseOwner: "stale", Status: model.TaskStatusRunning}
	err := c.handleExecutionFailure(task, errors.New("upstream failed"), false, false)
	if !errors.Is(err, repository.ErrTaskStateConflict) {
		t.Fatalf("missing conflict: %v", err)
	}
	if len(billing.refund)+len(billing.uncertain)+billing.settleCalls+len(replay.statuses) != 0 {
		t.Fatal("stale worker performed terminal side effects")
	}
}

type taskTerminalBillingStub struct {
	uncertain      []string
	release        []string
	refund         []string
	settleCalls    int
	review         bool
	settleError    error
	uncertainError error
	releaseError   error
	refundError    error
}

func (b *taskTerminalBillingStub) MarkBillingUncertain(_ string, reason string) error {
	b.uncertain = append(b.uncertain, reason)
	return b.uncertainError
}

func (b *taskTerminalBillingStub) ReleaseBillingReservation(_ string, reason string) error {
	b.release = append(b.release, reason)
	return b.releaseError
}

func (b *taskTerminalBillingStub) RefundBilling(_ string, reason string) error {
	b.refund = append(b.refund, reason)
	return b.refundError
}

func (b *taskTerminalBillingStub) BillingFailureRequiresReview(string, string, error) bool {
	return b.review
}

func (b *taskTerminalBillingStub) SettleBilling(string, string) error {
	b.settleCalls++
	return b.settleError
}

type taskTerminalReplayStub struct {
	statuses []model.TaskStatus
}

func (r *taskTerminalReplayStub) finalizeTaskTextReplay(_ string, status model.TaskStatus) error {
	r.statuses = append(r.statuses, status)
	return nil
}

type taskTerminalLoggerStub struct {
	messages []string
}

func (l *taskTerminalLoggerStub) log(_ string, _ string, _ string, message string, _ string) error {
	l.messages = append(l.messages, message)
	return nil
}

type taskTerminalOutputStub struct {
	calls int
	err   error
}

func (o *taskTerminalOutputStub) RegisterTaskOutputFromTask(model.Task) error {
	o.calls++
	return o.err
}

func newTaskTerminalCoordinatorForTest(repo taskTerminalRepository, billing *taskTerminalBillingStub, replay *taskTerminalReplayStub, logger *taskTerminalLoggerStub, outputs *taskTerminalOutputStub) *taskTerminalCoordinator {
	return &taskTerminalCoordinator{
		repo:              repo,
		billing:           billing,
		replay:            replay,
		logger:            logger,
		outputs:           outputs,
		userFacingMessage: func(err error) string { return "public: " + err.Error() },
	}
}

func TestTaskTerminalCoordinatorHandlesCancellation(t *testing.T) {
	task := &model.Task{ID: "task-1", UserID: "user-1", BillingOrderID: "order-1"}
	repo := &taskTerminalRepositoryStub{task: task}
	billing := &taskTerminalBillingStub{}
	replay := &taskTerminalReplayStub{}
	logger := &taskTerminalLoggerStub{}
	coordinator := newTaskTerminalCoordinatorForTest(repo, billing, replay, logger, &taskTerminalOutputStub{})

	if err := coordinator.handleExecutionFailure(task, context.Canceled, false, false); err != nil {
		t.Fatalf("handleExecutionFailure() error = %v", err)
	}
	if task.Status != model.TaskStatusCancelled || task.Error != "任务已取消" {
		t.Fatalf("unexpected cancelled task state: status=%s error=%q", task.Status, task.Error)
	}
	if len(billing.uncertain) != 1 || len(billing.refund) != 0 {
		t.Fatalf("unexpected billing actions: uncertain=%v refund=%v", billing.uncertain, billing.refund)
	}
	if len(replay.statuses) != 1 || replay.statuses[0] != model.TaskStatusCancelled {
		t.Fatalf("unexpected replay statuses: %v", replay.statuses)
	}
	if len(logger.messages) != 1 || repo.terminalCalls != 1 {
		t.Fatalf("expected cancellation side effects, replay=%v logs=%v terminalCalls=%d", replay.statuses, logger.messages, repo.terminalCalls)
	}
}

func TestTaskTerminalCoordinatorRefundsFailureBeforeProviderRequest(t *testing.T) {
	task := &model.Task{ID: "task-1", UserID: "user-1", BillingOrderID: "order-1"}
	repo := &taskTerminalRepositoryStub{task: task}
	billing := &taskTerminalBillingStub{}
	replay := &taskTerminalReplayStub{}
	logger := &taskTerminalLoggerStub{}
	coordinator := newTaskTerminalCoordinatorForTest(repo, billing, replay, logger, &taskTerminalOutputStub{})
	failure := errors.New("provider unavailable")

	if err := coordinator.handleExecutionFailure(task, failure, false, true); !errors.Is(err, failure) {
		t.Fatalf("handleExecutionFailure() error = %v, want %v", err, failure)
	}
	if task.Status != model.TaskStatusFailed || task.Error != "public: provider unavailable" {
		t.Fatalf("unexpected failed task state: status=%s error=%q", task.Status, task.Error)
	}
	if len(billing.refund) != 1 || len(billing.uncertain) != 0 {
		t.Fatalf("unexpected billing actions: uncertain=%v refund=%v", billing.uncertain, billing.refund)
	}
	if len(replay.statuses) != 1 || replay.statuses[0] != model.TaskStatusFailed {
		t.Fatalf("unexpected replay statuses: %v", replay.statuses)
	}
}

func TestTaskTerminalCoordinatorLogsUnrecordedProviderFailure(t *testing.T) {
	task := &model.Task{ID: "task-1", UserID: "user-1", BillingOrderID: "order-1"}
	coordinator := newTaskTerminalCoordinatorForTest(
		&taskTerminalRepositoryStub{task: task},
		&taskTerminalBillingStub{},
		&taskTerminalReplayStub{},
		&taskTerminalLoggerStub{},
		&taskTerminalOutputStub{},
	)
	var loggedTask model.Task
	var loggedErr error
	coordinator.logFailedAttempt = func(value model.Task, err error) {
		loggedTask = value
		loggedErr = err
	}
	failure := errors.New("provider preflight failed")

	if err := coordinator.handleExecutionFailure(task, failure, false, true); !errors.Is(err, failure) {
		t.Fatalf("handleExecutionFailure() error = %v, want %v", err, failure)
	}
	if loggedTask.ID != task.ID || !errors.Is(loggedErr, failure) {
		t.Fatalf("logged failure = task:%#v error:%v", loggedTask, loggedErr)
	}
}

func TestTaskTerminalCoordinatorReturnsTerminalStateWriteError(t *testing.T) {
	task := &model.Task{ID: "task-1", UserID: "user-1", BillingOrderID: "order-1"}
	terminalError := errors.New("database unavailable")
	repo := &taskTerminalRepositoryStub{task: task, terminalError: terminalError}
	coordinator := newTaskTerminalCoordinatorForTest(
		repo,
		&taskTerminalBillingStub{},
		&taskTerminalReplayStub{},
		&taskTerminalLoggerStub{},
		&taskTerminalOutputStub{},
	)

	providerError := errors.New("provider unavailable")
	if err := coordinator.handleExecutionFailure(task, providerError, false, true); !errors.Is(err, providerError) || !errors.Is(err, terminalError) {
		t.Fatalf("handleExecutionFailure() error = %v, want provider and terminal errors", err)
	}
}

func TestTaskTerminalCoordinatorReturnsBillingRefundError(t *testing.T) {
	task := &model.Task{ID: "task-1", UserID: "user-1", BillingOrderID: "order-1"}
	billingError := errors.New("billing database unavailable")
	billing := &taskTerminalBillingStub{refundError: billingError}
	coordinator := newTaskTerminalCoordinatorForTest(
		&taskTerminalRepositoryStub{task: task},
		billing,
		&taskTerminalReplayStub{},
		&taskTerminalLoggerStub{},
		&taskTerminalOutputStub{},
	)

	providerError := errors.New("provider unavailable")
	if err := coordinator.handleExecutionFailure(task, providerError, false, true); !errors.Is(err, providerError) || !errors.Is(err, billingError) {
		t.Fatalf("handleExecutionFailure() error = %v, want provider and billing errors", err)
	}
}

func TestTaskTerminalCoordinatorReturnsOutputRegistrationErrorAfterSuccess(t *testing.T) {
	task := &model.Task{ID: "task-1", UserID: "user-1", BillingOrderID: "order-1"}
	outputError := errors.New("project output unavailable")
	outputs := &taskTerminalOutputStub{err: outputError}
	billing := &taskTerminalBillingStub{}
	coordinator := newTaskTerminalCoordinatorForTest(
		&taskTerminalRepositoryStub{task: task},
		billing,
		&taskTerminalReplayStub{},
		&taskTerminalLoggerStub{},
		outputs,
	)

	if err := coordinator.handleSuccess(task); !errors.Is(err, outputError) {
		t.Fatalf("handleSuccess() error = %v, want %v", err, outputError)
	}
	if outputs.calls != 1 || billing.settleCalls != 1 {
		t.Fatalf("expected output registration and billing settlement, calls=%d settle=%d", outputs.calls, billing.settleCalls)
	}
}

func TestTaskTerminalCoordinatorReturnsTaskReadErrorAfterSuccess(t *testing.T) {
	task := &model.Task{ID: "task-1", UserID: "user-1", BillingOrderID: "order-1"}
	taskError := errors.New("task database unavailable")
	billing := &taskTerminalBillingStub{}
	coordinator := newTaskTerminalCoordinatorForTest(
		&taskTerminalRepositoryStub{taskError: taskError},
		billing,
		&taskTerminalReplayStub{},
		&taskTerminalLoggerStub{},
		&taskTerminalOutputStub{},
	)

	if err := coordinator.handleSuccess(task); !errors.Is(err, taskError) {
		t.Fatalf("handleSuccess() error = %v, want %v", err, taskError)
	}
	if billing.settleCalls != 1 {
		t.Fatalf("billing settlement calls = %d, want 1", billing.settleCalls)
	}
}

func TestTaskTerminalCoordinatorConsumesAudioOnPlatformFailureAndMarksFreeRetry(t *testing.T) {
	task := &model.Task{
		ID: "audio-task", UserID: "user-1", Type: "canvas_audio", WorkbenchKind: model.WorkbenchKindAudio,
		BillingOrderID: "order-1",
	}
	billing := &taskTerminalBillingStub{}
	coordinator := newTaskTerminalCoordinatorForTest(
		&taskTerminalRepositoryStub{task: task}, billing, &taskTerminalReplayStub{},
		&taskTerminalLoggerStub{}, &taskTerminalOutputStub{},
	)
	failure := providerHTTPError{StatusCode: 503, Status: "503 Service Unavailable"}

	if err := coordinator.handleExecutionFailure(task, failure, false, false); !errors.Is(err, failure) {
		t.Fatalf("handleExecutionFailure() error = %v, want %v", err, failure)
	}
	if billing.settleCalls != 1 || len(billing.refund) != 0 {
		t.Fatalf("audio billing actions: settle=%d refund=%v", billing.settleCalls, billing.refund)
	}
	if task.Diagnostic == nil || task.Diagnostic.RetryClass != "platform_retry" {
		t.Fatalf("diagnostic = %#v, want platform_retry", task.Diagnostic)
	}
}

func TestTaskTerminalCoordinatorDoesNotGrantFreeRetryForAudioUserError(t *testing.T) {
	task := &model.Task{
		ID: "audio-task", UserID: "user-1", Type: "canvas_audio", WorkbenchKind: model.WorkbenchKindAudio,
		BillingOrderID: "order-1",
	}
	billing := &taskTerminalBillingStub{}
	coordinator := newTaskTerminalCoordinatorForTest(
		&taskTerminalRepositoryStub{task: task}, billing, &taskTerminalReplayStub{},
		&taskTerminalLoggerStub{}, &taskTerminalOutputStub{},
	)
	failure := providerHTTPError{StatusCode: 422, Status: "422 Unprocessable Entity"}

	if err := coordinator.handleExecutionFailure(task, failure, false, false); !errors.Is(err, failure) {
		t.Fatalf("handleExecutionFailure() error = %v, want %v", err, failure)
	}
	if billing.settleCalls != 1 || len(billing.refund) != 0 {
		t.Fatalf("audio billing actions: settle=%d refund=%v", billing.settleCalls, billing.refund)
	}
	if task.Diagnostic == nil || task.Diagnostic.RetryClass == "platform_retry" {
		t.Fatalf("diagnostic = %#v, must not grant platform_retry", task.Diagnostic)
	}
}

func TestAudioPreRequestFailureReleasesReservationAndAllowsPlatformRetry(t *testing.T) {
	task := &model.Task{
		ID: "audio-task", UserID: "user-1", Type: "canvas_audio", WorkbenchKind: model.WorkbenchKindAudio,
		BillingOrderID: "order-1",
	}
	billing := &taskTerminalBillingStub{}
	coordinator := newTaskTerminalCoordinatorForTest(
		&taskTerminalRepositoryStub{task: task}, billing, &taskTerminalReplayStub{},
		&taskTerminalLoggerStub{}, &taskTerminalOutputStub{},
	)
	failure := providerRequestNotStartedError{Err: errors.New("provider connection refused")}

	if err := coordinator.handleExecutionFailure(task, failure, false, false); !errors.Is(err, failure) {
		t.Fatalf("handleExecutionFailure() error = %v, want %v", err, failure)
	}
	if len(billing.release) != 1 || billing.settleCalls != 0 || len(billing.refund) != 0 {
		t.Fatalf("audio pre-request billing actions: release=%v settle=%d refund=%v", billing.release, billing.settleCalls, billing.refund)
	}
	if task.Diagnostic == nil || task.Diagnostic.RetryClass != "platform_retry" {
		t.Fatalf("diagnostic = %#v, want platform_retry", task.Diagnostic)
	}
}

func TestAudioBillingReleaseFailureBlocksRetryWithReconciliationDiagnostic(t *testing.T) {
	task := &model.Task{
		ID: "audio-task", UserID: "user-1", Type: "canvas_audio", WorkbenchKind: model.WorkbenchKindAudio,
		BillingOrderID: "order-1",
	}
	repo := &taskTerminalRepositoryStub{task: task}
	billing := &taskTerminalBillingStub{releaseError: errors.New("billing database unavailable")}
	coordinator := newTaskTerminalCoordinatorForTest(
		repo, billing, &taskTerminalReplayStub{}, &taskTerminalLoggerStub{}, &taskTerminalOutputStub{},
	)
	failure := providerRequestNotStartedError{Err: errors.New("provider connection refused")}

	if err := coordinator.handleExecutionFailure(task, failure, false, false); err == nil {
		t.Fatal("handleExecutionFailure() error = nil, want billing cleanup failure")
	}
	if len(billing.release) != 1 || len(billing.uncertain) != 1 {
		t.Fatalf("billing cleanup actions: release=%v uncertain=%v", billing.release, billing.uncertain)
	}
	if diagnostic := taskExecutionDiagnostic(repo.task); diagnostic == nil || diagnostic.RetryClass != "reconcile_submission" {
		t.Fatalf("persisted diagnostic = %#v, want reconcile_submission", diagnostic)
	}
}

func TestAudioPreRequestUserErrorReleasesReservationWithoutFreeRetry(t *testing.T) {
	task := &model.Task{
		ID: "audio-task", UserID: "user-1", Type: "canvas_audio", WorkbenchKind: model.WorkbenchKindAudio,
		BillingOrderID: "order-1",
	}
	billing := &taskTerminalBillingStub{}
	coordinator := newTaskTerminalCoordinatorForTest(
		&taskTerminalRepositoryStub{task: task}, billing, &taskTerminalReplayStub{},
		&taskTerminalLoggerStub{}, &taskTerminalOutputStub{},
	)
	failure := providerRequestNotStartedError{Err: errors.New("invalid parameter")}

	if err := coordinator.handleExecutionFailure(task, failure, false, false); !errors.Is(err, failure) {
		t.Fatalf("handleExecutionFailure() error = %v, want %v", err, failure)
	}
	if len(billing.release) != 1 || billing.settleCalls != 0 || len(billing.refund) != 0 {
		t.Fatalf("audio pre-request user-error billing actions: release=%v settle=%d refund=%v", billing.release, billing.settleCalls, billing.refund)
	}
	if task.Diagnostic == nil || task.Diagnostic.RetryClass == "platform_retry" {
		t.Fatalf("diagnostic = %#v, must not grant platform_retry", task.Diagnostic)
	}
}

func TestPreparationPlatformFailureAllowsAudioFreeRetry(t *testing.T) {
	task := &model.Task{
		ID: "audio-task", UserID: "user-1", Type: "canvas_audio", WorkbenchKind: model.WorkbenchKindAudio,
		BillingOrderID: "order-1",
	}
	repo := &taskTerminalRepositoryStub{task: task}
	billing := &taskTerminalBillingStub{}
	coordinator := newTaskTerminalCoordinatorForTest(
		repo, billing, &taskTerminalReplayStub{}, &taskTerminalLoggerStub{}, &taskTerminalOutputStub{},
	)
	failure := errors.New("runtime policy store unavailable")

	if err := coordinator.markPreparationFailure(task, "运行策略读取失败", failure, false, "运行策略读取失败，上游请求未发出", true); !errors.Is(err, failure) {
		t.Fatalf("markPreparationFailure() error = %v, want %v", err, failure)
	}
	if len(billing.release) != 1 || billing.settleCalls != 0 || len(billing.refund) != 0 {
		t.Fatalf("preparation platform billing actions: release=%v settle=%d refund=%v", billing.release, billing.settleCalls, billing.refund)
	}
	if task.Diagnostic == nil || task.Diagnostic.RetryClass != "platform_retry" {
		t.Fatalf("diagnostic = %#v, want platform_retry", task.Diagnostic)
	}
}

func TestAudioPlatformRetryClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "unauthorized", err: providerHTTPError{StatusCode: 401}, want: false},
		{name: "forbidden", err: providerHTTPError{StatusCode: 403}, want: false},
		{name: "not found", err: providerHTTPError{StatusCode: 404}, want: false},
		{name: "provider rate limited", err: providerHTTPError{StatusCode: 429}, want: true},
		{name: "request timeout remains billing-uncertain", err: providerHTTPError{StatusCode: 408}, want: false},
		{name: "user quota rate limited", err: providerHTTPError{StatusCode: 429, Body: "quota exceeded"}, want: false},
		{name: "server error", err: providerHTTPError{StatusCode: 503}, want: true},
		{name: "site overloaded", err: providerHTTPError{StatusCode: 529}, want: true},
		{name: "bad request", err: providerHTTPError{StatusCode: 400}, want: false},
		{name: "unprocessable", err: providerHTTPError{StatusCode: 422}, want: false},
		{name: "gateway timeout", err: providerHTTPError{StatusCode: 524}, want: false},
		{name: "moderation payload", err: providerHTTPError{StatusCode: 503, Body: `{"code":"sensitive_words_detected"}`}, want: false},
		{name: "invalid parameter payload", err: providerHTTPError{StatusCode: 503, Body: `{"code":"invalid_parameter"}`}, want: false},
		{name: "authentication service outage", err: providerHTTPError{StatusCode: 503, Body: "authentication service unavailable"}, want: true},
		{name: "quota service outage", err: providerHTTPError{StatusCode: 502, Body: "quota service unavailable"}, want: true},
		{name: "structured authentication service outage", err: providerPayloadError{raw: "authentication service unavailable", message: "渠道认证服务暂不可用"}, want: true},
		{name: "structured quota service outage", err: providerPayloadError{raw: "quota service unavailable", message: "渠道额度服务暂不可用"}, want: true},
		{name: "structured user quota error", err: providerPayloadError{raw: "quota exceeded", message: "渠道余额不足"}, want: false},
		{name: "structured invalid parameter error", err: providerPayloadError{raw: "invalid parameter", message: "参数无效"}, want: false},
		{name: "invalid input message", err: errors.New("provider rejected invalid input"), want: false},
		{name: "invalid input chinese message", err: errors.New("输入无效"), want: false},
		{name: "invalid upstream response", err: errors.New("invalid upstream response"), want: true},
		{name: "load balancer outage", err: errors.New("load balancer unavailable"), want: true},
		{name: "upload response missing id", err: providerExecutionPhaseError{Phase: "upload", Err: errors.New("上游素材上传没有返回素材 ID")}, want: true},
		{name: "ambiguous timeout", err: errors.New("upstream timeout; request may have been accepted"), want: false},
		{name: "protocol outage", err: errors.New("provider connection refused"), want: true},
		{name: "wrapped circuit outage", err: fmt.Errorf("route failed: %w", providerCircuitOpenError{}), want: true},
		{name: "undecodable provider response", err: providerResponseDecodeError{Err: errors.New("invalid character '<' looking for beginning of value")}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := audioPlatformRetryAllowed(test.err); got != test.want {
				t.Fatalf("audioPlatformRetryAllowed(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}
