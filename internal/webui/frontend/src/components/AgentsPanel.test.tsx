import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { agentsOf, buildThread } from "../thread";
import type { StoredEvent } from "../types";
import { AgentBanner, AgentsPanel } from "./AgentsPanel";

let seq = 0;
const ev = (type: string, payload: unknown): StoredEvent => ({ seq: ++seq, type, payload, ts: 1_700_000_000_000 });

function liveEvents(): StoredEvent[] {
  return [
    ev("tool_use", { id: "a", name: "Task" }),
    ev("task", { id: "a", status: "running", usage: { tokens: 10, toolUses: 99, durationMs: 1000 } }),
    ev("tool_use", { id: "b", name: "Task" }),
    ev("thinking", { text: "A thoughts", parent: "a" }),
    ev("token", { text: "A commentary", parent: "a" }),
    ev("tool_use", { id: "a:read", name: "Read", parent: "a" }),
    ev("tool_use", { id: "a:read", name: "Read", summary: "file", parent: "a" }),
    ev("tool_result", { id: "a:read", ok: true, parent: "a" }),
    ev("tool_use", { id: "a:nested", name: "Task", parent: "a" }),
    ev("thinking", { text: "Nested thoughts", parent: "a:nested" }),
    ev("token", { text: "Nested commentary", parent: "a:nested" }),
    ev("tool_use", { id: "a:nested:read", name: "Read", parent: "a:nested" }),
    ev("tool_use", { id: "a:nested:bash", name: "Bash", parent: "a:nested" }),
    ev("token", { text: "Nested reply", parent: "a:nested" }),
    ev("thinking", { text: "B thoughts", parent: "b" }),
    ev("tool_use", { id: "b:read", name: "Read", parent: "b" }),
    ev("token", { text: "B reply", parent: "b" }),
  ];
}

function steps(html: string): number[] {
  return [...html.matchAll(/(\d+) steps/g)].map((match) => Number(match[1]));
}

function panelSteps(events: StoredEvent[]): number[] {
  return steps(renderToStaticMarkup(<AgentsPanel items={buildThread(events)} running={false} selected={null} onSelect={() => {}} />));
}

function bannerSteps(events: StoredEvent[]): number[][] {
  return agentsOf(buildThread(events)).map(({ tool }) => steps(renderToStaticMarkup(<AgentBanner tool={tool} onBack={() => {}} />)));
}

describe("agent step counts", () => {
  it("counts only tool calls recursively while running, with sibling isolation", () => {
    const events = liveEvents();
    expect(panelSteps(events)).toEqual([4, 2, 1]);
    expect(bannerSteps(events)).toEqual([[4], [2], [1]]);
  });

  it("prefers authoritative usage, including zero, once completed", () => {
    const events = [
      ...liveEvents(),
      ev("tool_result", { id: "a:nested", ok: true, parent: "a" }),
      ev("task", { id: "a:nested", status: "completed", usage: { tokens: 10, toolUses: 0, durationMs: 1000 } }),
      ev("tool_result", { id: "a", ok: true }),
      ev("task", { id: "a", status: "completed", usage: { tokens: 20, toolUses: 9, durationMs: 2000 } }),
      ev("tool_result", { id: "b", ok: true }),
    ];
    expect(panelSteps(events)).toEqual([9, 1]);
    expect(bannerSteps(events)).toEqual([[9], [], [1]]);
  });

  it.each(["running", "completed"])("does not count thinking and text as steps when %s", (status) => {
    const events = [
      ev("tool_use", { id: "a", name: "Task" }),
      ev("task", { id: "a", status }),
      ev("thinking", { text: "Thinking", parent: "a" }),
      ev("token", { text: "Commentary", parent: "a" }),
      ev("thinking", { text: "More thinking", parent: "a" }),
      ev("token", { text: "Final reply", parent: "a" }),
      ev("tool_result", { id: "a", ok: true, output: "Final reply" }),
    ];
    expect(panelSteps(events)).toEqual([]);
    expect(bannerSteps(events)).toEqual([[]]);
  });
});
