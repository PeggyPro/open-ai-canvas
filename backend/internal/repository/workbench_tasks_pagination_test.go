package repository

import (
	"fmt"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"yingce/backend/internal/model"
)

func TestTasksWithWorkbenchKindPageExcludesActiveTasksAndCountsHistory(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Task{}); err != nil {
		t.Fatal(err)
	}

	createdAt := time.Now().Add(-time.Hour)
	tasks := make([]model.Task, 0, 13)
	for index := 0; index < 12; index++ {
		tasks = append(tasks, model.Task{
			ID:            fmt.Sprintf("audio-%02d", index),
			UserID:        "user",
			Type:          "canvas_audio",
			WorkbenchKind: "audio",
			Status:        model.TaskStatusSucceeded,
			InputJSON:     `{}`,
			CreatedAt:     createdAt.Add(time.Duration(index) * time.Minute),
		})
	}
	tasks = append(tasks, model.Task{
		ID:            "audio-active",
		UserID:        "user",
		Type:          "canvas_audio",
		WorkbenchKind: "audio",
		Status:        model.TaskStatusRunning,
		InputJSON:     `{}`,
		CreatedAt:     createdAt.Add(13 * time.Minute),
	})
	if err := db.Create(&tasks).Error; err != nil {
		t.Fatal(err)
	}

	repo := New(db)
	first, total, err := repo.TasksWithWorkbenchKindPage("user", 1, 10, "", "audio")
	if err != nil {
		t.Fatal(err)
	}
	if total != 12 {
		t.Fatalf("history total = %d, want 12", total)
	}
	if len(first) != 10 {
		t.Fatalf("first page length = %d, want 10", len(first))
	}
	for _, task := range first {
		if task.ID == "audio-active" || task.Status == model.TaskStatusRunning {
			t.Fatalf("first page included active task %#v", task)
		}
	}

	second, secondTotal, err := repo.TasksWithWorkbenchKindPage("user", 2, 10, "", "audio")
	if err != nil {
		t.Fatal(err)
	}
	if secondTotal != total || len(second) != 2 {
		t.Fatalf("second page = %d tasks, total %d; want 2 tasks and total %d", len(second), secondTotal, total)
	}
}
