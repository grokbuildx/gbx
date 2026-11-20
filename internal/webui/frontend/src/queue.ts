export interface Queued {
  key: string;
  text: string;
  attachments: string[];
  failed?: boolean;
}

interface QueueState {
  items: Queued[];
  sending?: string;
  // An ordinary send waits for its own turn to finish, not the previous one.
  waiting?: { after: number; seq?: number };
}

type Send = (id: string, text: string, attachments: string[], steer: boolean) => Promise<{ running: boolean; steered?: boolean; seq?: number }>;
const EMPTY: QueueState = { items: [] };

// Ownership is per session and claimed synchronously, before any request.
export class MessageQueue {
  private queues: Record<string, QueueState> = {};
  private results = new Map<string, number>();
  private listeners = new Set<() => void>();

  snapshot = () => this.queues;
  subscribe = (listener: () => void) => {
    this.listeners.add(listener);
    return () => { this.listeners.delete(listener); };
  };

  private update(id: string, state: QueueState) {
    this.queues = { ...this.queues, [id]: state };
    for (const listener of this.listeners) listener();
  }

  get(id: string | null): QueueState {
    return (id && this.queues[id]) || EMPTY;
  }

  add(id: string, item: Queued) {
    const state = this.get(id);
    this.update(id, { ...state, items: [...state.items, item] });
  }

  remove(id: string, key: string) {
    const state = this.get(id);
    if (state.sending === key) return;
    this.update(id, { ...state, items: state.items.filter((item) => item.key !== key) });
  }

  observe(id: string, lastResult: number) {
    this.results.set(id, Math.max(lastResult, this.results.get(id) || 0));
    const state = this.get(id);
    if (state.waiting && this.finished(id, state.waiting)) this.update(id, { ...state, waiting: undefined });
  }

  private finished(id: string, waiting: NonNullable<QueueState["waiting"]>) {
    const result = this.results.get(id) || 0;
    return waiting.seq === undefined ? result > waiting.after : result >= waiting.seq;
  }

  async send(id: string, key: string, request: Send, steer = true): Promise<boolean> {
    const state = this.get(id);
    const item = state.items.find((entry) => entry.key === key);
    if (!item || state.sending) return false;
    const after = this.results.get(id) || 0;
    this.update(id, { ...state, sending: key, items: state.items.map((entry) => entry.key === key ? { ...entry, failed: false } : entry) });
    try {
      const response = await request(id, item.text, item.attachments, steer);
      const current = this.get(id);
      const waiting = response.running && !response.steered ? { after, seq: response.seq } : current.waiting;
      this.update(id, {
        ...current,
        items: current.items.filter((entry) => entry.key !== key),
        sending: undefined,
        waiting: waiting && !this.finished(id, waiting) ? waiting : undefined,
      });
      return true;
    } catch (error) {
      const current = this.get(id);
      this.update(id, { ...current, sending: undefined, items: current.items.map((entry) => entry.key === key ? { ...entry, failed: true } : entry) });
      throw error;
    }
  }

  async flush(id: string, request: Send): Promise<boolean> {
    const state = this.get(id);
    const first = state.items[0];
    // A failure needs an explicit retry or removal; renders never retry it.
    if (!first || first.failed || state.sending || state.waiting) return false;
    return this.send(id, first.key, request, false);
  }
}
