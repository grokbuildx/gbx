import type { ThreadItem } from "./thread";
import { toolEdit, toolPlan, toolTodos } from "./thread";
import type { Translate } from "./i18n";

// Transcript export: Markdown for pasting anywhere, HTML for a standalone,
// readable copy. Both are built in the browser from the rendered thread.

export function transcriptMarkdown(title: string, items: ThreadItem[], t: Translate): string {
  const out: string[] = [`# ${title}`, ""];
  let speaker = "";
  const walk = (list: ThreadItem[], depth: number) => {
    const quote = depth ? "> ".repeat(depth) : "";
    for (const item of list) {
      if (depth === 0 && item.kind !== "user" && item.kind !== "day" && item.kind !== "turn-end" && speaker !== "assistant") {
        speaker = "assistant";
        out.push("## Grok", "");
      }
      switch (item.kind) {
        case "user":
          speaker = "user";
          out.push(`## ${t("you")}`, "", item.text || "", ...item.attachments.map((a) => `- 📎 ${a.name}`), "");
          break;
        case "text":
          // A blank line first, so text after a tool list is not read as part of it.
          out.push("", ...item.text.split("\n").map((line) => quote + line), "");
          break;
        case "thinking":
          out.push(`<details><summary>${t("thinking")}</summary>`, "", item.text, "", "</details>", "");
          break;
        case "tool": {
          const todos = toolTodos(item);
          const plan = toolPlan(item);
          if (todos) {
            out.push(...todos.map((todo) => `${quote}- [${todo.status === "completed" ? "x" : " "}] ${todo.content}`), "");
          } else if (plan) {
            out.push(`${quote}**${t("plan")}**`, "", plan, "");
          } else {
            const line = item.description || item.summary;
            out.push(`${quote}- \`${item.name}\` ${line}`);
            const edits = toolEdit(item);
            if (edits) out.push(...edits.filter((edit) => edit.path && edit.path !== line).map((edit) => `${quote}  - ${edit.path}`));
            if (item.children.length) {
              out.push("");
              walk(item.children, depth + 1);
            }
          }
          break;
        }
        case "error":
          out.push("```", item.message, "```", "");
          break;
      }
    }
  };
  walk(items, 0);
  return out.join("\n").replace(/\n{3,}/g, "\n\n");
}

// diffSection is a worktree's change as a Markdown section, fenced with more
// backticks than the diff itself contains.
export function diffSection(diff: { base: string; diff: string; truncated?: boolean }, branch: string, t: Translate): string {
  if (!diff.diff.trim()) return "";
  const longest = Math.max(2, ...(diff.diff.match(/`+/g) || []).map((run) => run.length));
  const fence = "`".repeat(longest + 1);
  const note = diff.truncated ? ` (${t("truncated")})` : "";
  return `\n## ${t("shareChanges", { branch, base: diff.base })}${note}\n\n${fence}diff\n${diff.diff.replace(/\n$/, "")}\n${fence}\n`;
}

const escapeHTML = (value: string): string =>
  value.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]!);

function safeHref(url: string): string {
  const trimmed = url.trim();
  if (/^(https?:|mailto:|#)/i.test(trimmed)) return escapeHTML(trimmed);
  return "";
}

function renderInlineMarkdown(text: string): string {
  // Split out inline code spans first so markup inside code is never interpreted.
  const parts: string[] = [];
  const codePattern = /(`+)([\s\S]*?)\1/g;
  let last = 0;
  for (const match of text.matchAll(codePattern)) {
    const start = match.index ?? 0;
    if (start > last) parts.push(renderInlineProse(text.slice(last, start)));
    parts.push(`<code>${escapeHTML(match[2])}</code>`);
    last = start + match[0].length;
  }
  if (last < text.length) parts.push(renderInlineProse(text.slice(last)));
  return parts.join("");
}

function formatEmphasis(escaped: string): string {
  let out = escaped.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
  out = out.replace(/~~([^~]+)~~/g, "<del>$1</del>");
  out = out.replace(/(^|[^\w*])\*([^*]+)\*(?!\w)/g, "$1<em>$2</em>");
  return out;
}

function renderInlineProse(text: string): string {
  const clean = text.replace(/\0/g, "");
  const anchors: string[] = [];
  const withPlaceholders = clean.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (_, label: string, url: string) => {
    const href = safeHref(url);
    const renderedLabel = formatEmphasis(escapeHTML(label));
    if (!href) return `\0A${anchors.push(renderedLabel) - 1}\0`;
    const anchor = `<a href="${href}" target="_blank" rel="noopener noreferrer">${renderedLabel}</a>`;
    return `\0A${anchors.push(anchor) - 1}\0`;
  });
  const formatted = formatEmphasis(escapeHTML(withPlaceholders));
  return formatted.replace(/\0A(\d+)\0/g, (_, idx: string) => anchors[Number(idx)] ?? "");
}

function renderMarkdownHTML(markdown: string): string {
  const lines = markdown.replace(/\r\n?/g, "\n").split("\n");
  const out: string[] = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) {
      i++;
      continue;
    }
    const detailsOpen = /^<details><summary>(.*)<\/summary>\s*$/.exec(line);
    if (detailsOpen) {
      const summary = detailsOpen[1];
      const inner: string[] = [];
      i++;
      while (i < lines.length && !/^<\/details>\s*$/.test(lines[i])) {
        inner.push(lines[i]);
        i++;
      }
      if (i < lines.length) i++;
      out.push(`<details><summary>${escapeHTML(summary)}</summary>${renderMarkdownHTML(inner.join("\n"))}</details>`);
      continue;
    }
    const fence = /^(`{3,}|~{3,})(.*)$/.exec(line);
    if (fence) {
      const marker = fence[1];
      const lang = fence[2].trim().split(/\s+/)[0] || "";
      const codeLines: string[] = [];
      i++;
      const closePattern = new RegExp(`^${marker[0]}{${marker.length},}\\s*$`);
      while (i < lines.length && !closePattern.test(lines[i])) {
        codeLines.push(lines[i]);
        i++;
      }
      if (i < lines.length) i++;
      const cls = lang ? ` class="language-${escapeHTML(lang)}"` : "";
      out.push(`<pre><code${cls}>${escapeHTML(codeLines.join("\n"))}</code></pre>`);
      continue;
    }
    const heading = /^(#{1,6})\s+(.*)$/.exec(line);
    if (heading) {
      const level = heading[1].length;
      out.push(`<h${level}>${renderInlineMarkdown(heading[2].trim())}</h${level}>`);
      i++;
      continue;
    }
    if (/^(-{3,}|\*{3,}|_{3,})\s*$/.test(line)) {
      out.push("<hr>");
      i++;
      continue;
    }
    if (/^>\s?/.test(line)) {
      const quoted: string[] = [];
      while (i < lines.length && /^>\s?/.test(lines[i])) {
        quoted.push(lines[i].replace(/^>\s?/, ""));
        i++;
      }
      out.push(`<blockquote>${renderMarkdownHTML(quoted.join("\n"))}</blockquote>`);
      continue;
    }
    if (i + 1 < lines.length && line.includes("|") && /^\s*\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)+\|?\s*$/.test(lines[i + 1])) {
      const parseRow = (row: string) =>
        row.trim().replace(/^\|/, "").replace(/\|$/, "").split("|").map((cell) => renderInlineMarkdown(cell.trim()));
      const headers = parseRow(line);
      i += 2;
      const rows: string[][] = [];
      while (i < lines.length && lines[i].includes("|") && lines[i].trim()) {
        rows.push(parseRow(lines[i]));
        i++;
      }
      const head = `<thead><tr>${headers.map((c) => `<th>${c}</th>`).join("")}</tr></thead>`;
      const body = rows.length ? `<tbody>${rows.map((r) => `<tr>${r.map((c) => `<td>${c}</td>`).join("")}</tr>`).join("")}</tbody>` : "";
      out.push(`<table>${head}${body}</table>`);
      continue;
    }
    if (/^\s*[-*+]\s+/.test(line) || /^\s*\d+\.\s+/.test(line)) {
      const ordered = /^\s*\d+\.\s+/.test(line);
      const tag = ordered ? "ol" : "ul";
      const pattern = ordered ? /^\s*\d+\.\s+(.*)$/ : /^\s*[-*+]\s+(.*)$/;
      const items: string[] = [];
      while (i < lines.length) {
        const match = pattern.exec(lines[i]);
        if (!match) break;
        const itemText = match[1];
        const task = /^\[([ xX])\]\s+(.*)$/.exec(itemText);
        if (task) {
          const checked = task[1].toLowerCase() === "x" ? " checked" : "";
          items.push(`<li><input type="checkbox" disabled${checked}> ${renderInlineMarkdown(task[2])}</li>`);
        } else {
          items.push(`<li>${renderInlineMarkdown(itemText)}</li>`);
        }
        i++;
      }
      out.push(`<${tag}>${items.join("")}</${tag}>`);
      continue;
    }
    const para: string[] = [];
    while (
      i < lines.length &&
      lines[i].trim() &&
      !/^<details><summary>.*<\/summary>\s*$/.test(lines[i]) &&
      !/^<\/details>\s*$/.test(lines[i]) &&
      !/^(`{3,}|~{3,})/.test(lines[i]) &&
      !/^#{1,6}\s+/.test(lines[i]) &&
      !/^>\s?/.test(lines[i]) &&
      !/^\s*[-*+]\s+/.test(lines[i]) &&
      !/^\s*\d+\.\s+/.test(lines[i]) &&
      !/^(-{3,}|\*{3,}|_{3,})\s*$/.test(lines[i])
    ) {
      para.push(lines[i]);
      i++;
    }
    out.push(`<p>${para.map(renderInlineMarkdown).join("<br>")}</p>`);
  }
  return out.join("\n");
}

export async function transcriptHTML(title: string, markdown: string): Promise<string> {
  const body = renderMarkdownHTML(markdown);
  const escaped = escapeHTML(title);
  return `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>${escaped}</title>
<style>
:root{color-scheme:light dark;--bg:#f9f8f4;--ink:#1f1e1c;--line:rgba(0,0,0,.1);--code:rgba(0,0,0,.04)}
@media (prefers-color-scheme:dark){:root{--bg:#1c1b19;--ink:#ebe8e0;--line:rgba(255,255,255,.12);--code:rgba(255,255,255,.05)}}
body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.6 system-ui,-apple-system,"Segoe UI",Roboto,"PingFang SC",sans-serif}
main{max-width:760px;margin:0 auto;padding:32px 20px}
pre,code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:13px}
pre{background:var(--code);border:1px solid var(--line);border-radius:10px;padding:12px;overflow:auto}
:not(pre)>code{background:var(--code);border-radius:5px;padding:1px 5px}
table{border-collapse:collapse}th,td{border:1px solid var(--line);padding:4px 10px}
blockquote{border-left:3px solid var(--line);margin:0;padding-left:12px;opacity:.85}
h2{margin-top:2em;border-top:1px solid var(--line);padding-top:1em}
</style></head><body><main>${body}</main></body></html>`;
}

export function download(name: string, content: string, type: string) {
  const url = URL.createObjectURL(new Blob([content], { type }));
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  document.body.appendChild(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

export function safeFileName(title: string): string {
  return (title.replace(/[\\/:*?"<>|\s]+/g, "-").replace(/^-+|-+$/g, "") || "conversation").slice(0, 80);
}
