import { isValidElement } from "react";
import type { ReactElement, ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Api } from "../api";
import { translator } from "../i18n";
import type { Lang, MessageKey } from "../i18n";
import type { ConfigView } from "../types";
import { SettingsDialog } from "./SettingsDialog";

// Exercise the settings callbacks with persistent hook state in the Node test environment.
const hooks = vi.hoisted(() => ({
  index: 0,
  values: [] as unknown[],
  effects: [] as (() => void)[],
  lang: "en" as "en" | "zh",
}));
vi.mock("react", async (importOriginal) => ({
  ...await importOriginal<typeof import("react")>(),
  useState: (initial: unknown) => {
    const index = hooks.index++;
    if (!(index in hooks.values)) hooks.values[index] = typeof initial === "function" ? initial() : initial;
    return [hooks.values[index], (value: unknown) => {
      hooks.values[index] = typeof value === "function" ? value(hooks.values[index]) : value;
    }];
  },
  useRef: (initial: unknown) => {
    const index = hooks.index++;
    if (!(index in hooks.values)) hooks.values[index] = { current: initial };
    return hooks.values[index];
  },
  useEffect: (effect: () => void, dependencies: unknown[]) => {
    const index = hooks.index++;
    const previous = hooks.values[index] as unknown[] | undefined;
    if (!previous || dependencies.some((value, i) => !Object.is(value, previous[i]))) hooks.effects.push(effect);
    hooks.values[index] = dependencies;
  },
  useMemo: (compute: () => unknown) => compute(),
  useCallback: (callback: unknown) => callback,
}));
vi.mock("../i18n", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../i18n")>();
  return { ...actual, useI18n: () => ({ lang: hooks.lang, t: actual.translator(hooks.lang) }) };
});

type Element = ReactElement<Record<string, unknown>>;
function nodes(root: ReactNode): Element[] {
  if (Array.isArray(root)) return root.flatMap(nodes);
  if (!isValidElement<Record<string, unknown>>(root)) return [];
  return [root, ...nodes(root.props.children as ReactNode)];
}

const config = (patch: Partial<ConfigView> = {}): ConfigView => ({
  active: { normal: "default" }, lang: "en", bypassMode: false, providers: {}, ...patch,
});
const fetchRequest = vi.fn();

beforeEach(() => {
  hooks.index = 0;
  hooks.values = [];
  hooks.effects = [];
  hooks.lang = "en";
  fetchRequest.mockReset().mockImplementation(async () => new Response(JSON.stringify(config())));
  vi.stubGlobal("fetch", fetchRequest);
  vi.stubGlobal("confirm", vi.fn(() => true));
});
afterEach(() => vi.unstubAllGlobals());

function mountGeneral(current: ConfigView | null) {
  const api = new Api("test-token");
  const onChanged = vi.fn().mockResolvedValue(undefined);
  const dialog = SettingsDialog({ api, workspaceApi: api, config: current, loadError: "", onClose: vi.fn(), onChanged });
  const general = nodes(dialog).find((node) => node.props.config === current && node.props.onChanged === onChanged)!;
  const component = general.type as (props: typeof general.props) => ReactNode;
  hooks.values = [];
  hooks.effects = [];
  let tree: ReactNode;
  const render = () => {
    hooks.index = 0;
    tree = component(general.props);
    if (hooks.effects.length) {
      hooks.effects.splice(0).forEach((effect) => effect());
      hooks.index = 0;
      tree = component(general.props);
    }
  };
  const field = (key: MessageKey) => nodes(tree).find((node) => node.props.label === translator(hooks.lang)(key))!.props.children as Element;
  const saveButton = () => nodes(tree).find((node) => node.props.primary)!;
  render();
  return {
    onChanged,
    checked: (key: MessageKey) => field(key).props.checked,
    toggle: (key: MessageKey, checked: boolean) => {
      (field(key).props.onChange as (event: { target: { checked: boolean } }) => void)({ target: { checked } });
      render();
    },
    select: (key: MessageKey, value: string) => {
      (field(key).props.onChange as (value: string) => void)(value);
      render();
    },
    save: async () => {
      const pending = (saveButton().props.onClick as () => Promise<void>)();
      render();
      await pending;
      render();
    },
    disabled: () => saveButton().props.disabled,
    status: () => nodes(tree).find((node) => "text" in node.props)!.props.text,
    hint: () => nodes(tree).find((node) => node.props.id === "sandbox-hint")!.props.children,
  };
}

function requestBody() {
  const [url, options] = fetchRequest.mock.calls.at(-1)!;
  expect(url).toBe("/api/config");
  expect(options.method).toBe("PUT");
  return JSON.parse(options.body);
}

describe("General sandbox settings", () => {
  it("defaults to on for older configs and disables saving until configuration is loaded", async () => {
    const unloaded = mountGeneral(null);
    expect(unloaded.checked("sandbox")).toBe(true);
    expect(unloaded.disabled()).toBe(true);
    await unloaded.save();
    expect(fetchRequest).not.toHaveBeenCalled();
    hooks.index = 0;
    hooks.values = [];
    const loaded = mountGeneral(config());
    expect(loaded.checked("sandbox")).toBe(true);
    expect(loaded.disabled()).toBe(false);
  });

  it.each(["workspace", "off"] as const)("preserves normal saves when sandbox is unchanged at %s", async (serveSandbox) => {
    const ui = mountGeneral(config({ serveSandbox }));
    expect(ui.checked("sandbox")).toBe(serveSandbox === "workspace");
    ui.select("activeProvider", "provider-two");
    ui.select("language", "zh");
    ui.toggle("bypass", true);
    expect(ui.checked("sandbox")).toBe(serveSandbox === "workspace");
    await ui.save();
    expect(requestBody()).toEqual({ active: { normal: "provider-two" }, lang: "zh", bypassMode: true });
    expect(confirm).not.toHaveBeenCalled();
    expect(ui.onChanged).toHaveBeenCalledOnce();
    expect(ui.status()).toBe(translator("en")("saved"));
  });

  it("confirms persisting an off startup override even when the checkbox is unchanged", async () => {
    const ui = mountGeneral(config({ serveSandbox: "off", savedServeSandbox: "workspace" }));
    await ui.save();
    expect(confirm).toHaveBeenCalledOnce();
    expect(requestBody()).toMatchObject({ serveSandbox: "off", confirmSandboxOff: true });
  });

  it("can persist an on startup override without confirmation", async () => {
    const ui = mountGeneral(config({ serveSandbox: "workspace", savedServeSandbox: "off" }));
    await ui.save();
    expect(confirm).not.toHaveBeenCalled();
    expect(requestBody()).toMatchObject({ serveSandbox: "workspace" });
  });

  it("cancels an off change without sending any settings or refreshing", async () => {
    vi.mocked(confirm).mockReturnValue(false);
    const ui = mountGeneral(config());
    ui.toggle("sandbox", false);
    ui.select("language", "zh");
    expect(confirm).not.toHaveBeenCalled();
    await ui.save();
    expect(confirm).toHaveBeenCalledWith(translator("en")("confirmSandboxOff"));
    expect(fetchRequest).not.toHaveBeenCalled();
    expect(ui.onChanged).not.toHaveBeenCalled();
    expect(ui.disabled()).toBe(false);
    expect(ui.checked("sandbox")).toBe(false);
  });

  it("sends explicit confirmation only after confirming a changed off setting", async () => {
    const ui = mountGeneral(config({ serveSandbox: "workspace", bypassMode: true }));
    ui.toggle("sandbox", false);
    expect(ui.checked("bypass")).toBe(true);
    ui.select("language", "zh");
    await ui.save();
    expect(confirm).toHaveBeenCalledOnce();
    expect(requestBody()).toEqual({ active: { normal: "default" }, lang: "zh", bypassMode: true, serveSandbox: "off", confirmSandboxOff: true });
    expect(ui.onChanged).toHaveBeenCalledOnce();
  });

  it("enables the sandbox without confirmation or changing approval settings", async () => {
    const ui = mountGeneral(config({ serveSandbox: "off", bypassMode: true }));
    ui.toggle("sandbox", true);
    await ui.save();
    expect(confirm).not.toHaveBeenCalled();
    expect(requestBody()).toEqual({ active: { normal: "default" }, lang: "en", bypassMode: true, serveSandbox: "workspace" });
  });

  it("blocks duplicate saves through both the request and refresh", async () => {
    let finishRequest!: (response: Response) => void;
    fetchRequest.mockImplementationOnce(() => new Promise<Response>((resolve) => { finishRequest = resolve; }));
    let finishRefresh!: () => void;
    const ui = mountGeneral(config());
    ui.onChanged.mockImplementationOnce(() => new Promise<void>((resolve) => { finishRefresh = resolve; }));
    ui.toggle("sandbox", false);
    const pending = ui.save();
    expect(ui.disabled()).toBe(true);
    await ui.save();
    expect(fetchRequest).toHaveBeenCalledOnce();
    expect(confirm).toHaveBeenCalledOnce();
    finishRequest(new Response(JSON.stringify(config({ serveSandbox: "off" }))));
    await vi.waitFor(() => expect(ui.onChanged).toHaveBeenCalledOnce());
    await ui.save();
    expect(fetchRequest).toHaveBeenCalledOnce();
    expect(ui.disabled()).toBe(true);
    finishRefresh();
    await pending;
    expect(ui.disabled()).toBe(false);
  });

  it("displays server request errors and requires confirmation again when retrying off", async () => {
    fetchRequest.mockImplementationOnce(async () => new Response(JSON.stringify({ error: "Could not save sandbox default" }), { status: 500 }));
    const ui = mountGeneral(config());
    ui.toggle("sandbox", false);
    await ui.save();
    expect(ui.status()).toBe("Could not save sandbox default");
    expect(ui.disabled()).toBe(false);
    expect(ui.onChanged).not.toHaveBeenCalled();
    await ui.save();
    expect(confirm).toHaveBeenCalledTimes(2);
    expect(ui.status()).toBe(translator("en")("saved"));
  });

  it.each(["en", "zh"] as Lang[])("localizes sandbox help and the warning in %s", async (lang) => {
    hooks.lang = lang;
    const ui = mountGeneral(config({ lang }));
    const t = translator(lang);
    expect(ui.hint()).toBe(t("sandboxHint"));
    ui.toggle("sandbox", false);
    await ui.save();
    expect(confirm).toHaveBeenCalledWith(t("confirmSandboxOff"));
    if (lang === "en") expect(t("confirmSandboxOff")).toContain("write outside the workspace and launch programs");
    else expect(t("confirmSandboxOff")).not.toBe(translator("en")("confirmSandboxOff"));
  });
});
