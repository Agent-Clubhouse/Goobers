function quotePowerShell(value: string): string {
  return `'${value.replaceAll("'", "''")}'`;
}

export function manualRunCommand(
  gaggle: string,
  workflow: string,
  instanceRoot = ".",
): string {
  return `goobers run ${gaggle}/${workflow} ${quotePowerShell(instanceRoot)}`;
}

export function statusCommand(instanceRoot = "."): string {
  return `goobers status ${quotePowerShell(instanceRoot)}`;
}
