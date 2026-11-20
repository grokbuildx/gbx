import type { ComponentProps } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import { I18n, translator } from "../i18n";
import type { Lang } from "../i18n";
import type { SandboxMode, Session } from "../types";
import { Composer } from "./Composer";

const session = (sandbox?: SandboxMode, permissionMode = "default"): Session => ({
  id: "conversation", title: "Test", provider: "default", model: "", effort: "", workspace: "/workspace",
  createdAt: "", updatedAt: "", turns: 1, usage: { inputTokens: 0, outputTokens: 0, costUsd: 0, durationMs: 0 },
  sandbox, permissionMode,
});

function markup(current: Session | null, defaultSandbox?: SandboxMode, lang: Lang = "en", context: ComponentProps<typeof Composer>["context"] = { window: 200_000 }, overrides: Partial<ComponentProps<typeof Composer>> = {}) {
  const props: ComponentProps<typeof Composer> = {
    running: false,
    context,
    workspace: "/workspace",
    workspaces: [{ id: "workspace", name: "workspace", path: "/workspace" }],
    workspaceId: "workspace",
    session: current,
    providers: [],
    capabilities: { slashCommands: [], mcpServers: [], defaultSandbox },
    draft: {},
    queued: [],
    scheduled: [],
    awaitingPermission: false,
    draftKey: "sandbox-test",
    onWorkspace: vi.fn(),
    onRoute: vi.fn(),
    onUpload: vi.fn().mockResolvedValue([]),
    onSend: vi.fn().mockResolvedValue(true),
    onStop: vi.fn(),
    registerAddFiles: vi.fn(),
    registerMention: vi.fn(),
    onSearchFiles: vi.fn().mockResolvedValue([]),
    onSendQueued: vi.fn(),
    onRemoveQueued: vi.fn(),
    onSchedule: vi.fn().mockResolvedValue(true),
    onUnschedule: vi.fn(),
    onManageConnectors: vi.fn(),
    ...overrides,
  };
  return renderToStaticMarkup(<I18n.Provider value={{ lang, t: translator(lang) }}><Composer {...props} /></I18n.Provider>);
}

function indicator(html: string, lang: Lang = "en") {
  const t = translator(lang);
  const match = html.match(new RegExp(`<span[^>]*>\\s*<svg[^>]*>[\\s\\S]*?<\\/svg>(${t("sandboxOn")}|${t("sandboxOff")})<\\/span>`));
  expect(match, "sandbox is a read-only span, not a route control").not.toBeNull();
  return match![1];
}

describe("Composer sandbox indicator", () => {
  it.each([undefined, "workspace", "off"] as const)("uses the current server default %s for new conversations", (mode) => {
    expect(indicator(markup(null, mode))).toBe(translator("en")(mode === "off" ? "sandboxOff" : "sandboxOn"));
  });

  it.each([
    ["workspace", "off", "sandboxOn"],
    ["off", "workspace", "sandboxOff"],
    [undefined, "off", "sandboxOn"],
    [undefined, undefined, "sandboxOn"],
  ] as const)("keeps session mode %s when the server default is %s", (pinned, server, key) => {
    const html = markup(session(pinned), server);
    expect(indicator(html)).toBe(translator("en")(key));
    expect(html).toContain(translator("en")("sandboxSessionHint"));
  });

  it.each(["default", "acceptEdits", "plan", "bypassPermissions"])("is independent of approval mode %s", (permissionMode) => {
    expect(indicator(markup(session("workspace", permissionMode), "off"))).toBe(translator("en")("sandboxOn"));
    expect(indicator(markup(session("off", permissionMode), "workspace"))).toBe(translator("en")("sandboxOff"));
  });

  it.each(["en", "zh"] as Lang[])("localizes both indicator states in %s", (lang) => {
    const t = translator(lang);
    expect(indicator(markup(null, "workspace", lang), lang)).toBe(t("sandboxOn"));
    expect(indicator(markup(null, "off", lang), lang)).toBe(t("sandboxOff"));
  });
});


describe("Composer context indicator", () => {
  it.each(["en", "zh"] as Lang[])("always displays waiting usage and the window in %s", (lang) => {
    const t = translator(lang);
    for (const current of [null, session()]) {
      const html = markup(current, undefined, lang);
      expect(html).toContain(t("contextLabel"));
      expect(html).toContain(t("contextPending"));
      expect(html).toContain("— / 200.0k");
      expect(html).toContain(t("contextWaiting", { total: (200_000).toLocaleString() }));
    }
  });

  it.each(["en", "zh"] as Lang[])("shows usage inline and exact counts in its accessible label in %s", (lang) => {
    const html = markup(session(), undefined, lang, { tokens: 42_500, window: 200_000 });
    expect(html).toContain("42.5k / 200.0k");
    expect(html).toContain("· 21%");
    expect(html).toContain(translator(lang)("contextUsed", { used: (42_500).toLocaleString(), total: (200_000).toLocaleString(), percent: 21 }));
    expect(html).not.toContain(translator(lang)("contextPending"));
  });

  it("distinguishes zero from unknown and preserves over-capacity percentages", () => {
    expect(markup(session(), undefined, "en", { tokens: 0, window: 1_000_000 })).toContain("0 / 1.0M");
    expect(markup(session(), undefined, "en", { tokens: 220_000, window: 200_000 })).toContain("· 110%");
  });
});

function buttons(html: string, label: string) {
  return [...html.matchAll(/<button\b[^>]*>[\s\S]*?<\/button>/g)].map((match) => match[0]).filter((button) => button.includes(`aria-label="${label}"`));
}

describe("Composer queued Send now", () => {
  it.each(["en", "zh"] as Lang[])("shows an accessible mobile-visible Send now and retains remove/stop in %s", (lang) => {
    const t = translator(lang);
    const html = markup(session(), undefined, lang, undefined, {
      running: true,
      queued: [{ key: "first", text: "Follow up", files: 2 }],
    });
    const [send] = buttons(html, t("queueSendNow"));
    expect(send).toContain('type="button"');
    expect(send).toContain(t("queueSendNowHint"));
    expect(send).toContain(`>${t("queueSendNow")}</button>`);
    expect(send).toContain("max-sm:min-h-11");
    expect(send).not.toMatch(/(?:max-sm:hidden|\sdisabled="")/);
    expect(html).toContain(t("queuedNext"));
    expect(html).toContain("+2");
    expect(buttons(html, t("remove"))).toHaveLength(1);
    expect(buttons(html, t("stop"))).toHaveLength(1);
  });

  it.each(["en", "zh"] as Lang[])("shows pending state, disabling all sends and only the in-flight removal in %s", (lang) => {
    const t = translator(lang);
    const html = markup(session(), undefined, lang, undefined, {
      running: true,
      queued: [{ key: "first", text: "Follow up", files: 0, sending: true }, { key: "second", text: "Later", files: 0 }],
    });
    const sends = buttons(html, t("queueSendNow"));
    expect(sends).toHaveLength(2);
    for (const send of sends) expect(send).toContain('disabled=""');
    expect(sends[0]).toContain('aria-busy="true"');
    expect(sends[0]).toContain(t("queueSending"));
    expect(sends[0]).toContain("animate-spin");
    const removals = buttons(html, t("remove"));
    expect(removals[0]).toContain('disabled=""');
    expect(removals[1]).not.toContain('disabled=""');
    expect(buttons(html, t("stop"))[0]).not.toContain('disabled=""');
  });

  it.each(["en", "zh"] as Lang[])("keeps retry and remove available for attachment-only failures in %s", (lang) => {
    const t = translator(lang);
    const html = markup(session(), undefined, lang, undefined, {
      queued: [{ key: "first", text: "", files: 2, failed: true }],
    });
    expect(html).toContain(t("filesOnly", { n: 2 }));
    expect(html).toContain(t("queueFailed"));
    expect(html).not.toContain(t("queuedNext"));
    expect(buttons(html, t("queueSendNow"))[0]).not.toContain('disabled=""');
    expect(buttons(html, t("remove"))[0]).not.toContain('disabled=""');
  });
});
