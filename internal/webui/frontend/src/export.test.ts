import { describe, expect, it } from "vitest";
import { diffSection, transcriptHTML, transcriptMarkdown } from "./export";
import { buildThread } from "./thread";
import { translator } from "./i18n";

describe("diffSection", () => {
  const t = translator("en");
  it("fences the diff with more backticks than it holds", () => {
    const section = diffSection({ base: "main", diff: "+```js\n+x\n+```\n" }, "gbx/abc", t);
    expect(section).toContain("## Changes on gbx/abc since main");
    expect(section).toContain("````diff\n+```js");
    expect(section.trimEnd().endsWith("````")).toBe(true);
  });
  it("is empty without changes", () => {
    expect(diffSection({ base: "main", diff: "\n" }, "gbx/abc", t)).toBe("");
  });
  it("exports assistant turns under Grok heading", () => {
    const items = buildThread([
      { seq: 1, type: "user", payload: { text: "hello" }, ts: 1 },
      { seq: 2, type: "token", payload: { text: "world" }, ts: 2 },
    ]);
    const md = transcriptMarkdown("Session", items, t);
    expect(md).toContain("## You");
    expect(md).toContain("## Grok");
  });
});

describe("transcriptHTML", () => {
  it("renders markdown elements and escapes raw HTML safely", async () => {
    const md = [
      "# Title <script>alert(1)</script>",
      "",
      "Check **bold**, `code <tag>`, and [safe](https://example.com) vs [bad](javascript:alert(1)).",
      "",
      "- [x] done",
      "- [ ] todo",
      "",
      "```go",
      "fmt.Println(\"<ok>\")",
      "```",
    ].join("\n");
    const html = await transcriptHTML("My <Title>", md);
    expect(html).toContain("<title>My &lt;Title&gt;</title>");
    expect(html).toContain("<h1>Title &lt;script&gt;alert(1)&lt;/script&gt;</h1>");
    expect(html).toContain("<strong>bold</strong>");
    expect(html).toContain("<code>code &lt;tag&gt;</code>");
    expect(html).toContain('<a href="https://example.com"');
    expect(html).not.toContain("javascript:alert");
    expect(html).toContain('<input type="checkbox" disabled checked> done');
    expect(html).toContain('<pre><code class="language-go">fmt.Println(&quot;&lt;ok&gt;&quot;)</code></pre>');
  });

  it("preserves query ampersands and emphasis characters inside URLs and renders <details> safely", async () => {
    const md = [
      "See [query](https://example.com/search?a=1&b=2&c=**x**&d=~~y~~) and **outside**.",
      "",
      "<details><summary>Thinking <tag></summary>",
      "",
      "Step 1 with **bold** and <script>x</script>",
      "",
      "</details>",
    ].join("\n");
    const html = await transcriptHTML("Session", md);
    expect(html).toContain('href="https://example.com/search?a=1&amp;b=2&amp;c=**x**&amp;d=~~y~~"');
    expect(html).not.toContain("&amp;amp;");
    expect(html).toContain("<strong>outside</strong>");
    expect(html).toContain("<details><summary>Thinking &lt;tag&gt;</summary><p>Step 1 with <strong>bold</strong> and &lt;script&gt;x&lt;/script&gt;</p></details>");
  });
});
