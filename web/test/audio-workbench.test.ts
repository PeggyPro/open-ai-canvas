import { expect, test } from "bun:test";
import { audioWorkbenchGenerationDurationMs, audioWorkbenchOutput, audioWorkbenchRequest, audioWorkbenchSnapshot, formatAudioWorkbenchGenerationDuration, isSpeechWorkbenchModel } from "../src/pages/workbench/audio-workbench";
import { defaultConfig, type AiConfig } from "../src/stores/use-config-store";
import type { GenerationTask } from "../src/services/api/task-center";

const config: AiConfig = {
    ...defaultConfig,
    audioVoice: "  voice-custom-001  ",
    audioFormat: "wav",
    audioSpeed: "1.25",
    audioInstructions: " gently ",
    channels: [{ id: "voice", name: "VoiceStudio", scope: "system", baseUrl: "http://private.example", apiKey: "secret", apiFormat: "openai", interfaceType: "openai-audio", models: ["cosyvoice", "whisper-1"] }],
};
const task = (values: Partial<GenerationTask>) => ({ id: "speech-1", ...values }) as GenerationTask;

test("audio requests keep custom voices and submit only server-resolved model identity", () => {
    const request = audioWorkbenchRequest(config, "voice::cosyvoice", " hello ");
    expect(request).toEqual({ kind: "audio", prompt: "hello", model: "cosyvoice", channelId: "voice", parameters: { audioVoice: "voice-custom-001", audioFormat: "wav", audioSpeed: "1.25", audioInstructions: "gently" } });
    expect(JSON.stringify(request)).not.toContain("secret");
    expect(JSON.stringify(request)).not.toContain("private.example");
    const logical: AiConfig = { ...config, channels: [{ ...config.channels[0], modelCosts: [{ model: "cosyvoice", capability: "audio", billingMode: "fixed_request", unitPriceMicrocredits: 0, logicalModelId: "logical-voice" }] }] };
    const logicalRequest = audioWorkbenchRequest(logical, "voice::cosyvoice", "hello");
    expect(logicalRequest.logicalModelId).toBe("logical-voice");
    expect(logicalRequest.channelId).toBeUndefined();
});

test("speech model filter excludes transcription and personal channels", () => {
    expect(isSpeechWorkbenchModel(config, "voice::cosyvoice")).toBe(true);
    expect(isSpeechWorkbenchModel(config, "voice::whisper-1")).toBe(false);
    expect(isSpeechWorkbenchModel({ ...config, channels: [{ ...config.channels[0], scope: "user" }] }, "voice::cosyvoice")).toBe(false);
});

test("audio input validation rejects invalid lengths and speeds", () => {
    for (const speed of ["", "NaN", "Infinity", "0.1", "5"]) expect(() => audioWorkbenchRequest({ ...config, audioSpeed: speed }, "voice::cosyvoice", "hello")).toThrow();
    expect(() => audioWorkbenchRequest(config, "voice::cosyvoice", " ")).toThrow();
    expect(() => audioWorkbenchRequest(config, "voice::cosyvoice", "x".repeat(32001))).toThrow();
    expect(() => audioWorkbenchRequest({ ...config, audioVoice: "x".repeat(257) }, "voice::cosyvoice", "hello")).toThrow();
    expect(() => audioWorkbenchRequest({ ...config, audioInstructions: "x".repeat(4001) }, "voice::cosyvoice", "hello")).toThrow();
});

test("history restores persisted options and tolerates malformed legacy input", () => {
    expect(audioWorkbenchSnapshot(task({ inputJson: JSON.stringify({ workbench: { parameters: { audioVoice: "custom", audioFormat: "wav", audioSpeed: "2", audioInstructions: "warm" } } }) }))).toEqual({
        voice: "custom",
        format: "wav",
        speed: "2",
        instructions: "warm",
    });
    expect(audioWorkbenchSnapshot(task({ inputJson: "broken" })).voice).toBe("alloy");
});

test("audio generation duration measures execution time rather than audio length", () => {
    const startedAt = "2026-01-01T00:00:00.000Z";
    const completedAt = "2026-01-01T00:00:12.345Z";
    expect(audioWorkbenchGenerationDurationMs(task({ startedAt, completedAt }))).toBe(12345);
    expect(audioWorkbenchGenerationDurationMs(task({ startedAt }), Date.parse("2026-01-01T00:00:03.250Z"))).toBe(3250);
    expect(audioWorkbenchGenerationDurationMs(task({ startedAt: "invalid", completedAt }))).toBeUndefined();
    expect(audioWorkbenchGenerationDurationMs(task({ startedAt: completedAt, completedAt: startedAt }))).toBeUndefined();
    expect(formatAudioWorkbenchGenerationDuration(820)).toBe("820 毫秒");
    expect(formatAudioWorkbenchGenerationDuration(12345)).toBe("12.3 秒");
    expect(formatAudioWorkbenchGenerationDuration(62500)).toBe("1 分 3 秒");
});

test("audio output uses persisted resource identity before untrusted external URLs", () => {
    const result = audioWorkbenchOutput(task({ resultJson: JSON.stringify({ audio: { storageKey: "resource:audio-1", dataUrl: "https://old.example/a", format: "wav" } }) }));
    expect(result.url).toEndWith("/resources/audio-1/file");
    expect(result.storageKey).toBe("resource:audio-1");
    expect(result.fileName).toBe("语音-speech-1.wav");
    expect(audioWorkbenchOutput(task({ resultJson: JSON.stringify({ audio: { dataUrl: "/api/resources/audio-2/file" } }) })).url).toEndWith("/api/resources/audio-2/file");
    for (const candidate of ["javascript:alert(1)", "data:audio/wav;base64,AAAA", "//untrusted.example"]) expect(audioWorkbenchOutput(task({ resultJson: JSON.stringify({ audio: { url: candidate } }) })).url).toBe("");
    expect(audioWorkbenchOutput(task({ previewKind: "audio", previewUrl: "https://example.com/a.wav" })).url).toBe("https://example.com/a.wav");
    expect(audioWorkbenchOutput(task({ previewKind: "image", previewUrl: "https://example.com/a.png" })).url).toBe("");
});

test("audio download extension follows the returned MIME type", () => {
    const task = { id: "voice", resultJson: JSON.stringify({ audio: { url: "https://example.com/voice", mimeType: "audio/wav; charset=binary" } }) } as GenerationTask;
    expect(audioWorkbenchOutput(task).format).toBe("wav");
    expect(audioWorkbenchOutput(task).fileName).toBe("语音-voice.wav");
});
