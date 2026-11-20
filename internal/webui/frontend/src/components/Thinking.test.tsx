import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import { I18n, translator } from "../i18n";
import type { Lang } from "../i18n";
import { buildThread } from "../thread";
import type { StoredEvent } from "../types";
import { Thread } from "./Thread";

let seq = 0;
const ev = (type: string, payload: unknown): StoredEvent => ({ seq: ++seq, type, payload, ts: 1_700_000_000_000 });

function render(events: StoredEvent[], lang: Lang = "en") {
  const items = buildThread(events);
  return renderToStaticMarkup(
    <I18n.Provider value={{ lang, t: translator(lang) }}>
      <Thread items={items} running={false} loading={false} workspace="/w" route="" attachmentURL={(id) => id} onPermission={vi.fn()} />
    </I18n.Provider>,
  );
}

describe("Thought blocks", () => {
  it.each(["en", "zh"] as Lang[])("gives every collapsed trace a readable first line in %s", (lang) => {
    const html = render([
      ev("thinking", { text: "First trace" }),
      ev("token", { text: "reply" }),
      ev("thinking", { text: "\n\nSecond trace\nwith detail" }),
    ], lang);
    const summaries = [...html.matchAll(/<summary[^>]*>(?:(?!<\/summary>)[\s\S])*?<\/summary>/g)].filter((match) => match[0].includes(translator(lang)("thinking")));
    expect(summaries).toHaveLength(2);
    expect(summaries[0][0]).toContain("First trace");
    expect(summaries[1][0]).toContain("Second trace");
    expect(summaries.every((match) => match[0].replace(/<[^>]+>/g, "").trim().length > translator(lang)("thinking").length)).toBe(true);
  });

  it("falls back to the Thought label for a trace without a first line", () => {
    const html = render([ev("thinking", { text: "   " })], "en");
    expect(html).not.toContain("<details");
  });
});
