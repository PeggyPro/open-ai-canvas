package repository

import (
	"errors"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"yingce/backend/internal/model"
)

func TestDeleteTerminalTaskRemovesHistoryAndPreservesResources(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:task-delete-history?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&model.Task{}, &model.TaskTextDelta{}, &model.TaskLog{}, &model.Result{}, &model.ApiCallLog{}, &model.RouteAttempt{},
		&model.AssetRepresentation{}, &model.Resource{}, &model.BillingOrder{},
		&model.ShotArtifact{}, &model.CreationSubmission{}, &model.DramaTake{}, &model.DramaJob{},
		&model.AgentMemoryProposal{}, &model.ResourceOffloadJob{}, &model.WorkflowStepTask{}, &model.ProductionTaskLink{},
		&model.CloudAgentExecution{}, &model.CreditAccount{}, &model.CreditLedgerEntry{},
	); err != nil {
		t.Fatal(err)
	}

	taskID := "task-terminal"
	userID := "user-owner"
	createdAt := time.Now()
	if err := db.Create(&model.Task{ID: taskID, UserID: userID, Type: "canvas_generation", InputJSON: `{"mode":"audio"}`, BillingOrderID: "order-kept", Status: model.TaskStatusFailed, ExecutionDiagnosticJSON: `{"phase":"preparation","taskSubmitted":false,"submissionOutcome":"not_submitted","retryClass":"correct_input"}`, CreatedAt: createdAt}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Resource{ID: "resource-kept", UserID: userID, Kind: "audio"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AssetRepresentation{
		ID: "representation-kept", TaskID: taskID, ResourceID: "resource-kept", Role: "audio",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.CreditAccount{UserID: userID, AvailableMicrocredits: 900, ReservedMicrocredits: 100}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.BillingOrder{
		ID: "order-kept", UserID: userID, TaskID: taskID, Status: model.BillingStatusReserved,
		AmountMicrocredits: 100, ReservedAmountMicrocredits: 100,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create([]model.TaskTextDelta{
		{ID: "delta-owner", UserID: userID, TaskID: taskID, Sequence: 1},
		{ID: "delta-other", UserID: "other-user", TaskID: taskID, Sequence: 2},
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.TaskLog{ID: "log-owner", UserID: userID, TaskID: taskID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Result{ID: "result-owner", UserID: userID, TaskID: taskID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ApiCallLog{ID: "api-owner", UserID: userID, TaskID: taskID}).Error; err != nil {
		t.Fatal(err)
	}

	repo := New(db)
	if err := repo.DeleteTerminalTask(userID, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Task(taskID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("task lookup error = %v, want not found", err)
	}
	for _, item := range []struct {
		name  string
		model any
	}{
		{name: "task text delta", model: &model.TaskTextDelta{}},
		{name: "task log", model: &model.TaskLog{}},
		{name: "result", model: &model.Result{}},
		{name: "api call log", model: &model.ApiCallLog{}},
	} {
		var count int64
		if err := db.Model(item.model).Where("task_id = ? AND user_id = ?", taskID, userID).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s count = %d, want 0", item.name, count)
		}
	}
	var otherDelta model.TaskTextDelta
	if err := db.First(&otherDelta, "id = ?", "delta-other").Error; err != nil {
		t.Fatalf("foreign history row was removed: %v", err)
	}
	var representation model.AssetRepresentation
	if err := db.First(&representation, "id = ?", "representation-kept").Error; err != nil {
		t.Fatal(err)
	}
	if representation.TaskID != "" || representation.ResourceID != "resource-kept" {
		t.Fatalf("representation = %+v, want detached task and retained resource", representation)
	}
	var order model.BillingOrder
	if err := db.First(&order, "id = ?", "order-kept").Error; err != nil {
		t.Fatal(err)
	}
	if order.TaskID != "" {
		t.Fatalf("billing order task id = %q, want detached", order.TaskID)
	}
	if order.Capability != model.WorkbenchKindAudio {
		t.Fatalf("detached audio order capability = %q, want %q", order.Capability, model.WorkbenchKindAudio)
	}
	if order.Status != model.BillingStatusReleased {
		t.Fatalf("deleted pre-request audio order status = %q, want released", order.Status)
	}
	var account model.CreditAccount
	if err := db.First(&account, "user_id = ?", userID).Error; err != nil {
		t.Fatal(err)
	}
	if account.AvailableMicrocredits != 1_000 || account.ReservedMicrocredits != 0 {
		t.Fatalf("deleted pre-request audio account = %#v; want available=1000 reserved=0", account)
	}
	if err := repo.RefundBillingOrder(order.ID, "late refund attempt"); err == nil {
		t.Fatal("detached audio order was refundable")
	}
	var resource model.Resource
	if err := db.First(&resource, "id = ?", "resource-kept").Error; err != nil {
		t.Fatalf("generated resource was removed: %v", err)
	}
}

func TestDeleteTerminalTaskPreservesReservedAudioOrderWithoutPreSubmitProof(t *testing.T) {
	db := openTaskDeleteTestDB(t, "task-delete-unknown")
	userID := "user-owner"
	if err := db.Create(&model.CreditAccount{UserID: userID, AvailableMicrocredits: 500, ReservedMicrocredits: 500}).Error; err != nil {
		t.Fatal(err)
	}
	tasks := []model.Task{
		{ID: "task-accepted", UserID: userID, Type: "canvas_audio", WorkbenchKind: model.WorkbenchKindAudio, BillingOrderID: "order-accepted", Status: model.TaskStatusFailed, ExecutionDiagnosticJSON: `{"phase":"execution","taskSubmitted":true,"submissionOutcome":"accepted","retryClass":"reconcile_submission"}`},
		{ID: "task-unknown", UserID: userID, Type: "canvas_audio", WorkbenchKind: model.WorkbenchKindAudio, BillingOrderID: "order-unknown", Status: model.TaskStatusCancelled},
		{ID: "task-cleanup", UserID: userID, Type: "canvas_audio", WorkbenchKind: model.WorkbenchKindAudio, BillingOrderID: "order-cleanup", Status: model.TaskStatusFailed, ExecutionDiagnosticJSON: `{"phase":"billing","taskSubmitted":false,"submissionOutcome":"not_submitted","retryClass":"reconcile_submission"}`},
		{ID: "task-stale", UserID: userID, Type: "canvas_audio", WorkbenchKind: model.WorkbenchKindAudio, BillingOrderID: "order-stale", Status: model.TaskStatusSucceeded, ExecutionDiagnosticJSON: `{"phase":"execution","taskSubmitted":false,"submissionOutcome":"not_submitted","retryClass":"platform_retry"}`},
		{ID: "task-mismatch", UserID: userID, Type: "canvas_audio", WorkbenchKind: model.WorkbenchKindAudio, BillingOrderID: "order-mismatch", Status: model.TaskStatusFailed, ExecutionDiagnosticJSON: `{"phase":"preparation","taskSubmitted":false,"submissionOutcome":"not_submitted","retryClass":"correct_input"}`},
	}
	if err := db.Create(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create([]model.BillingOrder{
		{ID: "order-accepted", UserID: userID, TaskID: "task-accepted", Capability: model.WorkbenchKindAudio, Status: model.BillingStatusReserved, AmountMicrocredits: 100, ReservedAmountMicrocredits: 100},
		{ID: "order-unknown", UserID: userID, TaskID: "task-unknown", Capability: model.WorkbenchKindAudio, Status: model.BillingStatusReserved, AmountMicrocredits: 100, ReservedAmountMicrocredits: 100},
		{ID: "order-cleanup", UserID: userID, TaskID: "task-cleanup", Capability: model.WorkbenchKindAudio, Status: model.BillingStatusReserved, AmountMicrocredits: 100, ReservedAmountMicrocredits: 100},
		{ID: "order-stale", UserID: userID, TaskID: "task-stale", Capability: model.WorkbenchKindAudio, Status: model.BillingStatusReserved, AmountMicrocredits: 100, ReservedAmountMicrocredits: 100},
		{ID: "order-mismatch", UserID: userID, TaskID: "other-task", Capability: model.WorkbenchKindAudio, Status: model.BillingStatusReserved, AmountMicrocredits: 100, ReservedAmountMicrocredits: 100},
	}).Error; err != nil {
		t.Fatal(err)
	}

	repo := New(db)
	for _, taskID := range []string{"task-accepted", "task-unknown", "task-cleanup", "task-stale", "task-mismatch"} {
		if err := repo.DeleteTerminalTask(userID, taskID); err != nil {
			t.Fatalf("delete %s: %v", taskID, err)
		}
	}
	var orders []model.BillingOrder
	if err := db.Order("id").Find(&orders).Error; err != nil {
		t.Fatal(err)
	}
	if len(orders) != 5 {
		t.Fatalf("orders = %d, want 5 preserved orders", len(orders))
	}
	for _, order := range orders {
		if order.Status != model.BillingStatusReserved {
			t.Fatalf("order %s status = %q, want reserved", order.ID, order.Status)
		}
	}
	var account model.CreditAccount
	if err := db.First(&account, "user_id = ?", userID).Error; err != nil {
		t.Fatal(err)
	}
	if account.AvailableMicrocredits != 500 || account.ReservedMicrocredits != 500 {
		t.Fatalf("account = %#v, want available=500 reserved=500", account)
	}
}

func openTaskDeleteTestDB(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&model.Task{}, &model.TaskTextDelta{}, &model.TaskLog{}, &model.Result{}, &model.ApiCallLog{}, &model.RouteAttempt{},
		&model.AssetRepresentation{}, &model.Resource{}, &model.BillingOrder{}, &model.ShotArtifact{}, &model.CreationSubmission{}, &model.DramaTake{}, &model.DramaJob{},
		&model.AgentMemoryProposal{}, &model.ResourceOffloadJob{}, &model.WorkflowStepTask{}, &model.ProductionTaskLink{}, &model.CloudAgentExecution{},
		&model.CreditAccount{}, &model.CreditLedgerEntry{},
	); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestDeleteTerminalTaskRejectsActiveAndForeignTasks(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:task-delete-guards?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Task{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create([]model.Task{
		{ID: "task-running", UserID: "owner", Status: model.TaskStatusRunning},
		{ID: "task-queued", UserID: "owner", Status: model.TaskStatusQueued},
		{ID: "task-foreign", UserID: "other", Status: model.TaskStatusSucceeded},
	}).Error; err != nil {
		t.Fatal(err)
	}
	repo := New(db)
	for _, taskID := range []string{"task-running", "task-queued"} {
		if err := repo.DeleteTerminalTask("owner", taskID); !errors.Is(err, ErrTaskStateConflict) {
			t.Fatalf("delete %s error = %v, want state conflict", taskID, err)
		}
		if _, err := repo.Task(taskID); err != nil {
			t.Fatalf("active task %s disappeared: %v", taskID, err)
		}
	}
	for _, taskID := range []string{"task-foreign", "missing"} {
		if err := repo.DeleteTerminalTask("owner", taskID); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("delete %s error = %v, want not found", taskID, err)
		}
	}
}
