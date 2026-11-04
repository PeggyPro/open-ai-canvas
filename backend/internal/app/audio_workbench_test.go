package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"yingce/backend/internal/model"
	"yingce/backend/internal/platform"
)

func TestAudioWorkbenchFeatureDefaultsAndPersistence(t *testing.T) {
	svc, db := newFeatureAvailabilityTestService(t)
	if enabled, err := svc.FeatureEnabled(FeatureAudioWorkbench); err != nil || enabled {
		t.Fatalf("default = %v, %v", enabled, err)
	}
	if err := db.Create(&model.SystemSetting{Key: featureAvailabilitySettingKey, ValueJSON: `{"imageWorkbenchEnabled":true}`}).Error; err != nil {
		t.Fatal(err)
	}
	if enabled, err := svc.FeatureEnabled(FeatureAudioWorkbench); err != nil || enabled {
		t.Fatalf("legacy = %v, %v", enabled, err)
	}
	value := platform.DefaultFeatureAvailability()
	value.AudioWorkbenchEnabled = true
	if _, err := svc.UpdateFeatureAvailability(&model.User{ID: "ordinary", Role: model.UserRoleUser}, value); err == nil {
		t.Fatal("ordinary user changed availability")
	}
	for _, enabled := range []bool{true, false, true} {
		value.AudioWorkbenchEnabled = enabled
		if _, err := svc.UpdateFeatureAvailability(&model.User{ID: "admin", Role: model.UserRoleAdmin}, value); err != nil {
			t.Fatal(err)
		}
		actual, err := svc.FeatureEnabled(FeatureAudioWorkbench)
		if err != nil || actual != enabled {
			t.Fatalf("persisted = %v, %v", actual, err)
		}
	}
}

func TestAudioWorkbenchAdmissionBillingAndDisable(t *testing.T) {
	svc, db, _, _ := creationTestService(t)
	svc.coordinator = platform.NewCoordinatorWithRedis(nil, "audio-workbench-test")
	svc.platform = platform.New(svc.repo, svc.coordinator, platformHost{svc: svc})
	req := WorkbenchTaskInput{Kind: WorkbenchKindAudio, Prompt: "hello", Model: "cosyvoice", ChannelID: "channel", Parameters: map[string]any{"audioVoice": "custom-voice", "audioFormat": "wav", "audioSpeed": "1.25", "audioInstructions": "gently"}}
	assertClosed := func(err error) {
		t.Helper()
		var authErr *AuthError
		if !errors.As(err, &authErr) || authErr.Status != 403 {
			t.Fatalf("expected feature 403, got %v", err)
		}
	}
	_, err := svc.CreateWorkbenchTask("user", req, "", "")
	assertClosed(err)
	profile := DefaultModelCapabilityConfigForModel(string(model.ChannelInterfaceOpenAIAudio), "cosyvoice")
	for _, record := range []any{
		&model.ChannelModel{ID: "audio-model", ChannelID: "channel", ModelKey: "cosyvoice", Capability: "audio", Protocol: model.ChannelInterfaceOpenAIAudio, Enabled: true, CapabilityConfigJSON: mustEncodeModelCapabilityConfig(t, profile)},
		&model.ChannelModelPriceTier{ID: "audio-price", ChannelModelID: "audio-model", SelectorKey: `{}`, SelectorJSON: `{}`, BillingMode: "fixed_request", UnitPriceMicrocredits: 10, PriceConfigured: true, Enabled: true},
	} {
		if err := db.Create(record).Error; err != nil {
			t.Fatal(err)
		}
	}
	features := platform.DefaultFeatureAvailability()
	features.AudioWorkbenchEnabled = true
	admin := &model.User{ID: "admin", Role: model.UserRoleAdmin}
	if _, err := svc.UpdateFeatureAvailability(admin, features); err != nil {
		t.Fatal(err)
	}
	task, err := svc.CreateWorkbenchTask("user", req, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if task.Type != "canvas_audio" || task.WorkbenchKind != WorkbenchKindAudio || task.Operation != "audio" {
		t.Fatalf("wrong task: %+v", task)
	}
	var stored model.Task
	if err := db.First(&stored, "id = ?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	var execution canvasGenerationInput
	if err := json.Unmarshal([]byte(stored.InputJSON), &execution); err != nil {
		t.Fatal(err)
	}
	if execution.Config.AudioVoice != "custom-voice" || execution.Config.AudioFormat != "wav" || execution.Config.AudioSpeed != "1.25" || execution.Config.AudioInstructions != "gently" {
		t.Fatalf("audio params = %+v", execution.Config)
	}
	// Exercise the installed speech adapter using the persisted admission input.
	t.Setenv("CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS", "127.0.0.1")
	wave := []byte("RIFF\x26\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x40\x1f\x00\x00\x80\x3e\x00\x00\x02\x00\x10\x00data\x02\x00\x00\x00\x00\x00")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/audio/speech" || r.Header.Get("Authorization") != "Bearer local" {
			t.Errorf("unexpected speech request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["model"] != "cosyvoice" || body["input"] != "hello" || body["voice"] != "custom-voice" || body["response_format"] != "wav" || body["speed"] != 1.25 || body["instructions"] != "gently" {
			t.Errorf("speech payload = %#v", body)
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(wave)
	}))
	defer server.Close()
	execution.Config.BaseURL, execution.Config.APIKey = server.URL, "local"
	ctx := ensureOfficialProtocolAdapter(context.Background(), execution.Config.InterfaceType)
	if _, ok := declarativeProtocolAdapterForContext(ctx, execution.Config.InterfaceType); !ok {
		t.Fatal("official speech adapter not loaded")
	}
	if err := os.WriteFile(filepath.Join(svc.dataDir, ".settings-key"), make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	stored.Status, stored.LeaseOwner, stored.LeaseExpiresAt, stored.RouteRun = model.TaskStatusRunning, "audio-test", &expires, 1
	if err := db.Save(&stored).Error; err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(withProviderAnalytics(ctx, svc, stored), mediaExecutionTaskKey{}, stored)
	result, err := runAudioTask(ctx, execution)
	if err != nil {
		t.Fatalf("audio execution: %v (cause: %v)", err, errors.Unwrap(err))
	}
	result, err = svc.persistGeneratedMediaResult("user", result)
	if err != nil {
		t.Fatal(err)
	}
	audio := result["audio"].(map[string]any)
	resource, err := svc.repo.Resource(audio["resourceId"].(string))
	if err != nil || resource.UserID != "user" || resource.Kind != "audio" || resource.Size != int64(len(wave)) || !strings.HasPrefix(audio["url"].(string), "/api/resources/") {
		t.Fatalf("persisted speech resource = %#v, %v", resource, err)
	}
	var order model.BillingOrder
	if err := db.First(&order, "id = ?", task.BillingOrderID).Error; err != nil || order.PriceTierID != "audio-price" {
		t.Fatalf("billing = %s, %v", order.PriceTierID, err)
	}
	items, err := svc.WorkbenchTasks("user", WorkbenchKindAudio, false, 100)
	if err != nil || len(items) != 1 {
		t.Fatalf("history = %d, %v", len(items), err)
	}
	items, err = svc.WorkbenchTasks("other", WorkbenchKindAudio, false, 100)
	if err != nil || len(items) != 0 {
		t.Fatalf("cross-user history = %d, %v", len(items), err)
	}
	features.AudioWorkbenchEnabled = false
	if _, err := svc.UpdateFeatureAvailability(admin, features); err != nil {
		t.Fatal(err)
	}
	_, err = svc.WorkbenchTasks("user", WorkbenchKindAudio, false, 100)
	assertClosed(err)
	_, err = svc.CreateWorkbenchTask("user", req, "", "")
	assertClosed(err)
	_, err = svc.CreateTask("user", CreateTaskRequest{Type: "canvas_audio", Prompt: "hello", Input: map[string]any{"mode": "audio", "metadata": map[string]any{"source": "workbench", "workbenchKind": "audio"}, "config": map[string]any{"model": "cosyvoice", "channelId": "channel"}}})
	assertClosed(err)
	// A failed historical task remains readable, but cannot start another paid call.
	failed := &model.Task{ID: "old-audio", UserID: "user", Type: "canvas_audio", WorkbenchKind: WorkbenchKindAudio, Status: model.TaskStatusFailed, InputJSON: `{"mode":"audio"}`, ResultJSON: `{"audio":{"url":"/api/resources/old/file"}}`}
	if err := db.Create(failed).Error; err != nil {
		t.Fatal(err)
	}
	_, err = svc.RetryTask("user", failed.ID)
	assertClosed(err)
	if err := db.First(&stored, "id = ?", task.ID).Error; err != nil {
		t.Fatal("disabling deleted history:", err)
	}
	var retained model.Task
	if err := db.First(&retained, "id = ?", failed.ID).Error; err != nil || retained.ResultJSON != failed.ResultJSON {
		t.Fatalf("disabling lost result: %v", err)
	}
}

func TestAudioWorkbenchPreviewAndPublicParameters(t *testing.T) {
	for _, raw := range []string{`{"audio":{"dataUrl":"/api/resources/a/file","format":"wav"}}`, `{"audio":{"url":"https://media.example/speech.wav"}}`, `{"url":"/api/resources/a/file"}`} {
		url, kind := taskMediaPreview(raw, "canvas_audio")
		if url == "" || kind != "audio" {
			t.Fatalf("preview = %s, %s", url, kind)
		}
	}
	url, _ := taskMediaPreview(`{"audio":{"dataUrl":"data:audio/wav;base64,AAAA"}}`, "canvas_audio")
	if url != "" {
		t.Fatal("inline media must not bloat task summaries")
	}
	raw := `{"mode":"audio","metadata":{"source":"workbench","workbenchKind":"audio","workbench":{"kind":"audio","parameters":{"audioVoice":"my-voice","audioFormat":"wav","audioSpeed":"1.25","audioInstructions":"gently","apiKey":"secret"}}},"config":{"apiKey":"secret"}}`
	var output map[string]any
	if err := json.Unmarshal([]byte(publicTaskInputJSON(raw)), &output); err != nil {
		t.Fatal(err)
	}
	params := output["workbench"].(map[string]any)["parameters"].(map[string]any)
	if params["audioVoice"] != "my-voice" || params["audioInstructions"] != "gently" {
		t.Fatalf("params = %v", params)
	}
	if _, ok := params["apiKey"]; ok {
		t.Fatal("non-allowlisted parameter exposed")
	}
	if _, ok := output["config"]; ok {
		t.Fatal("provider config exposed")
	}
}
