import { CopyCommand } from "../ui/CopyCommand";

export function RecoveryCommand({ command }: { command: string }) {
  return (
    <div className="recovery-action">
      <span>Run:</span>
      <code>{command}</code>
      <CopyCommand
        compact
        command={command}
        failureLabel="Could not copy the command. Select and copy it manually."
        idleLabel="Copy command"
        successLabel="Command copied to the clipboard."
      />
    </div>
  );
}
