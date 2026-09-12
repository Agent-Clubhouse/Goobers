import { useState } from "react";
import { Icon } from "./Icon";

interface CopyCommandProps {
  command: string;
  compact?: boolean;
  idleLabel: string;
  successLabel: string;
  failureLabel: string;
}

export function CopyCommand({
  command,
  compact = false,
  failureLabel,
  idleLabel,
  successLabel,
}: CopyCommandProps) {
  const [status, setStatus] = useState<"idle" | "success" | "failure">("idle");

  async function copy() {
    try {
      await navigator.clipboard.writeText(command);
      setStatus("success");
    } catch {
      setStatus("failure");
    }
  }

  const buttonLabel =
    status === "success" ? successLabel : status === "failure" ? `Retry ${idleLabel.toLowerCase()}` : idleLabel;

  return (
    <span className={`copy-command copy-command-${status}${compact ? " copy-command-compact" : ""}`}>
      <button
        aria-label={buttonLabel}
        className={compact ? "copy-command-button copy-command-button-compact" : "secondary-button copy-command-button"}
        onClick={(event) => {
          event.preventDefault();
          event.stopPropagation();
          void copy();
        }}
        type="button"
      >
        <Icon name={status === "success" ? "check" : "copy"} size={16} />
        {status === "success" ? "Copied" : status === "failure" ? "Try again" : compact ? idleLabel : "Copy command"}
      </button>
      {status !== "idle" && (
        <span
          aria-live="polite"
          className={compact ? "sr-only" : "copy-command-status"}
          role="status"
        >
          {status === "success" ? successLabel : failureLabel}
        </span>
      )}
    </span>
  );
}
