import { useEffect, useRef, useState } from "react";
import { DaemonApiError, DaemonAuthError } from "../api/errors";
import type { SessionAcceptance, InteractiveCapabilities, InteractiveSession } from "../api/types";

export function sessionAction(capabilities: InteractiveCapabilities | undefined, action: "session.create" | "session.message") {
  return capabilities?.actions.some((entry) => entry.action === action && entry.available) ?? false;
}
export function sessionState(session: InteractiveSession) {
  return { idle: "Ready", queued: "Queued", running: "Agent working", "cancel-requested": "Closing; stop pending", closed: "Closed" }[session.state];
}
// Preserve the exact content and key when acceptance is uncertain.
export function useSessionCommand<T>(operation: (key: string, input: T) => Promise<SessionAcceptance>, accepted: (value: SessionAcceptance) => void) {
  const request = useRef<{ key: string; input: T } | undefined>(undefined);
  const running = useRef(false);
  const mounted = useRef(true);
  const [busy, setBusy] = useState(false);
  const [uncertain, setUncertain] = useState(false);
  const [notice, setNotice] = useState("");
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  async function submit(input: T) {
    if (running.current) return;
    running.current = true; setBusy(true); setNotice("");
    request.current ??= { key: crypto.randomUUID(), input };
    try {
      const result = await operation(request.current.key, request.current.input);
      request.current = undefined;
      if (mounted.current) { setUncertain(false); accepted(result); }
    } catch (error) {
      if (!mounted.current) return;
      const refused = error instanceof DaemonAuthError || (error instanceof DaemonApiError && error.status >= 400 && error.status < 500 && error.status !== 408 && error.status !== 429);
      if (refused) {
        request.current = undefined; setUncertain(false);
        setNotice("The command was refused. Refresh to review the session and your access.");
      } else {
        setUncertain(true);
        setNotice("Acceptance is not confirmed. Retry the same command to recover its result.");
      }
    } finally { running.current = false; if (mounted.current) setBusy(false); }
  }
  return { busy, uncertain, notice, submit };
}
