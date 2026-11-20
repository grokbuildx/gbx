import type { StoredEvent } from "./types";

export interface ContextUsage {
  // Absent until the current turn reports a measurement.
  tokens?: number;
  window: number;
}

// Keep the window visible before usage arrives. Older events lack a window,
// so they retain the provider preset / model suffix fallback.
export function contextOf(events: StoredEvent[], selectedModel?: string, presetWindow?: number, defaultModel?: string): ContextUsage {
  let tokens: number | undefined;
  let reportedWindow: number | undefined;
  let model = selectedModel || defaultModel || "";
  let seenInit = false;
  for (let i = events.length - 1; i >= 0; i--) {
    const event = events[i];
    // A user message starts a turn; earlier turns' usage is stale.
    if (event.type === "user") break;
    if (event.type === "context") {
      const payload = event.payload as { tokens?: number; window?: number };
      if (tokens === undefined && typeof payload.tokens === "number" && Number.isFinite(payload.tokens) && payload.tokens >= 0) tokens = payload.tokens;
      if (reportedWindow === undefined && typeof payload.window === "number" && Number.isFinite(payload.window) && payload.window > 0) reportedWindow = payload.window;
    } else if (event.type === "init" && !selectedModel && !seenInit) {
      // The newest init is this turn's model. Init can follow its usage, so
      // read it without ending the scan.
      seenInit = true;
      model = String((event.payload as { model?: string }).model || model);
    }
  }
  const fallbackWindow = presetWindow && presetWindow > 0 ? presetWindow : /\[1m\]|-1m\b/i.test(model) ? 1_000_000 : 200_000;
  return { tokens, window: reportedWindow || fallbackWindow };
}
