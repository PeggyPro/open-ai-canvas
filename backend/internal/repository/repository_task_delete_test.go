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
		&model.CloudAgentExecution{},
	); err != nil {
		t.Fatal(err)
	}

	taskID := "task-terminal"
	userID := "user-owner"
	createdAt := time.Now()
	if err := db.Create(&model.Task{ID: taskID, UserID: userID, Status: model.TaskStatusSucceeded, CreatedAt: createdAt}).Error; err != nil {
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
	if err := db.Create(&model.BillingOrder{ID: "order-kept", UserID: userID, TaskID: taskID}).Error; err != nil {
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
	var resource model.Resource
	if err := db.First(&resource, "id = ?", "resource-kept").Error; err != nil {
		t.Fatalf("generated resource was removed: %v", err)
	}
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
