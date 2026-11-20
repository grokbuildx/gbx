import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Api } from "./api";
import type { Conversation } from "./useConversation";
import type { StoredEvent } from "./types";
import { useMessageQueue } from "./useMessageQueue";

// Explicit renders exercise effect ordering without a DOM or real sessions.
const hooks = vi.hoisted(() => ({ slots: [] as unknown[], cursor: 0, effects: [] as (() => void)[] }));
vi.mock("react", () => ({
  useState: (initial: () => unknown) => {
    const index = hooks.cursor++;
    if (!(index in hooks.slots)) hooks.slots[index] = initial();
    return [hooks.slots[index], vi.fn()];
  },
  useRef: (initial: unknown) => {
    const index = hooks.cursor++;
    if (!(index in hooks.slots)) hooks.slots[index] = { current: initial };
    return hooks.slots[index];
  },
  useMemo: (compute: () => unknown) => compute(),
  useSyncExternalStore: (_subscribe: unknown, snapshot: () => unknown) => snapshot(),
  useEffect: (effect: () => void, deps: unknown[]) => {
    const index = hooks.cursor++;
    const previous = hooks.slots[index] as unknown[] | undefined;
    hooks.slots[index] = deps;
    if (!previous || deps.some((value, n) => !Object.is(value, previous[n]))) hooks.effects.push(effect);
  },
}));

const event = (seq: number, type = "result"): StoredEvent => ({ seq, type, ts: seq, payload: {} });
const item = (key: string) => ({ key, text: key, attachments: [`file-${key}`] });
function deferred() {
  let resolve!: (response: { running: boolean; steered?: boolean; seq?: number }) => void;
  const promise = new Promise<{ running: boolean; steered?: boolean; seq?: number }>((yes) => { resolve = yes; });
  return { promise, resolve };
}

function setup() {
  const api = { send: vi.fn().mockResolvedValue({ running: true, seq: 12 }) };
  const sent = vi.fn();
  const fail = vi.fn();
  const setRunning = vi.fn();
  function render(id: string, events: StoredEvent[], change: Partial<Conversation> = {}) {
    hooks.cursor = 0;
    const result = useMessageQueue(api as unknown as Api, id, { events, running: false, loading: false, error: "", setRunning, ...change }, sent, fail);
    for (const effect of hooks.effects.splice(0)) effect();
    return result;
  }
  return { api, sent, fail, setRunning, render };
}

beforeEach(() => {
  hooks.slots = [];
  hooks.effects = [];
  hooks.cursor = 0;
});

describe("useMessageQueue", () => {
  it("waits for a switched session's history, including repeated stale renders", async () => {
    const { render, api } = setup();
    const old = [event(90)];
    const { queue } = render("a", old);
    queue.add("b", item("b-first"));
    render("b", old);
    render("b", old);
    expect(api.send).not.toHaveBeenCalled();
    render("b", [], { loading: true });
    expect(api.send).not.toHaveBeenCalled();
    const history = [event(5)];
    render("b", history);
    expect(api.send).toHaveBeenCalledWith("b", "b-first", ["file-b-first"], false);
    await Promise.resolve();
    expect(queue.get("b").waiting).toMatchObject({ seq: 12 });
    render("b", history);
    expect(api.send).toHaveBeenCalledOnce();
  });

  it("does not flush a running, loading, or failed conversation", () => {
    const { render, api } = setup();
    const { queue } = render("a", []);
    queue.add("a", item("first"));
    const history = [event(5)];
    render("a", history, { running: true });
    render("a", history, { loading: true });
    render("a", history, { error: "Cannot load history" });
    expect(api.send).not.toHaveBeenCalled();
  });

  it("keeps a pending send tied to its session when another session is selected", async () => {
    const { render, api, sent, setRunning } = setup();
    const a = deferred();
    api.send.mockImplementationOnce(() => a.promise).mockResolvedValue({ running: true, seq: 12 });
    const { queue } = render("a", []);
    queue.add("a", item("first"));
    queue.add("b", item("first"));
    const history = [event(5)];
    const current = render("a", history, { running: true });
    const pending = current.sendNow("first");
    render("b", history);
    render("b", [event(8)]);
    expect(api.send).toHaveBeenNthCalledWith(1, "a", "first", ["file-first"], true);
    expect(api.send).toHaveBeenNthCalledWith(2, "b", "first", ["file-first"], false);
    a.resolve({ running: true, steered: true });
    await pending;
    expect(queue.get("a").items).toEqual([]);
    expect(queue.get("b").items).toEqual([]);
    expect(setRunning).not.toHaveBeenCalled();
    expect(sent).toHaveBeenCalledTimes(2);
  });

  it("keeps the item through steered user events and serializes end-of-turn auto-flush", async () => {
    const { render, api, setRunning } = setup();
    const response = deferred();
    api.send.mockImplementationOnce(() => response.promise).mockResolvedValue({ running: true, seq: 12 });
    const { queue } = render("a", []);
    queue.add("a", item("first"));
    queue.add("a", item("second"));
    const current = render("a", [event(5)], { running: true });
    const pending = current.sendNow("first");
    await current.sendNow("first");
    current.remove("first");
    render("a", [event(5), { ...event(6, "user"), payload: { steered: true } }], { running: true });
    expect(queue.get("a").items).toHaveLength(2);
    const finished = [event(10)];
    render("a", finished);
    expect(api.send).toHaveBeenCalledOnce();
    response.resolve({ running: true, steered: true });
    await pending;
    render("a", finished);
    expect(api.send).toHaveBeenNthCalledWith(2, "a", "second", ["file-second"], false);
    expect(setRunning).not.toHaveBeenCalled();
  });

  it("retains auto-flush failures across rerenders and switching away and back", async () => {
    const { render, api, fail } = setup();
    const error = new Error("offline");
    api.send.mockRejectedValue(error);
    const { queue } = render("a", []);
    queue.add("a", item("first"));
    render("a", [event(5)]);
    await vi.waitFor(() => expect(fail).toHaveBeenCalledWith(error));
    render("a", [event(5)]);
    render("b", [event(5)]);
    render("b", []);
    render("a", []);
    const current = render("a", [event(5)]);
    expect(api.send).toHaveBeenCalledOnce();
    expect(queue.get("a").items[0]).toEqual({ ...item("first"), failed: true });
    api.send.mockResolvedValue({ running: true, seq: 12 });
    await current.sendNow("first");
    expect(queue.get("a").items).toEqual([]);
  });
});
