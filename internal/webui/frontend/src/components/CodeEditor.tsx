import { useEffect, useRef } from "react";
import { EditorState, Compartment } from "@codemirror/state";
import { EditorView, keymap, lineNumbers, highlightActiveLine, highlightActiveLineGutter, drawSelection } from "@codemirror/view";
import { defaultKeymap, history, historyKeymap, indentWithTab } from "@codemirror/commands";
import { LanguageDescription, LanguageSupport, StreamLanguage, bracketMatching, indentOnInput, syntaxHighlighting } from "@codemirror/language";
import type { StreamParser } from "@codemirror/language";
import { highlightSelectionMatches, searchKeymap } from "@codemirror/search";
import { classHighlighter } from "@lezer/highlight";

// The editor of the file panel. It is its own chunk: the page loads it the
// first time a file is opened, and each curated language loads by file name.
const legacy = (parser: StreamParser<unknown>) => new LanguageSupport(StreamLanguage.define(parser));

const languages: LanguageDescription[] = [
  LanguageDescription.of({ name: "C", extensions: ["c", "h", "ino"], load: () => import("@codemirror/lang-cpp").then((m) => m.cpp()) }),
  LanguageDescription.of({ name: "C++", alias: ["cpp"], extensions: ["cpp", "c++", "cc", "cxx", "hpp", "h++", "hh", "hxx"], load: () => import("@codemirror/lang-cpp").then((m) => m.cpp()) }),
  LanguageDescription.of({ name: "CSS", extensions: ["css"], load: () => import("@codemirror/lang-css").then((m) => m.css()) }),
  LanguageDescription.of({ name: "Go", extensions: ["go"], load: () => import("@codemirror/lang-go").then((m) => m.go()) }),
  LanguageDescription.of({ name: "HTML", extensions: ["html", "htm", "hbs"], load: () => import("@codemirror/lang-html").then((m) => m.html()) }),
  LanguageDescription.of({ name: "Java", extensions: ["java"], load: () => import("@codemirror/lang-java").then((m) => m.java()) }),
  LanguageDescription.of({ name: "JavaScript", alias: ["js"], extensions: ["js", "mjs", "cjs"], load: () => import("@codemirror/lang-javascript").then((m) => m.javascript()) }),
  LanguageDescription.of({ name: "JSX", extensions: ["jsx"], load: () => import("@codemirror/lang-javascript").then((m) => m.javascript({ jsx: true })) }),
  LanguageDescription.of({ name: "TypeScript", alias: ["ts"], extensions: ["ts", "mts", "cts"], load: () => import("@codemirror/lang-javascript").then((m) => m.javascript({ typescript: true })) }),
  LanguageDescription.of({ name: "TSX", extensions: ["tsx"], load: () => import("@codemirror/lang-javascript").then((m) => m.javascript({ jsx: true, typescript: true })) }),
  LanguageDescription.of({ name: "JSON", extensions: ["json", "jsonc", "map"], load: () => import("@codemirror/lang-json").then((m) => m.json()) }),
  LanguageDescription.of({ name: "LESS", extensions: ["less"], load: () => import("@codemirror/lang-less").then((m) => m.less()) }),
  LanguageDescription.of({ name: "Markdown", extensions: ["md", "markdown", "mkd", "mdx"], load: () => import("@codemirror/lang-markdown").then((m) => m.markdown()) }),
  LanguageDescription.of({ name: "PHP", extensions: ["php", "phtml"], load: () => import("@codemirror/lang-php").then((m) => m.php()) }),
  LanguageDescription.of({ name: "Python", extensions: ["py", "pyi", "pyw"], load: () => import("@codemirror/lang-python").then((m) => m.python()) }),
  LanguageDescription.of({ name: "Rust", extensions: ["rs"], load: () => import("@codemirror/lang-rust").then((m) => m.rust()) }),
  LanguageDescription.of({ name: "Sass", extensions: ["sass", "scss"], load: () => import("@codemirror/lang-sass").then((m) => m.sass()) }),
  LanguageDescription.of({ name: "SQL", extensions: ["sql"], load: () => import("@codemirror/lang-sql").then((m) => m.sql()) }),
  LanguageDescription.of({ name: "Vue", extensions: ["vue"], load: () => import("@codemirror/lang-vue").then((m) => m.vue()) }),
  LanguageDescription.of({ name: "XML", extensions: ["xml", "svg", "plist", "xsd", "xsl"], load: () => import("@codemirror/lang-xml").then((m) => m.xml()) }),
  LanguageDescription.of({ name: "YAML", extensions: ["yaml", "yml"], load: () => import("@codemirror/lang-yaml").then((m) => m.yaml()) }),
  LanguageDescription.of({ name: "Shell", alias: ["bash", "sh", "zsh"], extensions: ["sh", "bash", "zsh", "ksh"], filename: /^\.(bashrc|zshrc|profile|env(\..+)?)$/, load: () => import("@codemirror/legacy-modes/mode/shell").then((m) => legacy(m.shell)) }),
  LanguageDescription.of({ name: "TOML", extensions: ["toml"], load: () => import("@codemirror/legacy-modes/mode/toml").then((m) => legacy(m.toml)) }),
  LanguageDescription.of({ name: "Dockerfile", filename: /^Dockerfile(\..+)?$/, load: () => import("@codemirror/legacy-modes/mode/dockerfile").then((m) => legacy(m.dockerFile)) }),
  LanguageDescription.of({ name: "diff", extensions: ["diff", "patch"], load: () => import("@codemirror/legacy-modes/mode/diff").then((m) => legacy(m.diff)) }),
  LanguageDescription.of({ name: "Properties", alias: ["ini"], extensions: ["properties", "ini", "cfg", "conf"], load: () => import("@codemirror/legacy-modes/mode/properties").then((m) => legacy(m.properties)) }),
  LanguageDescription.of({ name: "ProtoBuf", extensions: ["proto"], load: () => import("@codemirror/legacy-modes/mode/protobuf").then((m) => legacy(m.protobuf)) }),
  LanguageDescription.of({ name: "Ruby", extensions: ["rb"], filename: /^(Gemfile|Rakefile)$/, load: () => import("@codemirror/legacy-modes/mode/ruby").then((m) => legacy(m.ruby)) }),
  LanguageDescription.of({ name: "Lua", extensions: ["lua"], load: () => import("@codemirror/legacy-modes/mode/lua").then((m) => legacy(m.lua)) }),
  LanguageDescription.of({ name: "CMake", extensions: ["cmake"], filename: /^CMakeLists\.txt$/, load: () => import("@codemirror/legacy-modes/mode/cmake").then((m) => legacy(m.cmake)) }),
  LanguageDescription.of({ name: "PowerShell", extensions: ["ps1", "psd1", "psm1"], load: () => import("@codemirror/legacy-modes/mode/powershell").then((m) => legacy(m.powerShell)) }),
  LanguageDescription.of({ name: "Kotlin", extensions: ["kt", "kts"], load: () => import("@codemirror/legacy-modes/mode/clike").then((m) => legacy(m.kotlin)) }),
  LanguageDescription.of({ name: "C#", alias: ["csharp"], extensions: ["cs"], load: () => import("@codemirror/legacy-modes/mode/clike").then((m) => legacy(m.csharp)) }),
  LanguageDescription.of({ name: "Dart", extensions: ["dart"], load: () => import("@codemirror/legacy-modes/mode/clike").then((m) => legacy(m.dart)) }),
  LanguageDescription.of({ name: "Scala", extensions: ["scala"], load: () => import("@codemirror/legacy-modes/mode/clike").then((m) => legacy(m.scala)) }),
  LanguageDescription.of({ name: "Swift", extensions: ["swift"], load: () => import("@codemirror/legacy-modes/mode/swift").then((m) => legacy(m.swift)) }),
];

const theme = EditorView.theme({
  "&": { height: "100%", fontSize: "12.5px", backgroundColor: "transparent", color: "var(--ink)" },
  ".cm-scroller": { fontFamily: "var(--font-mono)", lineHeight: "1.55" },
  ".cm-content": { caretColor: "var(--ink)" },
  ".cm-gutters": { backgroundColor: "transparent", color: "var(--ink-3)", border: "none" },
  ".cm-activeLine, .cm-activeLineGutter": { backgroundColor: "var(--hover)" },
  "&.cm-focused .cm-selectionBackground, .cm-selectionBackground, ::selection": { backgroundColor: "color-mix(in srgb, #3b82f6 28%, transparent) !important" },
  "&.cm-focused": { outline: "none" },
  ".cm-searchMatch": { backgroundColor: "color-mix(in srgb, #f59e0b 30%, transparent)" },
  ".cm-panels": { backgroundColor: "var(--surface)", color: "var(--ink)", borderTop: "1px solid var(--line)" },
});

interface Props {
  value: string;
  filename: string;
  readOnly?: boolean;
  onChange: (value: string) => void;
  onSave: () => void;
}

export default function CodeEditor({ value, filename, readOnly, onChange, onSave }: Props) {
  const host = useRef<HTMLDivElement>(null);
  const view = useRef<EditorView | null>(null);
  const callbacks = useRef({ onChange, onSave });
  callbacks.current = { onChange, onSave };
  const language = useRef(new Compartment());

  useEffect(() => {
    if (!host.current) return;
    const editor = new EditorView({
      parent: host.current,
      state: EditorState.create({
        doc: value,
        extensions: [
          lineNumbers(),
          highlightActiveLineGutter(),
          highlightActiveLine(),
          drawSelection(),
          history(),
          indentOnInput(),
          bracketMatching(),
          highlightSelectionMatches(),
          syntaxHighlighting(classHighlighter),
          language.current.of([]),
          EditorState.readOnly.of(!!readOnly),
          keymap.of([
            { key: "Mod-s", preventDefault: true, run: () => (callbacks.current.onSave(), true) },
            indentWithTab,
            ...defaultKeymap,
            ...historyKeymap,
            ...searchKeymap,
          ]),
          EditorView.updateListener.of((update) => {
            if (update.docChanged) callbacks.current.onChange(update.state.doc.toString());
          }),
          theme,
        ],
      }),
    });
    view.current = editor;
    return () => {
      editor.destroy();
      view.current = null;
    };
    // The document is replaced through the effect below, not by remounting.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [readOnly]);

  // A reload from disk replaces the document.
  useEffect(() => {
    const editor = view.current;
    if (editor && editor.state.doc.toString() !== value) {
      editor.dispatch({ changes: { from: 0, to: editor.state.doc.length, insert: value } });
    }
  }, [value]);

  useEffect(() => {
    let cancelled = false;
    const description = LanguageDescription.matchFilename(languages, filename);
    if (!description) {
      view.current?.dispatch({ effects: language.current.reconfigure([]) });
      return;
    }
    description.load().then((support) => {
      if (!cancelled) view.current?.dispatch({ effects: language.current.reconfigure(support) });
    });
    return () => {
      cancelled = true;
    };
  }, [filename]);

  return <div ref={host} className="h-full min-h-0 overflow-hidden" />;
}
