import { describe, expect, it } from "vitest";
import { contextOf } from "./context";
import type { StoredEvent } from "./types";

const event = (type: string, payload: unknown): StoredEvent => ({ seq: 1, ts: 1, type, payload });
const init = (model = "test") => event("init", { model });
const usage = (tokens: number, window?: number) => event("context", { tokens, window });

describe("contextOf", () => {
  it("keeps a window and unknown usage for new conversations", () => {
    expect(contextOf([])).toEqual({ tokens: undefined, window: 200_000 });
    expect(contextOf([], "test[1m]")).toEqual({ tokens: undefined, window: 1_000_000 });
    expect(contextOf([], "", undefined, "test-1m")).toEqual({ tokens: undefined, window: 1_000_000 });
    expect(contextOf([], "test[1m]", 500_000)).toEqual({ tokens: undefined, window: 500_000 });
  });

  it("uses the newest measurement, including zero after compaction", () => {
    expect(contextOf([init(), usage(42_500), usage(0, 128_000)])).toEqual({ tokens: 0, window: 128_000 });
  });

  it("uses the reported window and supports historical events without it", () => {
    expect(contextOf([init(), usage(42_500, 128_000)], "test", 500_000)).toEqual({ tokens: 42_500, window: 128_000 });
    expect(contextOf([init("test[1m]"), usage(42_500)])).toEqual({ tokens: 42_500, window: 1_000_000 });
    expect(contextOf([init("test[1m]"), usage(42_500)], "test")).toEqual({ tokens: 42_500, window: 200_000 });
  });

  it("keeps a turn's usage across later init events", () => {
    expect(contextOf([init(), usage(42_500), init("test[1m]")])).toEqual({ tokens: 42_500, window: 1_000_000 });
    expect(contextOf([init(), usage(42_500), init(), usage(12_000)])).toEqual({ tokens: 12_000, window: 200_000 });
  });

  it("waits for the new turn instead of displaying the previous turn's usage", () => {
    const user = event("user", { text: "next" });
    expect(contextOf([init(), usage(42_500), event("result", {}), user])).toEqual({ tokens: undefined, window: 200_000 });
    expect(contextOf([init(), usage(42_500), event("result", {}), user, usage(12_000)])).toEqual({ tokens: 12_000, window: 200_000 });
  });

  it("adopts Grok's reported window even when the count is unknown", () => {
    expect(contextOf([init("test"), event("context", { window: 256_000 })], "test", 500_000)).toEqual({ tokens: undefined, window: 256_000 });
  });

  it("ignores invalid counts and windows", () => {
    for (const tokens of [-1, NaN, Infinity, null, "oops"]) {
      expect(contextOf([init(), event("context", { tokens, window: -10 })])).toEqual({ tokens: undefined, window: 200_000 });
    }
  });
});
