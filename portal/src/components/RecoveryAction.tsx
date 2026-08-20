export function RecoveryCommand({ command }: { command: string }) {
  return (
    <div className="recovery-action">
      <span>Run:</span>
      <code>{command}</code>
    </div>
  );
}
