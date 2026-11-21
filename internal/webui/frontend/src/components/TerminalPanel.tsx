import { useEffect, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { RotateCcw } from "lucide-react";
import { HttpError, type Api } from "../api";
import { useI18n } from "../i18n";

// The web terminal (gbx serve --shell), shown in a tab. It is its own chunk.
// Switching away unmounts it and leaves the shell running; coming back
// re-attaches and the server replays the recent output. Closing the tab ends
// the shell.

interface Props {
  api: Api;
  id: string | null;
  onId: (id: string | null) => void;
  onError: (error: unknown) => void;
}

function cssVar(name: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

export default function TerminalPanel({ api, id, onId, onError }: Props) {
  const { t } = useI18n();
  const host = useRef<HTMLDivElement>(null);
  const [exited, setExited] = useState(false);

  useEffect(() => {
    if (!host.current) return;
    const term = new Terminal({
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
      fontSize: 12.5,
      cursorBlink: true,
      allowProposedApi: false,
      theme: {
        background: cssVar("--canvas"),
        foreground: cssVar("--ink"),
        cursor: cssVar("--clay"),
        selectionBackground: "rgba(59,130,246,0.3)",
      },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host.current);
    fit.fit();

    let closed = false;
    let exitedLocal = false;
    let source: EventSource | null = null;
    let retry: ReturnType<typeof setTimeout> | undefined;
    let current = id;
    let lastOffset = 0;
    let backoff = 1000;
    let pending = "";
    let timer: ReturnType<typeof setTimeout> | undefined;
    let inputChain: Promise<void> = Promise.resolve();

    // Keystrokes go out in small batches, serialized so order is preserved.
    const flush = () => {
      timer = undefined;
      if (!current || !pending || closed) return;
      const terminalId = current;
      const data = pending;
      pending = "";
      inputChain = inputChain.then(() =>
        api.terminalInput(terminalId, data).then(
          () => {},
          () => {
            if (!closed) {
              exitedLocal = true;
              setExited(true);
            }
          },
        ),
      );
    };
    const input = term.onData((data) => {
      pending += data;
      if (!timer) timer = setTimeout(flush, 10);
    });

    const attach = (terminalId: string) => {
      if (closed || exitedLocal) return;
      const stream = new EventSource(api.terminalStreamURL(terminalId, lastOffset));
      source = stream;
      let opened = false;
      stream.onopen = () => {
        opened = true;
        backoff = 1000;
      };
      stream.addEventListener("output", (message) => {
        if (!(message instanceof MessageEvent)) return;
        opened = true;
        backoff = 1000;
        const parsed = JSON.parse(message.data as string) as { data: string; offset?: number };
        if (typeof parsed.offset === "number") {
          if (parsed.offset <= lastOffset) return;
          lastOffset = parsed.offset;
        }
        const bytes = Uint8Array.from(atob(parsed.data), (c) => c.charCodeAt(0));
        term.write(bytes);
      });
      stream.addEventListener("exit", () => {
        exitedLocal = true;
        setExited(true);
        term.write(`\r\n\x1b[2m[${t("terminalExited")}]\x1b[0m\r\n`);
        stream.close();
        if (source === stream) source = null;
      });
      stream.onerror = () => {
        if (source !== stream) return;
        stream.close();
        source = null;
        if (closed || exitedLocal) return;
        const delay = backoff;
        backoff = Math.min(backoff * 2, 30_000);
        const scheduleRetry = () => {
          if (!closed && !exitedLocal && source === null) retry = setTimeout(() => attach(terminalId), delay);
        };
        if (!opened) {
          void api.terminalResize(terminalId, term.cols, term.rows).then(
            () => scheduleRetry(),
            (err) => {
              if (closed) return;
              if (err instanceof HttpError && (err.status === 401 || err.status === 404 || err.status === 410)) {
                exitedLocal = true;
                setExited(true);
                if (err.status === 401) onError(err);
                return;
              }
              scheduleRetry();
            },
          );
          return;
        }
        scheduleRetry();
      };
    };

    (async () => {
      try {
        if (!current) {
          const opened = await api.terminalOpen(term.cols, term.rows);
          if (closed) {
            api.terminalClose(opened.id).catch(() => {});
            return;
          }
          current = opened.id;
          onId(current);
        } else {
          await api.terminalResize(current, term.cols, term.rows);
          if (closed) return;
        }
        attach(current);
        term.focus();
      } catch (error) {
        if (closed) return;
        // A shell that is gone (a restarted server) is replaced quietly.
        if (!(error instanceof HttpError && error.status === 404)) onError(error);
        onId(null);
      }
    })();

    const observer = new ResizeObserver(() => {
      fit.fit();
      if (current) api.terminalResize(current, term.cols, term.rows).catch(() => {});
    });
    observer.observe(host.current);

    return () => {
      closed = true;
      observer.disconnect();
      input.dispose();
      source?.close();
      if (retry) clearTimeout(retry);
      if (timer) clearTimeout(timer);
      term.dispose();
    };
    // The shell outlives re-renders; a new one starts only when id is cleared.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id === null]);

  return (
    <div className="relative flex h-full min-h-0 flex-col">
      {exited && (
        <button
          onClick={() => { setExited(false); onId(null); }}
          className="absolute right-3 top-2 z-10 flex items-center gap-1.5 rounded-lg border border-line bg-surface px-2.5 py-1 text-[13px] text-ink-2 shadow-sm hover:text-ink"
        >
          <RotateCcw size={13} /> {t("terminalNew")}
        </button>
      )}
      <div ref={host} className="min-h-0 flex-1 px-3 pb-2 pt-2" />
    </div>
  );
}
