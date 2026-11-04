import { normalizeAudioFormatValue, normalizeAudioSpeedValue, normalizeAudioVoiceValue } from "@/lib/audio-generation";
import { resourceFileUrl, resourceIdFromStorageKey, resolveResourceAccessURL } from "@/services/api/resources";
import type { GenerationTask } from "@/services/api/task-center";
import type { CreateWorkbenchTaskInput } from "@/services/api/workbench";
import { logicalModelIDForConfig, modelOptionName, resolveModelChannel, type AiConfig } from "@/stores/use-config-store";

export function isSpeechWorkbenchModel(config: AiConfig, model: string) {
    const channel = resolveModelChannel(config, model);
    const name = modelOptionName(model);
    if (/(whisper|transcri|(?:^|[-_])(?:asr|stt)(?:$|[-_])|music|suno)/i.test(name)) return false;
    return channel?.scope === "system" || Boolean(logicalModelIDForConfig({ ...config, model }));
}

export function audioWorkbenchRequest(config: AiConfig, model: string, prompt: string): CreateWorkbenchTaskInput {
    const text = prompt.trim();
    if (!text) throw new Error("请填写朗读文本");
    if (Array.from(text).length > 32000) throw new Error("朗读文本不能超过 32000 字");
    if (!model || !isSpeechWorkbenchModel(config, model)) throw new Error("请选择系统目录中的语音合成模型");
    const voice = config.audioVoice.trim();
    const instructions = config.audioInstructions.trim();
    if (Array.from(voice).length > 256) throw new Error("音色名称或 ID 不能超过 256 字");
    if (Array.from(instructions).length > 4000) throw new Error("朗读指令不能超过 4000 字");
    const speed = Number(config.audioSpeed);
    if (!config.audioSpeed.trim() || !Number.isFinite(speed) || speed < 0.25 || speed > 4) throw new Error("语速须在 0.25 到 4 之间");
    const logicalModelId = logicalModelIDForConfig({ ...config, model });
    return {
        kind: "audio",
        prompt: text,
        model: modelOptionName(model),
        ...(logicalModelId ? { logicalModelId } : { channelId: resolveModelChannel(config, model)?.id }),
        parameters: {
            audioVoice: normalizeAudioVoiceValue(voice),
            audioFormat: normalizeAudioFormatValue(config.audioFormat),
            audioSpeed: normalizeAudioSpeedValue(config.audioSpeed),
            audioInstructions: instructions,
        },
    };
}

function record(value: unknown): Record<string, unknown> {
    return value && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : {};
}
function parseRecord(value?: string) {
    try {
        return record(JSON.parse(value || "{}"));
    } catch {
        return {};
    }
}
function text(value: unknown) {
    return typeof value === "string" ? value : "";
}

export function audioWorkbenchSnapshot(task: GenerationTask) {
    const input = parseRecord(task.inputJson);
    const workbench = record(input.workbench);
    const parameters = record(workbench.parameters);
    return {
        voice: normalizeAudioVoiceValue(text(parameters.audioVoice)),
        format: normalizeAudioFormatValue(text(parameters.audioFormat)),
        speed: normalizeAudioSpeedValue(text(parameters.audioSpeed)),
        instructions: text(parameters.audioInstructions),
    };
}

export function audioWorkbenchOutput(task: GenerationTask) {
    const audio = record(parseRecord(task.resultJson).audio);
    const storageKey = text(audio.storageKey) || (text(audio.resourceId) ? `resource:${text(audio.resourceId)}` : "");
    const resourceId = resourceIdFromStorageKey(storageKey);
    const candidate = text(audio.url) || text(audio.dataUrl) || (task.previewKind === "audio" ? task.previewUrl || "" : "");
    const url = resourceId ? resourceFileUrl(resourceId) : /^(https?:\/\/|\/api\/resources\/)/i.test(candidate) ? resolveResourceAccessURL(candidate) : "";
    const mimeFormats: Record<string, string> = {
        "audio/mpeg": "mp3",
        "audio/mp3": "mp3",
        "audio/wav": "wav",
        "audio/x-wav": "wav",
        "audio/wave": "wav",
        "audio/ogg": "opus",
        "audio/opus": "opus",
        "audio/aac": "aac",
        "audio/flac": "flac",
        "audio/x-flac": "flac",
        "audio/pcm": "pcm",
    };
    const mimeFormat = mimeFormats[text(audio.mimeType).split(";")[0].trim().toLowerCase()];
    const format = normalizeAudioFormatValue(mimeFormat || text(audio.format) || audioWorkbenchSnapshot(task).format);
    return { url, storageKey, format, fileName: `语音-${task.id}.${format}` };
}
