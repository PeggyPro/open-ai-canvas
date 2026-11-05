package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"yingce/backend/internal/model"
	"yingce/backend/internal/repository"
)

// taskTerminalCoordinator 收敛任务进入终态后的业务策略。
//
// 任务执行本身仍由 Service 编排，但失败、取消和成功收尾必须保持一致，
// 不能散落在 provider/worker 分支中。
type taskTerminalCoordinator struct {
	repo              taskTerminalRepository
	billing           taskBillingLifecycle
	replay            taskReplayLifecycle
	logger            taskLifecycleLogger
	outputs           taskOutputLifecycle
	userFacingMessage func(error) string
	logFailedAttempt  func(model.Task, error)
}

type taskTerminalRepository interface {
	Task(id string) (*model.Task, error)
	UpdateTaskTerminalDiagnostic(task *model.Task, completedAt time.Time) (bool, error)
	UpdateTaskExecutionDiagnostic(userID, taskID, diagnosticJSON string) error
}

type taskBillingLifecycle interface {
	MarkBillingUncertain(orderID string, errorText string) error
	ReleaseBillingReservation(orderID string, reason string) error
	RefundBilling(orderID string, errorText string) error
	BillingFailureRequiresReview(orderID string, taskID string, err error) bool
	SettleBilling(orderID string, providerRequestID string) error
}

type taskReplayLifecycle interface {
	finalizeTaskTextReplay(taskID string, status model.TaskStatus) error
}

type taskLifecycleLogger interface {
	log(userID string, taskID string, level string, message string, payload string) error
}

type taskOutputLifecycle interface {
	RegisterTaskOutputFromTask(task model.Task) error
}

type audioDurationTaskBilling interface {
	SettleBillingWithAudioDuration(orderID string, providerRequestID string, durationMs int64) error
}

type billingOrderReader interface {
	BillingOrder(orderID string) (*model.BillingOrder, error)
}

type audioDurationTaskOutput interface {
	AudioOutputDurationMs(task model.Task) (int64, error)
}

type videoProcessingTaskSettlement interface {
	settleVideoProcessingBilling(task model.Task, providerRequestID string) (bool, error)
}

func newTaskTerminalCoordinator(s *Service) *taskTerminalCoordinator {
	return &taskTerminalCoordinator{
		repo:              s.repo,
		billing:           s.taskBilling(),
		replay:            s,
		logger:            s,
		outputs:           s,
		userFacingMessage: s.UserFacingErrorMessage,
		logFailedAttempt:  s.ensureFailedProviderAttemptLogged,
	}
}

func (s *Service) terminalCoordinator() *taskTerminalCoordinator {
	if s.taskTerminalCoordinator != nil {
		return s.taskTerminalCoordinator
	}
	// 部分单元测试直接构造 Service 字面量；延迟创建保持这些测试和内部工具兼容。
	return newTaskTerminalCoordinator(s)
}

func (c *taskTerminalCoordinator) markPreparationFailure(task *model.Task, stage string, err error, billingUncertain bool, refundReason string, platformRetry bool) error {
	c.ensureFailedAttemptLogged(task, err)
	task.Status = model.TaskStatusFailed
	task.Stage = stage
	task.Error = c.userFacingMessage(err)
	outcome, retryClass := "not_submitted", "correct_input"
	if platformRetry {
		retryClass = "platform_retry"
	}
	if billingUncertain {
		outcome, retryClass = "unknown", "reconcile_submission"
	}
	recordTaskDiagnostic(task, "preparation", "preparation_failed", outcome, retryClass, err)
	if terminalErr := c.markTerminalState(task); terminalErr != nil {
		return errors.Join(err, terminalErr)
	}
	var billingErr error
	if billingUncertain {
		billingErr = c.billing.MarkBillingUncertain(task.BillingOrderID, task.Error)
	} else if taskIsAudio(task, nil) {
		billingErr = c.billing.ReleaseBillingReservation(task.BillingOrderID, refundReason)
	} else {
		billingErr = c.billing.RefundBilling(task.BillingOrderID, refundReason)
	}
	if billingErr != nil {
		// A failed release/refund must never turn into an ordinary retry. Mark the
		// order for reconciliation when possible and persist a durable retry guard;
		// this also protects the path where the billing transaction's final state
		// is not known to the caller.
		if !billingUncertain && taskIsAudio(task, nil) {
			billingErr = errors.Join(billingErr, c.billing.MarkBillingUncertain(task.BillingOrderID, "音频预授权释放失败，费用状态待核对"))
		}
		billingErr = c.persistBillingCleanupFailure(task, billingErr)
		return errors.Join(err, fmt.Errorf("任务计费收尾失败：%w", billingErr))
	}
	return err
}

func (c *taskTerminalCoordinator) persistBillingCleanupFailure(task *model.Task, billingErr error) error {
	if billingErr == nil {
		return nil
	}
	diagnostic := taskExecutionDiagnostic(task)
	if diagnostic == nil {
		diagnostic = &model.TaskExecutionDiagnostic{
			SubmissionOutcome: "unknown",
			DiagnosticID:      task.ID,
		}
	}
	diagnostic.Phase = "billing"
	diagnostic.Code = "billing_cleanup_failed"
	diagnostic.RetryClass = "reconcile_submission"
	diagnostic.SafeMessage = safeTaskDiagnosticMessage(task.Error)
	raw, marshalErr := json.Marshal(diagnostic)
	if marshalErr != nil {
		return errors.Join(billingErr, marshalErr)
	}
	task.ExecutionDiagnosticJSON = string(raw)
	task.Diagnostic = diagnostic
	if diagnosticErr := c.repo.UpdateTaskExecutionDiagnostic(task.UserID, task.ID, task.ExecutionDiagnosticJSON); diagnosticErr != nil {
		return errors.Join(billingErr, diagnosticErr)
	}
	return billingErr
}

// handleExecutionFailure 返回 nil 仅表示取消已被正常收尾；普通失败仍返回原始错误，
// 让 worker 保留重试/监控所需的失败语义。
func audioPlatformRetryAllowed(err error) bool {
	if err == nil {
		return false
	}
	var settlementErr billingSettlementError
	if errors.As(err, &settlementErr) {
		return false
	}
	phase := providerExecutionPhase(err)
	// A local preflight failure is safe to retry only when it is not clearly
	// caused by the user's input or channel configuration. Upload failures are
	// handled below because their HTTP/decode evidence determines the cause.
	if providerRequestNotStarted(err) && phase == "" {
		message := strings.ToLower(err.Error())
		if providerErrorLooksUserCaused(message) {
			return false
		}
		return true
	}
	if billingFailureUncertain(err) && phase != "upload" {
		return false
	}
	var circuitErr providerCircuitOpenError
	if errors.As(err, &circuitErr) {
		return true
	}
	var decodeErr providerResponseDecodeError
	if errors.As(err, &decodeErr) {
		// Upload decoding happens before billable create and therefore cannot
		// leave an audio generation charge uncertain. Create decoding remains
		// conservative because the provider may already have accepted the job.
		return phase == "upload"
	}
	var payloadErr providerPayloadError
	if errors.As(err, &payloadErr) {
		raw := strings.ToLower(payloadErr.raw)
		// An explicit outage marker is authoritative even when the provider
		// mentions an authentication or quota subsystem in the same payload.
		if providerErrorLooksExplicitPlatformFailure(raw) {
			return true
		}
		if providerErrorLooksUserCaused(raw) {
			return false
		}
		return providerErrorLooksPlatformFailure(raw)
	}
	var httpErr providerHTTPError
	if errors.As(err, &httpErr) {
		body := strings.ToLower(httpErr.Body)
		// 400, 422, and Cloudflare 524 are user/error-boundary outcomes for
		// retry authorization even when the response body contains outage words.
		if httpErr.StatusCode == 400 || httpErr.StatusCode == 422 || httpErr.StatusCode == 524 {
			return false
		}
		if providerErrorLooksExplicitPlatformFailure(body) {
			return true
		}
		if providerErrorLooksUserCaused(body) {
			return false
		}
		return providerHTTPStatusAllowsPlatformRetry(httpErr.StatusCode)
	}
	message := strings.ToLower(err.Error())
	if providerErrorLooksExplicitPlatformFailure(message) {
		return true
	}
	if providerErrorLooksUserCaused(message) {
		return false
	}
	return providerErrorLooksPlatformFailure(message)
}

func providerHTTPStatusAllowsPlatformRetry(statusCode int) bool {
	switch statusCode {
	case 408, 429, 500, 501, 502, 503, 504, 505, 507, 508, 520, 521, 522, 523, 525, 526, 529:
		return true
	default:
		return false
	}
}

func providerErrorLooksUserCaused(message string) bool {
	if isContentModerationFailure(message) {
		return true
	}
	for _, marker := range []string{
		"invalid parameter", "invalid_argument", "invalid argument", "invalid input", "invalid request", "invalid prompt", "invalid model", "invalid api key", "invalid_api_key", "invalid credentials", "parameter", "argument", "bad request", "user input", "authentication", "unauthorized", "forbidden", "permission", "access denied", "api key", "quota", "insufficient", "balance", "billing", "model not found", "model does not exist", "not found", "不存在", "权限", "鉴权", "额度", "配额", "余额", "参数错误", "参数无效", "参数不合法", "输入错误", "输入无效",
	} {
		if providerErrorContainsMarker(message, marker) {
			return true
		}
	}
	return false
}

func providerErrorContainsMarker(message string, marker string) bool {
	if strings.ContainsAny(marker, " _-") {
		return strings.Contains(message, marker)
	}
	for _, token := range strings.FieldsFunc(message, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if token == marker {
			return true
		}
	}
	return false
}

func providerErrorLooksExplicitPlatformFailure(message string) bool {
	message = strings.ToLower(message)
	for _, marker := range []string{
		"authentication service unavailable", "authentication temporarily unavailable",
		"authorization service unavailable", "authorization temporarily unavailable",
		"quota service unavailable", "quota temporarily unavailable",
		"parameter service unavailable", "parameter temporarily unavailable",
		"invalid upstream response", "malformed upstream response", "unexpected upstream response", "load balancer unavailable", "上游素材上传没有返回素材 id",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func providerErrorLooksPlatformFailure(message string) bool {
	for _, marker := range []string{
		"internal server error", "service unavailable", "temporarily unavailable", "temporary failure", "server overloaded", "overloaded", "upstream error", "gateway error", "bad gateway", "connection refused", "connection failed", "network unreachable", "dns", "dial tcp", "系统繁忙", "服务暂时不可用", "服务不可用",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func (c *taskTerminalCoordinator) handleExecutionFailure(task *model.Task, err error, providerSucceeded bool, channelSlotFailedBeforeRequest bool) error {
	requestNotStarted := channelSlotFailedBeforeRequest || providerRequestDefinitelyNotSubmitted(err)
	if errors.Is(err, context.Canceled) {
		// 用户取消会先把数据库任务置为 cancelled，再停止 worker context。
		// 此时不再重复退款/核对，只补齐 worker 侧的回放和日志收尾。
		if latest, latestErr := c.repo.Task(task.ID); latestErr == nil && latest.Status == model.TaskStatusCancelled {
			return c.handleAlreadyCancelled(*latest)
		}
		task.Status = model.TaskStatusCancelled
		task.Stage = "任务已取消"
		task.Error = "任务已取消"
		now := time.Now()
		task.CancellationSource, task.CancellationRequestedAt = model.TaskCancellationWorkerContext, &now
		recordTaskDiagnostic(task, "cancellation", "task_cancelled", taskSubmissionOutcome(task, requestNotStarted), "reconcile_cancellation", err)
		if terminalErr := c.markTerminalState(task); terminalErr != nil {
			return terminalErr
		}
		var billingErr error
		var settlementErr billingSettlementError
		if errors.As(err, &settlementErr) {
			billingErr = c.billing.MarkBillingUncertain(task.BillingOrderID, "任务取消时音频结算状态不明确")
		} else if requestNotStarted && taskIsAudio(task, nil) {
			billingErr = c.billing.ReleaseBillingReservation(task.BillingOrderID, "上游请求未发出，任务取消")
		} else if requestNotStarted {
			billingErr = c.billing.RefundBilling(task.BillingOrderID, "上游请求未发出，任务取消")
		} else {
			billingErr = c.billing.MarkBillingUncertain(task.BillingOrderID, "任务取消时上游费用状态不明确")
		}
		c.finalizeReplay(task, model.TaskStatusCancelled, "文本回放草稿归并失败")
		_ = c.logger.log(task.UserID, task.ID, "warn", "任务已取消", "")
		var resultErr error
		if billingErr != nil {
			if requestNotStarted && taskIsAudio(task, nil) {
				billingErr = errors.Join(billingErr, c.billing.MarkBillingUncertain(task.BillingOrderID, "取消时音频预授权释放失败，费用状态待核对"))
			}
			billingErr = c.persistBillingCleanupFailure(task, billingErr)
			resultErr = errors.Join(resultErr, fmt.Errorf("任务取消后的计费收尾失败：%w", billingErr))
		}
		return resultErr
	}

	task.Status = model.TaskStatusFailed
	c.ensureFailedAttemptLogged(task, err)
	task.Stage = "任务失败"
	task.Error = c.userFacingMessage(err)
	outcome := taskSubmissionOutcome(task, requestNotStarted)
	if providerSucceeded {
		outcome = "accepted"
	}
	var settlementErr billingSettlementError
	requiresReview := providerSucceeded || errors.As(err, &settlementErr) || (!requestNotStarted && c.billing.BillingFailureRequiresReview(task.BillingOrderID, task.ID, err))
	platformRetry := taskIsAudio(task, nil) && !requiresReview && audioPlatformRetryAllowed(err)
	retryClass := "new_generation_authorization"
	if platformRetry {
		retryClass = "platform_retry"
	} else if requiresReview || outcome == "unknown" {
		retryClass = "reconcile_submission"
	}
	recordTaskDiagnostic(task, "execution", "provider_execution_failed", outcome, retryClass, err)
	if terminalErr := c.markTerminalState(task); terminalErr != nil {
		return errors.Join(err, terminalErr)
	}
	c.finalizeReplay(task, model.TaskStatusFailed, "文本回放草稿归并失败")
	var billingErr error
	if requiresReview {
		billingErr = c.billing.MarkBillingUncertain(task.BillingOrderID, task.Error)
	} else if taskIsAudio(task, nil) {
		if requestNotStarted {
			billingErr = c.billing.ReleaseBillingReservation(task.BillingOrderID, "上游请求未发出，音频未开始执行")
		} else {
			billingErr = c.billing.SettleBilling(task.BillingOrderID, task.ProviderRequestID)
		}
	} else {
		billingErr = c.billing.RefundBilling(task.BillingOrderID, task.Error)
	}
	_ = c.logger.log(task.UserID, task.ID, "error", "任务处理失败", task.Error)
	resultErr := err
	if billingErr != nil {
		if !requiresReview && taskIsAudio(task, nil) {
			billingErr = errors.Join(billingErr, c.billing.MarkBillingUncertain(task.BillingOrderID, "音频计费收尾失败，费用状态待核对"))
		}
		billingErr = c.persistBillingCleanupFailure(task, billingErr)
		resultErr = errors.Join(resultErr, fmt.Errorf("任务计费收尾失败：%w", billingErr))
	}
	return resultErr
}

func (c *taskTerminalCoordinator) handleAlreadyCancelled(task model.Task) error {
	c.finalizeReplay(&task, model.TaskStatusCancelled, "文本回放草稿归并失败")
	_ = c.logger.log(task.UserID, task.ID, "warn", "任务已取消，worker 已停止执行", "")
	return nil
}

func (c *taskTerminalCoordinator) handleCancelledResult(task model.Task) error {
	c.finalizeReplay(&task, model.TaskStatusCancelled, "文本回放草稿归并失败")
	billingErr := c.billing.MarkBillingUncertain(task.BillingOrderID, "上游已返回结果，但任务被取消")
	_ = c.logger.log(task.UserID, task.ID, "warn", "任务已取消，丢弃生成结果", "")
	var resultErr error
	if billingErr != nil {
		resultErr = errors.Join(resultErr, fmt.Errorf("丢弃已生成结果后的计费收尾失败：%w", billingErr))
	}
	return resultErr
}

// handleResultPersistenceFailure 处理“上游成功但本地结果保存失败”的收尾。
// 返回 handled=true 表示并发取消已被识别并正常收尾，调用方不应再返回保存错误。
func (c *taskTerminalCoordinator) handleResultPersistenceFailure(task *model.Task, saveErr error) (handled bool, err error) {
	if errors.Is(saveErr, repository.ErrTaskStateConflict) {
		latest, latestErr := c.repo.Task(task.ID)
		if latestErr == nil && latest.Status == model.TaskStatusCancelled {
			if billingErr := c.handleCancelledResult(*latest); billingErr != nil {
				return true, billingErr
			}
			return true, nil
		}
	}

	task.Status = model.TaskStatusFailed
	task.Stage = "任务结果保存失败"
	task.Error = c.userFacingMessage(saveErr)
	recordTaskDiagnostic(task, "result_persistence", "result_persistence_failed", "accepted", "recover_result", saveErr)
	if terminalErr := c.markTerminalState(task); terminalErr != nil {
		return false, errors.Join(saveErr, terminalErr)
	}
	c.finalizeReplay(task, model.TaskStatusFailed, "文本回放草稿归并失败")
	billingErr := c.billing.MarkBillingUncertain(task.BillingOrderID, "上游已成功但任务结果未保存："+task.Error)
	_ = c.logger.log(task.UserID, task.ID, "error", "任务结果保存失败", task.Error)
	resultErr := saveErr
	if billingErr != nil {
		resultErr = errors.Join(resultErr, fmt.Errorf("任务结果保存失败后的计费收尾失败：%w", billingErr))
	}
	return false, resultErr
}

func (c *taskTerminalCoordinator) ensureFailedAttemptLogged(task *model.Task, err error) {
	if c.logFailedAttempt == nil || task == nil || err == nil {
		return
	}
	c.logFailedAttempt(*task, err)
}

func (c *taskTerminalCoordinator) handleSuccess(task *model.Task) error {
	return c.handleSuccessWithSettlement(task, nil)
}

// handleSuccessWithSettlement 与 handleSuccess 相同；settle 非 nil 时替换常规结算，
// 用于订单已退款后再交付作品的场景。
func (c *taskTerminalCoordinator) handleSuccessWithSettlement(task *model.Task, settle func() error) error {
	c.finalizeReplay(task, model.TaskStatusSucceeded, "文本回放窗口更新失败")
	var completionErr error
	completedTask, fetchErr := c.repo.Task(task.ID)
	if fetchErr != nil {
		completionErr = fmt.Errorf("任务成功后读取任务产物失败：%w", fetchErr)
		_ = c.logger.log(task.UserID, task.ID, "error", "任务成功但读取任务产物失败", fetchErr.Error())
	} else if completedTask.MediaRecoveryJSON == "" {
		if registerErr := c.outputs.RegisterTaskOutputFromTask(*completedTask); registerErr != nil {
			// 任务成功与产物登记分开记账；登记失败保持步骤异常，允许项目页幂等补登记。
			_ = c.logger.log(task.UserID, task.ID, "error", "任务成功但项目产物登记失败", registerErr.Error())
			completionErr = fmt.Errorf("任务成功后的项目产物登记失败：%w", registerErr)
		}
	}
	settleBilling := func() error {
		// 视频处理（画质增强等）按秒订单按实际输出时长结算；拿不到时长时返回错误进入待核对。
		if processing, ok := c.outputs.(videoProcessingTaskSettlement); ok && completedTask != nil && capabilityFromTaskType(task.Type) == "video" {
			if handled, err := processing.settleVideoProcessingBilling(*completedTask, ""); handled || err != nil {
				return err
			}
		}

		providerRequestID := task.ProviderRequestID
		if completedTask != nil && completedTask.ProviderRequestID != "" {
			providerRequestID = completedTask.ProviderRequestID
		}
		return c.billing.SettleBilling(task.BillingOrderID, providerRequestID)
	}
	if settle != nil {
		settleBilling = settle
	}
	if err := settleBilling(); err != nil {
		uncertainErr := c.billing.MarkBillingUncertain(task.BillingOrderID, "生成成功但积分结算失败："+err.Error())
		_ = c.logger.log(task.UserID, task.ID, "error", "积分结算失败，已进入待核对", err.Error())
		completionErr = errors.Join(completionErr, fmt.Errorf("积分结算失败：%w", err))
		if uncertainErr != nil {
			completionErr = errors.Join(completionErr, fmt.Errorf("记录计费待核对状态失败：%w", uncertainErr))
		}
	}
	_ = c.logger.log(task.UserID, task.ID, "info", "任务完成，结果已持久化", "")
	return completionErr
}

func (c *taskTerminalCoordinator) markTerminalState(task *model.Task) error {
	completedAt := time.Now()
	task.CompletedAt = &completedAt
	updated, err := c.repo.UpdateTaskTerminalDiagnostic(task, completedAt)
	if err != nil {
		return fmt.Errorf("写入任务终态失败：%w", err)
	}
	if !updated {
		return repository.ErrTaskStateConflict
	}
	if leases, ok := c.repo.(interface {
		ReleaseCloudAgentResourceLeases(string, string) error
	}); ok && task.ID != "" {
		if err := leases.ReleaseCloudAgentResourceLeases(task.UserID, "task:"+task.ID); err != nil {
			return fmt.Errorf("释放任务资源租约失败：%w", err)
		}
	}
	return nil
}

func (c *taskTerminalCoordinator) finalizeReplay(task *model.Task, status model.TaskStatus, message string) {
	if err := c.replay.finalizeTaskTextReplay(task.ID, status); err != nil {
		_ = c.logger.log(task.UserID, task.ID, "error", message, err.Error())
	}
}
