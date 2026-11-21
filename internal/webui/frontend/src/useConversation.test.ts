import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Unauthorized } from "./api";
import type { Api } from "./api";
import type { StoredEvent } from "./types";
import { useConversation } from "./useConversation";

// Run the hook's effect with controllable browser events and timers, without
// mounting a UI or connecting to a real conversation.
const hooks = vi.hoisted(() => ({
  effects: [] as (() => void | (() => void))[],
  values: [] as unknown[],
}));
vi.mock("react", () => ({
  useEffect: (effect: () => void | (() => void)) => hooks.effects.push(effect),
  useState: (initial: unknown) => {
    const index = hooks.values.length;
    hooks.values.push(initial);
    return [initial, (value: unknown) => { hooks.values[index] = value; }];
  },
  useRef: (value: unknown) => ({ current: value }),
}));

class Stream extends EventTarget {
  static instances: Stream[] = [];
  onopen: ((event: Event) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  close = vi.fn();

  constructor(public url: string) {
    super();
    Stream.instances.push(this);
  }

  dispatchEvent(event: Event): boolean {
    const result = super.dispatchEvent(event);
    if (event.type === "open") this.onopen?.(event);
    if (event.type === "error") this.onerror?.(event);
    return result;
  }
}

describe("conversation stream errors", () => {
  let cleanup: (() => void) | undefined;

  beforeEach(() => {
    hooks.effects = [];
    hooks.values = [];
    Stream.instances = [];
    vi.useFakeTimers();
    vi.stubGlobal("EventSource", Stream);
    vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => setTimeout(() => callback(0), 16));
    vi.stubGlobal("cancelAnimationFrame", clearTimeout);
  });

  afterEach(() => {
    cleanup?.();
    cleanup = undefined;
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  const events = () => hooks.values[0] as StoredEvent[];
  const errorText = () => hooks.values[3] as string;
  const latest = () => Stream.instances[Stream.instances.length - 1];
  const start = async (sessionMock?: ReturnType<typeof vi.fn>) => {
    const api = {
      events: vi.fn().mockResolvedValue([{ seq: 59, type: "result", payload: { isError: false }, ts: 1 }]),
      session: sessionMock ?? vi.fn().mockResolvedValue({ id: "history", running: false }),
      streamURL: (_id: string, since: number) => `/stream?since=${since}`,
    } as unknown as Api;
    useConversation(api, "history", vi.fn(), vi.fn());
    cleanup = hooks.effects[0]() || undefined;
    await vi.advanceTimersByTimeAsync(0);
    return api;
  };

  it("reconnects idle history without adding errors or skipping the next real event", async () => {
    await start();
    for (let attempt = 0; attempt < 5; attempt++) {
      const stream = latest();
      stream.dispatchEvent(new Event("open"));
      stream.dispatchEvent(new Event("error"));
      expect(stream.close).toHaveBeenCalledOnce();
      await vi.advanceTimersByTimeAsync(1000);
      expect(latest().url).toBe("/stream?since=59");
      expect(events().map((event) => event.type)).toEqual(["result"]);
    }
    latest().dispatchEvent(new MessageEvent("token", { data: JSON.stringify({ text: "continued" }), lastEventId: "60" }));
    await vi.advanceTimersByTimeAsync(16);
    expect(events().map((event) => event.seq)).toEqual([59, 60]);
    expect(events()[1].payload).toEqual({ text: "continued" });
  });

  it("backs off exponentially when connections fail before opening and resets on open", async () => {
    await start();
    // First failure -> 1s
    latest().dispatchEvent(new Event("error"));
    await vi.advanceTimersByTimeAsync(999);
    expect(Stream.instances).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(Stream.instances).toHaveLength(2);

    // Second failure -> 2s
    latest().dispatchEvent(new Event("error"));
    await vi.advanceTimersByTimeAsync(1999);
    expect(Stream.instances).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(Stream.instances).toHaveLength(3);

    // Open resets backoff back to 1s.
    latest().dispatchEvent(new Event("open"));
    latest().dispatchEvent(new Event("error"));
    await vi.advanceTimersByTimeAsync(1000);
    expect(Stream.instances).toHaveLength(4);
  });

  it("stops reconnecting when auth probe rejects with Unauthorized", async () => {
    const session = vi.fn()
      .mockResolvedValueOnce({ id: "history", running: false })
      .mockRejectedValueOnce(new Unauthorized());
    await start(session);
    latest().dispatchEvent(new Event("error"));
    await vi.advanceTimersByTimeAsync(60_000);
    expect(Stream.instances).toHaveLength(1);
    expect(errorText()).toBe("Token rejected");
  });

  it("displays a real server error without treating it as a disconnect", async () => {
    await start();
    const stream = latest();
    stream.dispatchEvent(new MessageEvent("error", { data: JSON.stringify({ message: "Upstream unavailable" }), lastEventId: "60" }));
    await vi.advanceTimersByTimeAsync(3000);
    expect(events()[1]).toMatchObject({ seq: 60, type: "error", payload: { message: "Upstream unavailable" } });
    expect(stream.close).not.toHaveBeenCalled();
    expect(Stream.instances).toHaveLength(1);
  });

  it("re-queries session when events already contain a new user turn while session reported idle", async () => {
    const session = vi.fn()
      .mockResolvedValueOnce({ id: "history", running: false })
      .mockResolvedValueOnce({ id: "history", running: true });
    const api = {
      events: vi.fn().mockResolvedValue([
        { seq: 1, type: "user", payload: { text: "hello" }, ts: 1 },
      ]),
      session,
      streamURL: (_id: string, since: number) => `/stream?since=${since}`,
    } as unknown as Api;
    useConversation(api, "history", vi.fn(), vi.fn());
    cleanup = hooks.effects[0]() || undefined;
    await vi.advanceTimersByTimeAsync(0);
    expect(session).toHaveBeenCalledTimes(2);
    expect(hooks.values[1]).toBe(true);
  });
});
