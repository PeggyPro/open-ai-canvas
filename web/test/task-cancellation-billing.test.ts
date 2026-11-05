import { describe, expect, test } from "bun:test";

import { providerCancelStatusLabel } from "@/pages/tasks/task-shared";
import type { GenerationTask } from "@/services/api/task-center";

type TaskOverrides = Partial<GenerationTask>;

function task(overrides: TaskOverrides = {}): GenerationTask {
    return {
        id: "task-1",
        type: "canvas_audio",
        status: "cancelled",
        prompt: "hello",
        attempts: 1,
        createdAt: "2026-01-01T00:00:00Z",
        updatedAt: "2026-01-01T00:00:00Z",
        ...overrides,
    };
}

describe("confirmed provider cancellation billing copy", () => {
    test("confirmed audio cancellation never says that credits were returned", () => {
        expect(providerCancelStatusLabel(task({ providerCancelStatus: "confirmed" }))).toBe("上游已确认取消，音频费用已消费");
    });

    test("settled audio cancellation keeps consumed-fee wording", () => {
        expect(
            providerCancelStatusLabel(
                task({
                    providerCancelStatus: "confirmed",
                    billing: { amountMicrocredits: 1_000_000, reservedMicrocredits: 1_000_000, refundedMicrocredits: 0, status: "settled" },
                }),
            ),
        ).toBe("上游已确认取消，音频费用已消费");
    });

    test("a non-audio confirmed cancellation still reports a refund when billing is absent", () => {
        expect(providerCancelStatusLabel(task({ type: "canvas_video", providerCancelStatus: "confirmed" }))).toBe("上游已确认取消，积分已退回");
    });

    test("settled cancellation never reports a refund even for a non-audio task", () => {
        expect(
            providerCancelStatusLabel(
                task({
                    type: "canvas_video",
                    providerCancelStatus: "confirmed",
                    billing: { amountMicrocredits: 1_000_000, reservedMicrocredits: 1_000_000, refundedMicrocredits: 0, status: "settled" },
                }),
            ),
        ).toBe("上游已确认取消，费用已结算");
    });
});
