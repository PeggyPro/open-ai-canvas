package workbench

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestAudioWorkbenchResolution(t *testing.T) {
	svc := &Service{}
	req := TaskInput{Kind: KindAudio, Prompt: " hello ", Model: "cosyvoice", ChannelID: "voice", Parameters: map[string]any{"audioVoice": " custom-voice-123 ", "audioFormat": " WAV ", "audioSpeed": 1.25, "audioInstructions": " gently "}}
	resolved, err := svc.ResolveTask("user", req)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.TaskType != "canvas_audio" || resolved.Operation != "audio" || resolved.Prompt != "hello" {
		t.Fatalf("resolution = %+v", resolved)
	}
	for key, want := range map[string]string{"audioVoice": "custom-voice-123", "audioFormat": "wav", "audioSpeed": "1.25", "audioInstructions": "gently"} {
		if resolved.Parameters[key] != want {
			t.Fatalf("%s = %v, want %s", key, resolved.Parameters[key], want)
		}
	}
	metadata := resolved.Input["metadata"].(map[string]any)
	if metadata["source"] != "workbench" || metadata["workbenchKind"] != KindAudio {
		t.Fatalf("metadata = %v", metadata)
	}
	if validKind(KindAudio) {
		t.Fatal("audio must not enter the image/video tool catalog")
	}
}

func TestAudioWorkbenchRejectsUnsupportedInput(t *testing.T) {
	svc := &Service{}
	for name, mutate := range map[string]func(*TaskInput){
		"empty prompt": func(r *TaskInput) { r.Prompt = " " },
		"long prompt":  func(r *TaskInput) { r.Prompt = strings.Repeat("x", 32001) },
		"tool":         func(r *TaskInput) { r.ToolID = "tool" },
		"nested tool":  func(r *TaskInput) { r.Workbench = &TaskWorkbench{Kind: KindAudio, ToolID: "tool"} },
		"preset":       func(r *TaskInput) { r.PresetID = "preset" },
		"reference":    func(r *TaskInput) { r.References = []TaskReference{{ResourceID: "ref", Kind: "audio"}} },
		"custom model": func(r *TaskInput) { r.ChannelID = "" },
		"url":          func(r *TaskInput) { r.Parameters = map[string]any{"baseURL": "https://untrusted.example"} },
	} {
		t.Run(name, func(t *testing.T) {
			req := TaskInput{Kind: KindAudio, Prompt: "hello", Model: "cosyvoice", ChannelID: "voice"}
			mutate(&req)
			if _, err := svc.ResolveTask("user", req); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestAudioWorkbenchParameterBoundaries(t *testing.T) {
	for _, params := range []map[string]any{
		{"audioSpeed": 0.24}, {"audioSpeed": 4.01}, {"audioSpeed": "NaN"}, {"audioSpeed": math.Inf(1)}, {"audioSpeed": true},
		{"audioFormat": "ogg"}, {"audioFormat": false}, {"audioVoice": 42}, {"audioInstructions": []string{"a"}},
		{"audioVoice": strings.Repeat("x", 257)}, {"audioInstructions": strings.Repeat("x", 4001)}, {"count": 1},
	} {
		if _, err := normalizeTaskParameters(KindAudio, params); err == nil {
			t.Fatalf("accepted %v", params)
		}
	}
	for _, speed := range []any{0.25, 4, " 1.5 ", json.Number("2"), 1} {
		if _, err := normalizeTaskParameters(KindAudio, map[string]any{"audioSpeed": speed}); err != nil {
			t.Fatal(err)
		}
	}
	for _, format := range []string{"mp3", "wav", "opus", "aac", "flac", "pcm"} {
		if _, err := normalizeTaskParameters(KindAudio, map[string]any{"audioFormat": format}); err != nil {
			t.Fatal(err)
		}
	}
}
