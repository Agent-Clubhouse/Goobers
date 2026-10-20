import type { InteractiveEvent, StreamState, StreamSubscription } from "./client";
import { humanControlHeaders } from "./humanAuth";

function reconnectDelay(signal: AbortSignal): Promise<void> {
  return new Promise(resolve => {
    const finish = () => { clearTimeout(timer); signal.removeEventListener("abort", finish); resolve(); };
    const timer = setTimeout(finish, 1000);
    signal.addEventListener("abort", finish, { once: true });
    if (signal.aborted) finish();
  });
}

// EventSource cannot attach the origin-bound credential. Fetch keeps secrets in
// headers, never in stream URLs, while retaining cursor-based reconnection.
export function subscribeHumanStream(
  url: string, after: number, fetcher: typeof fetch,
  onEvent: (event: InteractiveEvent) => void, onState: (state: StreamState) => void,
): StreamSubscription {
  const controller = new AbortController();
  let cursor = after;
  const consume = async () => {
    const response = await fetcher(`${url}?after=${cursor}`, {
      credentials: "same-origin", signal: controller.signal,
      headers: { Accept: "text/event-stream", ...humanControlHeaders() },
      redirect: "error",
    });
    if (!response.ok || !response.body || !response.headers.get("content-type")?.includes("text/event-stream")) {
      throw new Error("Authenticated stream unavailable");
    }
    onState("live");
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    try {
      while (!controller.signal.aborted) {
        const { value, done } = await reader.read();
        if (done) return;
        buffer = (buffer + decoder.decode(value, { stream: true })).replace(/\r\n/g, "\n");
        if (buffer.length > 2 * 1024 * 1024) throw new Error("Stream frame exceeds limit");
        let end: number;
        while ((end = buffer.indexOf("\n\n")) >= 0) {
          const frame = buffer.slice(0, end);
          buffer = buffer.slice(end + 2);
          const lines = frame.split("\n");
          if (!lines.some(line => line === "event: interactive")) continue;
          const event = JSON.parse(lines.filter(line => line.startsWith("data:")).map(line => line.slice(5).trimStart()).join("\n")) as InteractiveEvent;
          if (!Number.isSafeInteger(event.sequence) || event.sequence <= cursor) continue;
          onEvent(event);
          cursor = event.sequence;
        }
      }
    } finally { await reader.cancel().catch(() => {}); reader.releaseLock(); }
  };
  void (async () => {
    while (!controller.signal.aborted) {
      try { await consume(); } catch { /* Reconnect from the last delivered event. */ }
      if (controller.signal.aborted) break;
      onState("reconnecting");
      await reconnectDelay(controller.signal);
    }
  })();
  return { close: () => controller.abort() };
}
