import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { App, Button, Form, Input, InputNumber, Popconfirm, Select, Spin } from "antd";
import { AudioLines, Copy, Download, RefreshCw, RotateCcw, Square, Trash2 } from "lucide-react";
import { ModelPicker } from "@/components/model-picker";
import { PageHeader, PaginationBar, WorkspacePage } from "@/components/layout/workspace-page";
import { Callout } from "@/components/ui/product/callout/callout";
import { openAIAudioFormatOptions, normalizeAudioFormatValue } from "@/lib/audio-generation";
import { cancelGenerationTask, deleteGenerationTask, queryGenerationTask, retryGenerationTask, type GenerationTask } from "@/services/api/task-center";
import { createWorkbenchTask, listWorkbenchTaskPage } from "@/services/api/workbench";
import { downloadBrowserMedia } from "@/services/browser-download";
import { modelOptionName, selectableModelsByCapability, useConfigStore, useEffectiveConfig } from "@/stores/use-config-store";
import { useUserStore } from "@/stores/use-user-store";
import { audioWorkbenchGenerationDurationMs, audioWorkbenchOutput, audioWorkbenchRequest, audioWorkbenchSnapshot, formatAudioWorkbenchGenerationDuration, isSpeechWorkbenchModel } from "./audio-workbench";
import { isActiveWorkbenchTask } from "./workbench-batches";

const errorText = (error: unknown) => (error instanceof Error ? error.message : "操作失败，请稍后重试");
const queryRoot = (userId: string) => ["audio-workbench", userId] as const;
const HISTORY_PAGE_SIZE = 10;

async function copyTextToClipboard(value: string) {
    try {
        if (navigator.clipboard?.writeText) {
            await navigator.clipboard.writeText(value);
            return;
        }
    } catch {
        // Fall through to the legacy browser copy path.
    }
    const textarea = document.createElement("textarea");
    textarea.value = value;
    textarea.setAttribute("readonly", "");
    textarea.style.position = "fixed";
    textarea.style.opacity = "0";
    document.body.appendChild(textarea);
    textarea.select();
    const copied = document.execCommand("copy");
    textarea.remove();
    if (!copied) throw new Error("copy failed");
}

type AudioTaskStatusMeta = {
    label: string;
    borderColor: string;
    iconClassName: string;
    badgeClassName: string;
    progressClassName: string;
    textClassName: string;
};

function audioTaskStatusMeta(status: GenerationTask["status"]): AudioTaskStatusMeta {
    switch (status) {
        case "succeeded":
            return {
                label: "已完成",
                borderColor: "#27a36a",
                iconClassName: "bg-emerald-50 text-emerald-600",
                badgeClassName: "border-emerald-200 bg-emerald-50 text-emerald-700",
                progressClassName: "bg-emerald-500",
                textClassName: "text-emerald-700",
            };
        case "failed":
            return {
                label: "失败",
                borderColor: "#df4f5f",
                iconClassName: "bg-red-50 text-red-600",
                badgeClassName: "border-red-200 bg-red-50 text-red-700",
                progressClassName: "bg-red-500",
                textClassName: "text-red-700",
            };
        case "running":
            return {
                label: "进行中",
                borderColor: "#4387e8",
                iconClassName: "bg-blue-50 text-blue-600",
                badgeClassName: "border-blue-200 bg-blue-50 text-blue-700",
                progressClassName: "bg-blue-500",
                textClassName: "text-blue-700",
            };
        case "queued":
            return {
                label: "排队中",
                borderColor: "#d89c24",
                iconClassName: "bg-amber-50 text-amber-600",
                badgeClassName: "border-amber-200 bg-amber-50 text-amber-700",
                progressClassName: "bg-amber-500",
                textClassName: "text-amber-700",
            };
        default:
            return {
                label: "已取消",
                borderColor: "#94a3b8",
                iconClassName: "bg-slate-50 text-slate-500",
                badgeClassName: "border-slate-200 bg-slate-50 text-slate-600",
                progressClassName: "bg-slate-400",
                textClassName: "text-slate-600",
            };
    }
}

function AudioPlayback({ url }: { url: string }) {
    const [failed, setFailed] = useState(false);
    return (
        <div className="space-y-2">
            <audio aria-label="生成音频试听" controls preload="none" src={url} className="w-full" onError={() => setFailed(true)} onCanPlay={() => setFailed(false)} />
            {failed && <Callout tone="warning">浏览器无法播放此音频，可下载后试听，或重新生成 MP3 / WAV 格式。</Callout>}
        </div>
    );
}

function AudioTaskCard({ summary, userId, onReuse }: { summary: GenerationTask; userId: string; onReuse: (task: GenerationTask) => void }) {
    const { message } = App.useApp();
    const queryClient = useQueryClient();
    const [busy, setBusy] = useState<"retry" | "cancel" | "download" | "delete" | null>(null);
    const detail = useQuery({
        queryKey: [...queryRoot(userId), "task", summary.id, summary.updatedAt],
        queryFn: ({ signal }) => queryGenerationTask(summary.id, { signal }),
        staleTime: 30000,
        gcTime: 60000,
        retry: false,
    });
    const task = detail.data || summary;
    const output = audioWorkbenchOutput(task);
    const snapshot = audioWorkbenchSnapshot(task);
    const active = isActiveWorkbenchTask(task);
    const statusMeta = audioTaskStatusMeta(task.status);
    const progress = Math.min(100, Math.max(8, typeof task.progress === "number" ? (task.progress <= 1 ? task.progress * 100 : task.progress) : task.status === "queued" ? 12 : 61));
    const [, setElapsedTick] = useState(0);
    useEffect(() => {
        if (!active) return;
        const timer = window.setInterval(() => setElapsedTick((tick) => tick + 1), 1000);
        return () => window.clearInterval(timer);
    }, [active]);
    const generationDuration = formatAudioWorkbenchGenerationDuration(audioWorkbenchGenerationDurationMs(task));
    const act = async (action: "retry" | "cancel" | "download" | "delete") => {
        if (busy) return;
        setBusy(action);
        try {
            if (action === "download") await downloadBrowserMedia({ ...output, variant: "original" });
            else if (action === "delete") {
                await deleteGenerationTask(task.id);
                await queryClient.invalidateQueries({ queryKey: queryRoot(userId) });
                void message.success("已删除音频记录");
            } else {
                if (action === "retry") await retryGenerationTask(task.id);
                else await cancelGenerationTask(task.id);
                window.dispatchEvent(new CustomEvent("wallet:updated"));
                await queryClient.invalidateQueries({ queryKey: queryRoot(userId) });
                void message.success(action === "retry" ? "已提交重试任务" : "已取消任务");
            }
        } catch (error) {
            void message.error(errorText(error));
        } finally {
            setBusy(null);
        }
    };
    return (
        <article className="relative space-y-3 overflow-hidden rounded-xl border border-border border-l-4 bg-surface p-4 shadow-sm" style={{ borderLeftColor: statusMeta.borderColor }}>
            <div className="flex flex-wrap items-start justify-between gap-2">
                <div className="min-w-0">
                    <div className="flex min-w-0 items-center gap-2">
                        <span className={`grid size-7 shrink-0 place-items-center rounded-lg ${statusMeta.iconClassName}`}>
                            <AudioLines className="size-4" aria-hidden />
                        </span>
                        <span className="truncate text-sm font-medium">{task.model || "语音合成"}</span>
                    </div>
                    <span className={`mt-2 inline-flex items-center gap-1.5 rounded-md border px-2 py-1 text-xs font-semibold ${statusMeta.badgeClassName}`}>
                        <span className={`size-1.5 rounded-full bg-current ${active ? "animate-pulse" : ""}`} aria-hidden />
                        {statusMeta.label}
                    </span>
                </div>
                <time className="text-xs text-muted-foreground" dateTime={task.createdAt}>
                    {new Date(task.createdAt).toLocaleString()}
                </time>
            </div>
            <div className="relative">
                <p className="max-h-36 overflow-y-auto whitespace-pre-wrap break-words rounded-lg border border-border/70 bg-muted/20 p-3 pr-12 text-sm leading-relaxed">{task.prompt}</p>
                <Button
                    type="default"
                    size="small"
                    icon={<Copy className="size-3.5" />}
                    aria-label="复制文本"
                    title="复制文本"
                    className="!absolute right-2 top-2 z-10 !h-8 !w-8 !p-0"
                    disabled={!task.prompt.trim()}
                    onClick={() =>
                        void copyTextToClipboard(task.prompt)
                            .then(() => message.success("文本已复制"))
                            .catch(() => message.error("复制失败，请手动选择文本复制"))
                    }
                />
            </div>
            {detail.data && (
                <p className="text-xs text-muted-foreground">
                    音色：{snapshot.voice} · {snapshot.format.toUpperCase()} · 语速 {snapshot.speed}×
                </p>
            )}
            {generationDuration && (
                <p className="text-xs text-muted-foreground" title="从任务开始执行到完成的墙钟时间，不包含排队等待，也不是生成音频本身的播放时长">
                    生成耗时：{generationDuration}
                    {task.status === "queued" ? "（排队中）" : active ? "（进行中）" : "（不含排队）"}
                </p>
            )}
            {active && (
                <div className="space-y-2" role="status">
                    <div className={`flex items-center justify-between text-xs font-semibold ${statusMeta.textClassName}`}>
                        <span className="inline-flex items-center gap-1.5">
                            <Spin size="small" />
                            {task.status === "queued" ? "等待资源" : "调用生成模型"}
                        </span>
                        <span>{task.status === "queued" ? "排队等待" : "处理中"}</span>
                    </div>
                    <div className="h-1.5 overflow-hidden rounded-full bg-muted">
                        <div className={`h-full rounded-full transition-all ${statusMeta.progressClassName}`} style={{ width: `${progress}%` }} />
                    </div>
                    <p className="text-xs text-muted-foreground">{task.stage || (task.status === "queued" ? "前面的任务完成后将自动开始生成。" : "任务正在处理中，可以离开页面，稍后回来查看结果。")}</p>
                </div>
            )}
            {task.error && (
                <Callout tone="error" title="生成未完成">
                    {task.error}
                </Callout>
            )}
            {detail.isError && (
                <Callout
                    tone="warning"
                    action={
                        <Button size="small" onClick={() => void detail.refetch()}>
                            重试加载
                        </Button>
                    }
                >
                    音频详情加载失败：{errorText(detail.error)}
                </Callout>
            )}
            {task.status === "succeeded" && output.url && (output.format === "pcm" ? <Callout>PCM 是原始音频，请下载后用支持 PCM 的播放器打开。需要浏览器试听时请选择 MP3 或 WAV。</Callout> : <AudioPlayback key={output.url} url={output.url} />)}
            {task.status === "succeeded" && !output.url && !detail.isPending && !detail.isError && <Callout tone="warning">任务已完成，但未找到可播放的音频资源。请刷新记录，或在任务中心查看详情。</Callout>}
            <div className="flex flex-wrap items-center gap-2">
                {task.status === "succeeded" && output.url && (
                    <Button icon={<Download className="size-4" />} loading={busy === "download"} disabled={busy !== null} onClick={() => void act("download")}>
                        下载音频
                    </Button>
                )}
                {(task.status === "failed" || task.status === "cancelled") && (
                    <Button icon={<RotateCcw className="size-4" />} loading={busy === "retry"} disabled={busy !== null} onClick={() => void act("retry")}>
                        重试
                    </Button>
                )}
                {active && (
                    <Button icon={<Square className="size-3" />} loading={busy === "cancel"} disabled={busy !== null} onClick={() => void act("cancel")}>
                        取消任务
                    </Button>
                )}
                {!active && (
                    <Popconfirm title="确认删除这条音频记录？" description="删除后将从生成记录中移除，操作不可撤销。" okText="删除" okButtonProps={{ danger: true }} cancelText="取消" disabled={busy !== null} onConfirm={() => void act("delete")}>
                        <Button danger icon={<Trash2 className="size-4" />} loading={busy === "delete"} disabled={busy !== null}>
                            删除
                        </Button>
                    </Popconfirm>
                )}
                <Button type="default" disabled={!detail.data} onClick={() => onReuse(task)}>
                    使用这段文本和参数
                </Button>
                <span className="ml-auto text-xs text-muted-foreground" title={task.id}>
                    任务 {task.id.slice(0, 8)}
                </span>
            </div>
        </article>
    );
}

export default function AudioWorkbenchPage() {
    const { message } = App.useApp();
    const userId = useUserStore((state) => state.user?.id || "");
    const enabled = useUserStore((state) => state.features.audioWorkbenchEnabled);
    const config = useEffectiveConfig();
    const updateConfig = useConfigStore((state) => state.updateConfig);
    const queryClient = useQueryClient();
    const [prompt, setPrompt] = useState("");
    const [submitting, setSubmitting] = useState(false);
    const [submitError, setSubmitError] = useState("");
    const [historyPage, setHistoryPage] = useState(1);
    const models = selectableModelsByCapability(config, "audio").filter((model) => isSpeechWorkbenchModel(config, model));
    const selectedModel = models.includes(config.audioModel) ? config.audioModel : models[0] || "";
    const history = useQuery({
        queryKey: [...queryRoot(userId), "history", historyPage],
        queryFn: ({ signal }) => listWorkbenchTaskPage("audio", historyPage, HISTORY_PAGE_SIZE, signal),
        enabled: Boolean(userId) && enabled,
        refetchInterval: (query) => (query.state.status === "error" ? false : query.state.data?.active.some(isActiveWorkbenchTask) ? 4000 : 15000),
        refetchIntervalInBackground: false,
        retry: false,
    });
    const activeTasks = history.data?.active || [];
    const tasks = history.data?.items || [];
    const totalHistory = history.data?.total || 0;
    useEffect(() => {
        if (!history.isSuccess) return;
        const lastPage = Math.max(1, Math.ceil(totalHistory / HISTORY_PAGE_SIZE));
        if (historyPage > lastPage) setHistoryPage(lastPage);
    }, [history.isSuccess, historyPage, totalHistory]);
    const submit = async () => {
        if (submitting || !enabled) return;
        setSubmitError("");
        setSubmitting(true);
        try {
            if (!models.includes(selectedModel)) throw new Error("请选择可用的语音合成模型");
            const request = audioWorkbenchRequest(config, selectedModel, prompt);
            await createWorkbenchTask(request);
            setHistoryPage(1);
            await queryClient.invalidateQueries({ queryKey: queryRoot(userId) });
            void message.success("音频任务已提交，可在生成记录中查看进度");
        } catch (error) {
            setSubmitError(errorText(error));
        } finally {
            setSubmitting(false);
        }
    };
    const reuse = (task: GenerationTask) => {
        setPrompt(task.prompt);
        const snapshot = audioWorkbenchSnapshot(task);
        updateConfig("audioVoice", snapshot.voice);
        updateConfig("audioFormat", snapshot.format);
        updateConfig("audioSpeed", snapshot.speed);
        updateConfig("audioInstructions", snapshot.instructions);
        const model = models.find((value) => modelOptionName(value) === task.model || value === task.model);
        if (model) updateConfig("audioModel", model);
        void message.success("已填入文本和参数，可编辑后生成");
    };
    return (
        <WorkspacePage>
            <PageHeader title="音频生成" description="将台词、旁白或文案转成语音，试听并下载生成结果。" />
            <div className="mt-4 grid min-w-0 gap-5 xl:grid-cols-[minmax(320px,440px)_minmax(0,1fr)]">
                <section aria-label="语音合成设置" className="min-w-0 self-start rounded-xl border border-border bg-surface p-4 sm:p-5">
                    <Form layout="vertical" onFinish={() => void submit()}>
                        <Form.Item label="语音合成模型" required>
                            <ModelPicker
                                config={config}
                                capability="audio"
                                value={selectedModel}
                                onChange={(model) => updateConfig("audioModel", model)}
                                modelFilter={(model) => isSpeechWorkbenchModel(config, model)}
                                fullWidth
                                placeholder="选择语音合成模型"
                            />
                        </Form.Item>
                        {!models.length && (
                            <Callout tone="warning" className="mb-4">
                                暂无可用的语音合成模型，请管理员启用音频模型并配置价格。
                            </Callout>
                        )}
                        <Form.Item label="朗读文本" htmlFor="audio-workbench-prompt" required extra={`${Array.from(prompt).length} / 32000 字`}>
                            <Input.TextArea id="audio-workbench-prompt" value={prompt} onChange={(event) => setPrompt(event.target.value)} autoSize={{ minRows: 7, maxRows: 18 }} placeholder="输入想要朗读的台词、旁白或文案……" />
                        </Form.Item>
                        <Form.Item label="音色名称或 ID" htmlFor="audio-workbench-voice" extra="填写模型支持的音色；VoiceStudio 可填已保存的自定义音色名称或 ID。留空使用 alloy。">
                            <Input id="audio-workbench-voice" value={config.audioVoice} onChange={(event) => updateConfig("audioVoice", event.target.value)} placeholder="alloy 或自定义音色 ID" />
                        </Form.Item>
                        <div className="grid grid-cols-2 gap-3">
                            <Form.Item label="输出格式" htmlFor="audio-workbench-format">
                                <Select id="audio-workbench-format" value={normalizeAudioFormatValue(config.audioFormat)} options={openAIAudioFormatOptions} onChange={(value) => updateConfig("audioFormat", value)} />
                            </Form.Item>
                            <Form.Item label="语速" htmlFor="audio-workbench-speed">
                                <InputNumber
                                    id="audio-workbench-speed"
                                    min={0.25}
                                    max={4}
                                    step={0.05}
                                    value={config.audioSpeed ? Number(config.audioSpeed) : null}
                                    onChange={(value) => updateConfig("audioSpeed", value === null ? "" : String(value))}
                                    className="!w-full"
                                />
                            </Form.Item>
                        </div>
                        <Form.Item label="朗读指令（可选）" htmlFor="audio-workbench-instructions" extra="例如温柔、舒缓地朗读；是否生效取决于所选模型。">
                            <Input.TextArea
                                id="audio-workbench-instructions"
                                value={config.audioInstructions}
                                onChange={(event) => updateConfig("audioInstructions", event.target.value)}
                                autoSize={{ minRows: 2, maxRows: 5 }}
                                placeholder="描述情绪、语气或朗读方式"
                            />
                        </Form.Item>
                        {submitError && (
                            <Callout tone="error" className="mb-4" role="alert">
                                {submitError}
                            </Callout>
                        )}
                        <Button type="primary" htmlType="submit" block size="large" icon={<AudioLines className="size-4" />} loading={submitting} disabled={!selectedModel || !prompt.trim() || !enabled}>
                            生成音频
                        </Button>
                    </Form>
                </section>
                <section aria-label="音频生成记录" className="min-w-0 space-y-3">
                    <div className="flex items-center justify-between gap-3">
                        <h2 className="font-semibold">
                            生成记录 <span className="text-xs font-normal text-muted-foreground">{history.isSuccess ? `历史记录 ${totalHistory} 条 · 每页 ${HISTORY_PAGE_SIZE} 条` : `每页 ${HISTORY_PAGE_SIZE} 条`}</span>
                        </h2>
                        <Button icon={<RefreshCw className="size-4" />} loading={history.isFetching} onClick={() => void queryClient.invalidateQueries({ queryKey: queryRoot(userId) })}>
                            刷新
                        </Button>
                    </div>
                    {history.isPending && (
                        <div className="p-8 text-center" role="status">
                            <Spin /> <span className="text-sm text-muted-foreground">正在加载历史记录…</span>
                        </div>
                    )}
                    {history.isError && (
                        <Callout
                            tone="error"
                            action={
                                <Button size="small" onClick={() => void history.refetch()}>
                                    重试
                                </Button>
                            }
                        >
                            加载记录失败：{errorText(history.error)}
                        </Callout>
                    )}
                    {history.isSuccess && activeTasks.length > 0 && (
                        <div className="space-y-3">
                            <h3 className="text-sm font-semibold">进行中的任务</h3>
                            {activeTasks.map((task) => (
                                <AudioTaskCard key={task.id} summary={task} userId={userId} onReuse={reuse} />
                            ))}
                        </div>
                    )}
                    {history.isSuccess && tasks.length === 0 && activeTasks.length === 0 && (
                        <div className="rounded-xl border border-dashed border-border p-10 text-center text-muted-foreground">
                            <AudioLines className="mx-auto mb-3 size-8" aria-hidden />
                            <p className="text-sm">还没有音频记录</p>
                            <p className="mt-1 text-xs">输入朗读文本并生成，结果会自动保存在这里。</p>
                        </div>
                    )}
                    {history.isSuccess && tasks.length > 0 && (
                        <div className="space-y-3">
                            <h3 className="text-sm font-semibold">历史记录</h3>
                            {tasks.map((task) => (
                                <AudioTaskCard key={task.id} summary={task} userId={userId} onReuse={reuse} />
                            ))}
                        </div>
                    )}
                    {history.isSuccess && totalHistory > 0 && <PaginationBar current={historyPage} pageSize={HISTORY_PAGE_SIZE} total={totalHistory} onChange={setHistoryPage} pageSizeOptions={[HISTORY_PAGE_SIZE]} alwaysShow itemLabel="条记录" />}
                </section>
            </div>
        </WorkspacePage>
    );
}
