import { describe, expect, it, vi } from "vitest";
import { MessageQueue } from "./queue";
import type { Queued } from "./queue";

const item = (key: string): Queued => ({ key, text: key, attachments: [`file-${key}`] });
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
const keys = (queue: MessageQueue, id = "a") => queue.get(id).items.map((entry) => entry.key);

function setup() {
  const queue = new MessageQueue();
  queue.add("a", item("first"));
  queue.add("a", item("second"));
  return queue;
}

describe("MessageQueue", () => {
  it("keeps text and attachments until Send now succeeds, leaving the remaining FIFO order", async () => {
    const queue = setup();
    const response = deferred<{ running: boolean; steered: boolean }>();
    const send = vi.fn(() => response.promise);
    const pending = queue.send("a", "second", send);
    expect(send).toHaveBeenCalledWith("a", "second", ["file-second"], true);
    expect(keys(queue)).toEqual(["first", "second"]);
    expect(queue.get("a").sending).toBe("second");
    response.resolve({ running: true, steered: true });
    expect(await pending).toBe(true);
    expect(keys(queue)).toEqual(["first"]);
    expect(queue.get("a").sending).toBeUndefined();
    expect(queue.get("a").waiting).toBeUndefined();
  });

  it("claims a session synchronously against duplicate clicks, removal, and auto-flush", async () => {
    const queue = setup();
    const response = deferred<{ running: boolean; steered: boolean }>();
    const send = vi.fn(() => response.promise);
    const pending = queue.send("a", "first", send);
    queue.remove("a", "first");
    expect(await queue.send("a", "first", send)).toBe(false);
    expect(await queue.send("a", "second", send)).toBe(false);
    queue.observe("a", 10);
    expect(await queue.flush("a", send)).toBe(false);
    expect(keys(queue)).toEqual(["first", "second"]);
    expect(send).toHaveBeenCalledOnce();
    queue.remove("a", "second");
    queue.add("a", item("third"));
    response.resolve({ running: true, steered: true });
    await pending;
    expect(keys(queue)).toEqual(["third"]);
  });

  it("isolates in-flight ownership across sessions, including matching item keys", async () => {
    const queue = setup();
    queue.add("b", item("first"));
    const a = deferred<{ running: boolean; steered: boolean }>();
    const send = vi.fn((id: string) => id === "a" ? a.promise : Promise.resolve({ running: true, steered: true }));
    const pending = queue.send("a", "first", send);
    await queue.send("b", "first", send);
    expect(keys(queue, "b")).toEqual([]);
    expect(keys(queue)).toEqual(["first", "second"]);
    a.resolve({ running: true, steered: true });
    await pending;
    expect(keys(queue)).toEqual(["second"]);
    expect(keys(queue, "b")).toEqual([]);
  });

  it("retains a failed Send now and its attachments without auto-retrying at turn end", async () => {
    const queue = setup();
    const send = vi.fn().mockRejectedValueOnce(new Error("interjection rejected")).mockResolvedValue({ running: true, steered: true });
    await expect(queue.send("a", "first", send)).rejects.toThrow("interjection rejected");
    expect(queue.get("a").items[0]).toEqual({ ...item("first"), failed: true });
    expect(queue.get("a").sending).toBeUndefined();
    queue.observe("a", 10);
    for (let n = 0; n < 4; n++) expect(await queue.flush("a", send)).toBe(false);
    expect(send).toHaveBeenCalledOnce();
    expect(await queue.send("a", "first", send)).toBe(true);
    expect(send).toHaveBeenLastCalledWith("a", "first", ["file-first"], true);
    expect(keys(queue)).toEqual(["second"]);
  });

  it("auto-flushes FIFO, keeps a pending item, and waits for that turn's result", async () => {
    const queue = setup();
    queue.observe("a", 10);
    const response = deferred<{ running: boolean; seq: number }>();
    const send = vi.fn().mockImplementationOnce(() => response.promise).mockResolvedValue({ running: true, seq: 30 });
    const pending = queue.flush("a", send);
    expect(send).toHaveBeenCalledWith("a", "first", ["file-first"], false);
    expect(keys(queue)).toEqual(["first", "second"]);
    expect(await queue.send("a", "first", send)).toBe(false);
    response.resolve({ running: true, seq: 11 });
    await pending;
    expect(keys(queue)).toEqual(["second"]);
    expect(await queue.flush("a", send)).toBe(false);
    queue.observe("a", 10);
    expect(await queue.flush("a", send)).toBe(false);
    queue.observe("a", 20);
    expect(await queue.flush("a", send)).toBe(true);
    expect(send).toHaveBeenLastCalledWith("a", "second", ["file-second"], false);
  });

  it("does not lose or endlessly retry a failed auto-flush; removal unblocks FIFO", async () => {
    const queue = setup();
    const send = vi.fn().mockRejectedValueOnce(new Error("offline")).mockResolvedValue({ running: true, seq: 12 });
    await expect(queue.flush("a", send)).rejects.toThrow("offline");
    expect(queue.get("a").items[0]).toEqual({ ...item("first"), failed: true });
    for (let n = 0; n < 4; n++) expect(await queue.flush("a", send)).toBe(false);
    queue.remove("a", "first");
    await queue.flush("a", send);
    expect(send).toHaveBeenCalledTimes(2);
    expect(send).toHaveBeenLastCalledWith("a", "second", ["file-second"], false);
  });

  it("protects a Send now that becomes an ordinary turn from the preceding result", async () => {
    const queue = setup();
    queue.observe("a", 5);
    const response = deferred<{ running: boolean; seq: number }>();
    const send = vi.fn(() => response.promise);
    const pending = queue.send("a", "first", send);
    queue.observe("a", 10);
    expect(await queue.flush("a", send)).toBe(false);
    response.resolve({ running: true, seq: 11 });
    await pending;
    expect(await queue.flush("a", send)).toBe(false);
    queue.observe("a", 20);
    expect(queue.get("a").waiting).toBeUndefined();
    expect(keys(queue)).toEqual(["second"]);
  });

  it("does not resurrect a finished turn when the HTTP response arrives after its result", async () => {
    const queue = setup();
    queue.observe("a", 5);
    const response = deferred<{ running: boolean; seq: number }>();
    const send = vi.fn(() => response.promise);
    const pending = queue.flush("a", send);
    queue.observe("a", 20);
    response.resolve({ running: true, seq: 11 });
    await pending;
    expect(queue.get("a").waiting).toBeUndefined();
    expect(keys(queue)).toEqual(["second"]);
  });

  it("allows ordinary FIFO to continue after a pending interjection succeeds at turn end", async () => {
    const queue = setup();
    const response = deferred<{ running: boolean; steered: boolean }>();
    const send = vi.fn().mockImplementationOnce(() => response.promise).mockResolvedValue({ running: true, seq: 12 });
    const pending = queue.send("a", "first", send);
    queue.observe("a", 10);
    response.resolve({ running: true, steered: true });
    await pending;
    expect(await queue.flush("a", send)).toBe(true);
    expect(send).toHaveBeenLastCalledWith("a", "second", ["file-second"], false);
  });

  it("keeps a fallback turn barrier when another item is interjected into it", async () => {
    const queue = setup();
    queue.add("a", item("third"));
    const send = vi.fn().mockResolvedValueOnce({ running: true, seq: 11 }).mockResolvedValue({ running: true, steered: true });
    await queue.send("a", "first", send);
    await queue.send("a", "second", send);
    expect(keys(queue)).toEqual(["third"]);
    expect(await queue.flush("a", send)).toBe(false);
    queue.observe("a", 20);
    expect(queue.get("a").waiting).toBeUndefined();
  });

  it("never sends a removed or already accepted item again", async () => {
    const queue = setup();
    const send = vi.fn().mockResolvedValue({ running: true, steered: true });
    queue.remove("a", "first");
    expect(await queue.send("a", "first", send)).toBe(false);
    await queue.send("a", "second", send);
    expect(await queue.send("a", "second", send)).toBe(false);
    expect(send).toHaveBeenCalledOnce();
  });

  it("publishes immutable snapshots and unsubscribes listeners", () => {
    const queue = setup();
    const listener = vi.fn();
    const unsubscribe = queue.subscribe(listener);
    const before = queue.snapshot();
    queue.add("b", item("third"));
    expect(queue.snapshot()).not.toBe(before);
    expect(listener).toHaveBeenCalledOnce();
    unsubscribe();
    queue.remove("b", "third");
    expect(listener).toHaveBeenCalledOnce();
  });
});
