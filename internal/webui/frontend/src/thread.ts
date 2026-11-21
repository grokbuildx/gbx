import type {
  Attachment,
  Capabilities,
  InitPayload,
  ResultPayload,
  StoredEvent,
  TaskPayload,
  TokenPayload,
  ToolResultPayload,
  ToolUsePayload,
  UserPayload,
} from "./types";
import { diffLines, diffStats } from "./diff";

// A conversation's history is a flat event stream; the thread is what the page
// renders from it. buildThread is pure so it can be tested and recomputed on
// every streamed token. A sub-agent's events nest under the Task call that
// started it.

export type TextItem = { kind: "text"; key: string; text: string; subagent: boolean };
export type ThinkingItem = { kind: "thinking"; key: string; text: string };

export type ToolItem = {
  kind: "tool";
  key: string;
  id: string;
  name: string;
  summary: string;
  description: string;
  input?: Record<string, unknown>;
  subagent: boolean;
  status: "running" | "ok" | "err";
  output?: string;
  // What a sub-agent did inside this call (Task / Agent).
  children: ThreadItem[];
  // The background work the call started. Its tool_result only says the work
  // was launched; this says whether it still runs and how it ended.
  task?: Omit<TaskPayload, "id">;
  // When the call started and, for background work, when that ended (ms).
  startedAt: number;
  endedAt?: number;
  // The id of the sub-agent call this one was made in.
  parentId?: string;
};

export type PermissionItem = {
  kind: "permission";
  key: string;
  requestId: string;
  toolName: string;
  title: string;
  description: string;
  summary: string;
  reason: string;
  input?: Record<string, unknown>;
  canRemember: boolean;
  status: "pending" | "allow" | "deny" | "cancelled";
  message: string;
  mode: string;
};

export type ThreadItem =
  // A day separator; the label is rendered in the page's language.
  | { kind: "day"; key: string; ts: number }
  | { kind: "user"; key: string; text: string; attachments: Attachment[]; ts: number; steered?: boolean }
  | TextItem
  | ThinkingItem
  | ToolItem
  | { kind: "error"; key: string; message: string }
  | { kind: "queued"; key: string }
  | PermissionItem
  | { kind: "turn-end"; key: string; result: ResultPayload; model?: string; ts: number };

interface Scope {
  owner: ToolItem | null;
  items: ThreadItem[];
  text: TextItem | null;
  thinking: ThinkingItem | null;
  lastTool: ToolItem | null;
}

const makeScope = (owner: ToolItem | null, items: ThreadItem[]): Scope => ({
  owner,
  items,
  text: null,
  thinking: null,
  lastTool: null,
});

// ThreadReducer builds a conversation thread incrementally while preserving
// referential identity for untouched ThreadItem objects across streaming frames.
export class ThreadReducer {
  private root: Scope = makeScope(null, []);
  // Tool ids are only meaningful within a turn (some providers reuse them).
  // A background sub-agent can report after the turn that started it, so its
  // events fall back to the latest earlier call with that id.
  private tools = new Map<string, ToolItem>();
  private earlier = new Map<string, ToolItem>();
  private scopes = new Map<ToolItem, Scope>();
  private permissions = new Map<string, PermissionItem>();
  private model: string | undefined;
  private lastDay = "";

  items(): ThreadItem[] {
    return this.root.items;
  }

  private callOf(id: string): ToolItem | undefined {
    return this.tools.get(id) || this.earlier.get(id);
  }

  private inRunningAgent(tool: ToolItem): boolean {
    let pid = tool.parentId;
    const seen = new Set<string>();
    while (pid && !seen.has(pid)) {
      seen.add(pid);
      const owner = this.callOf(pid);
      if (!owner) break;
      if (owner.task?.status === "running") return true;
      pid = owner.parentId;
    }
    return false;
  }

  private scopeFor(parent?: string): Scope {
    if (!parent) return this.root;
    const tool = this.callOf(parent);
    if (!tool) return this.root;
    let child = this.scopes.get(tool);
    if (!child) {
      child = makeScope(tool, tool.children);
      this.scopes.set(tool, child);
    }
    return child;
  }

  apply(events: StoredEvent[]): ThreadItem[] {
    if (events.length === 0) return this.root.items;
    const dirty = new Set<object>();

    const cloneTool = (tool: ToolItem): ToolItem => {
      if (dirty.has(tool)) return tool;
      let parentScope: Scope = this.root;
      if (tool.parentId) {
        const parentTool = this.callOf(tool.parentId);
        if (parentTool) {
          const clonedParent = cloneTool(parentTool);
          parentScope = this.scopes.get(clonedParent) || this.root;
        }
      }
      cloneItems(parentScope);
      const next: ToolItem = { ...tool };
      dirty.add(next);
      const idx = parentScope.items.lastIndexOf(tool);
      if (idx >= 0) parentScope.items[idx] = next;
      if (parentScope.lastTool === tool) parentScope.lastTool = next;
      if (tool.id) {
        if (this.tools.get(tool.id) === tool) this.tools.set(tool.id, next);
        if (this.earlier.get(tool.id) === tool) this.earlier.set(tool.id, next);
      }
      const ownedScope = this.scopes.get(tool);
      if (ownedScope) {
        this.scopes.delete(tool);
        ownedScope.owner = next;
        this.scopes.set(next, ownedScope);
      }
      return next;
    };

    const cloneItems = (target: Scope) => {
      if (dirty.has(target.items)) return;
      target.items = target.items.slice();
      dirty.add(target.items);
      if (target.owner) {
        const owner = cloneTool(target.owner);
        owner.children = target.items;
      }
    };

    const pushItem = (target: Scope, item: ThreadItem) => {
      cloneItems(target);
      target.items.push(item);
      dirty.add(item);
    };

    const cloneText = (target: Scope): TextItem => {
      const current = target.text!;
      if (dirty.has(current)) return current;
      cloneItems(target);
      const next: TextItem = { ...current };
      dirty.add(next);
      const idx = target.items.lastIndexOf(current);
      if (idx >= 0) target.items[idx] = next;
      target.text = next;
      return next;
    };

    const cloneThinking = (target: Scope): ThinkingItem => {
      const current = target.thinking!;
      if (dirty.has(current)) return current;
      cloneItems(target);
      const next: ThinkingItem = { ...current };
      dirty.add(next);
      const idx = target.items.lastIndexOf(current);
      if (idx >= 0) target.items[idx] = next;
      target.thinking = next;
      return next;
    };

    const clonePermission = (item: PermissionItem): PermissionItem => {
      if (dirty.has(item)) return item;
      cloneItems(this.root);
      const next: PermissionItem = { ...item };
      dirty.add(next);
      const idx = this.root.items.lastIndexOf(item);
      if (idx >= 0) this.root.items[idx] = next;
      this.permissions.set(next.requestId, next);
      return next;
    };

    for (const event of events) {
      const payload = (event.payload || {}) as Record<string, unknown>;
      const parent = typeof payload.parent === "string" ? payload.parent : undefined;
      switch (event.type) {
        case "user": {
          const user = payload as unknown as UserPayload;
          const day = event.ts ? new Date(event.ts).toDateString() : "";
          if (day && day !== this.lastDay) {
            this.lastDay = day;
            pushItem(this.root, { kind: "day", key: `d${event.seq}`, ts: event.ts });
          }
          pushItem(this.root, { kind: "user", key: `u${event.seq}`, text: String(user.text || ""), attachments: user.attachments || [], ts: event.ts, steered: user.steered || undefined });
          this.root.text = this.root.thinking = this.root.lastTool = null;
          // A message sent into the running turn continues it: its calls and
          // model still belong to that turn.
          if (user.steered) break;
          for (const [id, tool] of this.tools) this.earlier.set(id, tool);
          this.tools = new Map();
          this.model = undefined;
          break;
        }
        case "init":
          this.model = (payload as unknown as InitPayload).model || this.model;
          // A later init in the same turn starts Claude Code's answer to
          // background work that finished: its text is a new paragraph.
          this.root.text = this.root.thinking = null;
          break;
        case "thinking": {
          const text = (payload as unknown as TokenPayload).text || "";
          const target = this.scopeFor(parent);
          if (target.thinking) {
            // Whitespace inside a trace separates its paragraphs.
            cloneThinking(target).text += text;
            break;
          }
          // Grok ends a trace with whitespace-only chunks. They must not start a
          // blank Thought block or reopen the one the previous step closed.
          if (!text.trim()) break;
          target.thinking = { kind: "thinking", key: `k${event.seq}`, text };
          pushItem(target, target.thinking);
          target.text = null;
          break;
        }
        case "token": {
          const token = payload as unknown as TokenPayload;
          const target = this.scopeFor(parent);
          // Legacy events mark sub-agent text without naming its call.
          const subagent = !!token.subagent && target === this.root;
          if (!target.text || target.text.subagent !== subagent) {
            target.text = { kind: "text", key: `t${event.seq}`, text: "", subagent };
            pushItem(target, target.text);
          }
          target.thinking = null;
          cloneText(target).text += token.text || "";
          break;
        }
        case "tool_use": {
          const use = payload as unknown as ToolUsePayload;
          const target = this.scopeFor(parent);
          target.text = target.thinking = null;
          const inputObj = use.input && typeof use.input === "object" ? (use.input as Record<string, unknown>) : undefined;
          const agentName = canonicalAgentName(use.name, inputObj);
          // A call is announced as it starts streaming and again, with its input,
          // once the assistant message is complete: one row for both.
          let tool = use.id ? this.tools.get(use.id) : undefined;
          if (!tool) {
            tool = {
              kind: "tool",
              key: `c${event.seq}`,
              id: use.id || "",
              name: agentName || use.name || "tool",
              summary: "",
              description: "",
              subagent: !!use.subagent && target === this.root,
              status: "running",
              children: [],
              startedAt: event.ts,
              parentId: target.owner?.id || undefined,
            };
            if (use.id) this.tools.set(use.id, tool);
            pushItem(target, tool);
          } else {
            tool = cloneTool(tool);
          }
          if (agentName) {
            tool.name = agentName;
          } else if (use.name && tool.name !== "Task" && tool.name !== "Agent") {
            tool.name = use.name;
          }
          if (use.summary) tool.summary = use.summary;
          if (use.description) tool.description = use.description;
          if (inputObj) tool.input = inputObj;
          target.lastTool = tool;
          break;
        }
        case "tool_result": {
          const result = payload as unknown as ToolResultPayload;
          const target = this.scopeFor(parent);
          const found = (result.id && (parent ? this.callOf(result.id) : this.tools.get(result.id))) || target.lastTool;
          if (found) {
            const tool = cloneTool(found);
            tool.status = result.ok === false ? "err" : "ok";
            tool.output = result.output || "";
          }
          target.lastTool = null;
          break;
        }
        case "task": {
          const { id, ...task } = payload as unknown as TaskPayload;
          const found = id ? this.callOf(id) : undefined;
          if (found) {
            const tool = cloneTool(found);
            tool.task = { ...tool.task, ...task, summary: task.summary || tool.task?.summary, usage: task.usage || tool.task?.usage };
            tool.endedAt = task.status === "running" ? undefined : event.ts;
          }
          break;
        }
        case "permission": {
          this.root.text = this.root.thinking = null;
          const request = payload as Record<string, unknown>;
          const item: PermissionItem = {
            kind: "permission",
            key: `p${event.seq}`,
            requestId: String(request.requestId || ""),
            toolName: String(request.toolName || ""),
            title: String(request.title || ""),
            description: String(request.description || ""),
            summary: String(request.summary || ""),
            reason: String(request.reason || ""),
            input: request.input && typeof request.input === "object" ? (request.input as Record<string, unknown>) : undefined,
            canRemember: !!request.canRemember,
            status: "pending",
            message: "",
            mode: "",
          };
          this.permissions.set(item.requestId, item);
          pushItem(this.root, item);
          break;
        }
        case "permission_resolved": {
          const resolved = payload as Record<string, unknown>;
          const found = this.permissions.get(String(resolved.requestId || ""));
          if (found) {
            const item = clonePermission(found);
            item.status = (["allow", "deny", "cancelled"].includes(String(resolved.behavior)) ? resolved.behavior : "cancelled") as PermissionItem["status"];
            item.message = String(resolved.message || "");
            item.mode = String(resolved.mode || "");
          }
          break;
        }
        case "error":
          this.root.text = this.root.thinking = null;
          pushItem(this.root, { kind: "error", key: `e${event.seq}`, message: String(payload.message || "error") });
          break;
        case "queued":
        case "status":
          if (event.type === "queued" || payload.state === "queued") {
            this.root.text = null;
            pushItem(this.root, { kind: "queued", key: `q${event.seq}` });
          }
          break;
        case "result": {
          this.root.text = this.root.thinking = this.root.lastTool = null;
          // A tool still marked running when its turn ended never reported back,
          // and a question nobody answered can no longer be answered. Background
          // work outlives a turn that ended normally (one continued in a
          // terminal); a stopped or failed turn ends it.
          const ended = !!(payload.cancelled || payload.isError);
          for (const id of Array.from(this.tools.keys())) {
            const tool = this.tools.get(id);
            if (!tool) continue;
            const failTool = tool.status === "running" && (ended || !this.inRunningAgent(tool));
            const stopTask = ended && tool.task?.status === "running";
            if (failTool || stopTask) {
              const next = cloneTool(tool);
              if (failTool) next.status = "err";
              if (stopTask && next.task) next.task = { ...next.task, status: "stopped" };
            }
          }
          for (const item of Array.from(this.permissions.values())) {
            if (item.status === "pending") {
              clonePermission(item).status = "cancelled";
            }
          }
          pushItem(this.root, { kind: "turn-end", key: `r${event.seq}`, result: payload as unknown as ResultPayload, model: this.model, ts: event.ts });
          break;
        }
      }
    }
    return this.root.items;
  }
}

export function buildThread(events: StoredEvent[]): ThreadItem[] {
  return new ThreadReducer().apply(events);
}

// latestCapabilities is what the newest init event of a conversation offered.
export function latestCapabilities(events: StoredEvent[]): Capabilities | null {
  for (let i = events.length - 1; i >= 0; i--) {
    if (events[i].type !== "init") continue;
    const init = events[i].payload as InitPayload;
    return { slashCommands: init.slashCommands || [], mcpServers: init.mcpServers || [] };
  }
  return null;
}

// A call is working while it runs or while the background work it started does.
export function toolActive(tool: ToolItem): boolean {
  return tool.status === "running" || tool.task?.status === "running";
}

function canonicalAgentName(name?: string, input?: Record<string, unknown>): "Task" | "Agent" | null {
  if (name === "Task" || name === "Agent") return name;
  const variant = typeof input?.variant === "string" ? input.variant : "";
  if (variant === "Task" || variant === "Agent") return variant;
  return null;
}

export const isAgent = (tool: ToolItem) =>
  tool.name === "Agent" ||
  tool.name === "Task" ||
  tool.input?.variant === "Task" ||
  tool.input?.variant === "Agent" ||
  tool.children.length > 0;

// agentsOf lists a conversation's sub-agents in order, each with how deeply
// it is nested in another one.
export function agentsOf(items: ThreadItem[], depth = 0): { tool: ToolItem; depth: number }[] {
  const out: { tool: ToolItem; depth: number }[] = [];
  for (const item of items) {
    if (item.kind !== "tool") continue;
    const agent = isAgent(item);
    if (agent) out.push({ tool: item, depth });
    out.push(...agentsOf(item.children, agent ? depth + 1 : depth));
  }
  return out;
}

// endsWithLineAlignedReply checks in linear time whether streamedReply ends with
// reply (modulo whitespace differences), aligned at the start of a line.
function endsWithLineAlignedReply(streamedReply: string, reply: string): boolean {
  const replyWords = reply.split(/\s+/).filter(Boolean);
  if (replyWords.length === 0 || !streamedReply) return false;
  const tokens: { word: string; start: number }[] = [];
  const wordRe = /\S+/g;
  let match: RegExpExecArray | null;
  while ((match = wordRe.exec(streamedReply)) !== null) {
    tokens.push({ word: match[0], start: match.index });
  }
  if (tokens.length < replyWords.length) return false;
  const offset = tokens.length - replyWords.length;
  for (let i = 0; i < replyWords.length; i++) {
    if (tokens[offset + i].word !== replyWords[i]) return false;
  }
  const firstStart = tokens[offset].start;
  if (firstStart === 0) return true;
  for (let i = firstStart - 1; i >= 0; i--) {
    const ch = streamedReply[i];
    if (ch === "\n") return true;
    if (!/\s/.test(ch)) return false;
  }
  return true;
}

// agentBodyItems returns a sub-agent's own steps and any final reply that
// was not already streamed by its child session.
export function agentBodyItems(tool: ToolItem): ThreadItem[] {
  if (toolActive(tool)) return tool.children;
  // Grok's task_result appends model-facing context after the reply; it is not part of what the sub-agent said.
  const rawOutput = (tool.output || "").split(/\n*<subagent_meta>/)[0].trim();
  const launchNote = /^async agent launched/i.test(rawOutput);
  const reply = (launchNote ? tool.task?.summary || "" : rawOutput || tool.task?.summary || "").trim();
  if (!reply) return tool.children;
  const last = tool.children.at(-1);
  const streamedReply = last?.kind === "text" ? last.text.trim() : "";
  // A final reply can follow commentary in the same block, but not mid-line.
  if (endsWithLineAlignedReply(streamedReply, reply)) return tool.children;
  if (tool.status === "err" || tool.task?.status === "failed") {
    return [...tool.children, { kind: "error", key: `${tool.key}-error`, message: reply }];
  }
  return [...tool.children, { kind: "text", key: `${tool.key}-output`, text: reply, subagent: false }];
}

// agentThread is a sub-agent's own conversation, for the main view: the
// prompt it was given, then what it did.
export function agentThread(tool: ToolItem): ThreadItem[] {
  const prompt = typeof tool.input?.prompt === "string" ? tool.input.prompt : "";
  const head: ThreadItem[] = prompt ? [{ kind: "user", key: `${tool.key}-prompt`, text: prompt, attachments: [], ts: tool.startedAt }] : [];
  return [...head, ...agentBodyItems(tool)];
}

export function agentStepCount(tool: ToolItem): number {
  const count = (items: ThreadItem[]): number => items.reduce((total, item) => total + (item.kind === "tool" ? 1 + count(item.children) : 0), 0);
  return (!toolActive(tool) ? tool.task?.usage?.toolUses : undefined) ?? (count(tool.children) || tool.task?.usage?.toolUses || 0);
}

// The latest step of a sub-agent, for its call's one-line preview.
export function latestStep(tool: ToolItem): string {
  for (let i = tool.children.length - 1; i >= 0; i--) {
    const child = tool.children[i];
    if (child.kind === "tool") return child.description || [child.name, child.summary].filter(Boolean).join(" ");
  }
  return tool.task?.summary || tool.task?.lastTool || "";
}

const str = (value: unknown) => (typeof value === "string" ? value : "");

// The before/after text of an editing call, or null for other tools.
export function toolEdit(tool: ToolItem): { path: string; before: string; after: string }[] | null {
  const input = tool.input;
  if (!input) return null;
  const path = str(input.file_path) || str(input.notebook_path);
  switch (tool.name) {
    case "Edit":
      return [{ path, before: str(input.old_string), after: str(input.new_string) }];
    case "MultiEdit":
      if (Array.isArray(input.diffs)) {
        return input.diffs.map((diff) => {
          const item = diff as Record<string, unknown>;
          return { path: str(item.path) || path, before: str(item.before), after: str(item.after) };
        });
      }
      return Array.isArray(input.edits)
        ? input.edits.map((edit) => ({ path, before: str((edit as Record<string, unknown>).old_string), after: str((edit as Record<string, unknown>).new_string) }))
        : null;
    case "Write":
      return [{ path, before: "", after: str(input.content) }];
    case "NotebookEdit":
      return [{ path, before: "", after: str(input.new_source) }];
  }
  return null;
}

const editStatsCache = new WeakMap<ToolItem, { added: number; deleted: number } | null>();

export function editStats(tool: ToolItem): { added: number; deleted: number } | null {
  if (editStatsCache.has(tool)) return editStatsCache.get(tool) ?? null;
  const edits = toolEdit(tool);
  if (!edits) {
    editStatsCache.set(tool, null);
    return null;
  }
  let added = 0;
  let deleted = 0;
  for (const edit of edits) {
    const stats = diffStats(diffLines(edit.before, edit.after));
    added += stats.added;
    deleted += stats.deleted;
  }
  const result = { added, deleted };
  editStatsCache.set(tool, result);
  return result;
}

export interface Todo {
  content: string;
  status: "pending" | "in_progress" | "completed";
  activeForm?: string;
}

export function toolTodos(tool: ToolItem): Todo[] | null {
  if (tool.name !== "TodoWrite" || !tool.input || !Array.isArray(tool.input.todos)) return null;
  return (tool.input.todos as Record<string, unknown>[]).map((todo) => ({
    content: str(todo.content),
    status: (["pending", "in_progress", "completed"].includes(str(todo.status)) ? str(todo.status) : "pending") as Todo["status"],
    activeForm: str(todo.activeForm) || undefined,
  }));
}

export function toolPlan(tool: ToolItem): string | null {
  if (tool.name !== "ExitPlanMode" || !tool.input) return null;
  return str(tool.input.plan) || null;
}

export function pendingPermissions(items: ThreadItem[]): PermissionItem[] {
  return items.filter((item): item is PermissionItem => item.kind === "permission" && item.status === "pending");
}
