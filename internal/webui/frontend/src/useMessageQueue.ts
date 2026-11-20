import { useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import type { Api } from "./api";
import type { Conversation } from "./useConversation";
import { MessageQueue } from "./queue";

export function useMessageQueue(
  api: Api,
  id: string | null,
  conversation: Conversation,
  onSent: () => void,
  onError: (error: unknown) => void,
) {
  const [queue] = useState(() => new MessageQueue());
  const snapshot = useSyncExternalStore(queue.subscribe, queue.snapshot, queue.snapshot);
  const owner = useRef<{ id: string | null; api: Api; staleEvents: Conversation["events"] } | null>(null);
  const lastResult = useMemo(() => conversation.events.reduce((seq, event) => event.type === "result" ? Math.max(seq, event.seq) : seq, 0), [conversation.events]);

  useEffect(() => {
    const switched = owner.current?.id !== id || owner.current?.api !== api;
    if (switched) owner.current = { id, api, staleEvents: conversation.events };
    // useConversation resets in an effect; its first render still belongs to
    // the previous session. Wait for the new history before auto-sending.
    if (owner.current?.staleEvents === conversation.events || !id || conversation.loading || conversation.error) return;
    queue.observe(id, lastResult);
    if (conversation.running) return;
    void queue.flush(id, api.send).then((sent) => { if (sent) onSent(); }).catch(onError);
  }, [api, id, conversation.events, conversation.loading, conversation.error, conversation.running, lastResult, snapshot, queue, onSent, onError]);

  const sendNow = async (key: string) => {
    if (!id) return;
    try {
      if (await queue.send(id, key, api.send)) onSent();
    } catch (error) {
      onError(error);
    }
  };

  return {
    queue,
    ...queue.get(id),
    sendNow,
    remove: (key: string) => { if (id) queue.remove(id, key); },
  };
}
