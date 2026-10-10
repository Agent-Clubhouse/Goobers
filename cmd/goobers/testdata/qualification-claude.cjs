#!/usr/bin/env node
// Deterministic model substitute for the real parent/child transport journey.
// It launches the supplied Goobers MCP server; it never reads or sends grants.
const fs = require('node:fs');
const path = require('node:path');
const { spawn } = require('node:child_process');
const readline = require('node:readline');
const args = process.argv.slice(2);
if (args.includes('--version')) {
  process.stdout.write('2.1.0 (qualification fixture)\n');
  process.exit(0);
}
if (args[0] === 'auth' && args[1] === 'status') {
  process.stdout.write('{"loggedIn":true,"authMethod":"api_key"}\n');
  process.exit(0);
}
const prompt = args.at(-1);
const index = args.indexOf('--mcp-config');
if (index < 0) throw new Error('qualification requires real Goobers MCP registration');
const registered = JSON.parse(args[index + 1]).mcpServers['goobers-io'];
if (!registered) throw new Error('Goobers MCP server missing');
const mcp = spawn(registered.command, registered.args, { stdio: ['pipe', 'pipe', 'pipe'] });
// Deliberately do not echo MCP configuration, arguments, stderr or response data.
const pending = new Map();
let nextID = 0;
readline.createInterface({ input: mcp.stdout }).on('line', line => {
  const response = JSON.parse(line);
  const resolve = pending.get(response.id);
  if (resolve) {
    pending.delete(response.id);
    resolve(response);
  }
});
mcp.stderr.resume();
function request(method, params) {
  const id = ++nextID;
  return new Promise(resolve => {
    pending.set(id, resolve);
    mcp.stdin.write(JSON.stringify({ jsonrpc: '2.0', id, method, params }) + '\n');
  });
}
async function tool(name, arguments_) {
  const response = await request('tools/call', { name, arguments: arguments_ });
  if (response.error || response.result?.isError) return { error: true };
  return { value: JSON.parse(response.result.content.find(item => item.type === 'text').text) };
}
function waitForHost() {
  // The ordinary parent driver must own the durable yield and interrupt us.
  setInterval(() => {}, 1000);
}
async function main() {
  const init = await request('initialize', { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'qualification-model', version: '1' } });
  if (init.error) throw new Error('MCP initialization failed');
  mcp.stdin.write(JSON.stringify({ jsonrpc: '2.0', method: 'notifications/initialized' }) + '\n');
  const invocationKey = 'qualification-child';
  let status = await tool('get_child_workflow', { invocationKey });
  if (status.error) {
    const sourceFile = 'generated-child.yaml';
    fs.writeFileSync(sourceFile, 'apiVersion: goobers.dev/v1alpha1\nkind: Workflow\ndslVersion: "3.1"\nmetadata: {name: generated-check}\nspec:\n  gaggle: example\n  triggers: [{type: manual}]\n  start: check\n  tasks:\n    - name: check\n      type: deterministic\n      goal: Verify the generated workstream\n      timeoutSeconds: 60\n      runsOn: {os: linux, capabilities: [isolated-child]}\n      run: {workspace: scratch, command: [sh, -c, "echo real-generated-child"]}\n');
    const validation = await tool('validate_child_workflow', { sourceFile });
    if (validation.error || !validation.value.valid) throw new Error('generated source rejected');
    const accepted = await tool('start_child_workflow', { sourceFile, invocationKey });
    if (accepted.error) throw new Error('child acceptance failed');
    return waitForHost();
  }
  if (!status.value.resultRef) return waitForHost();
  if (!status.value.acknowledged) {
    const disposition = await tool('resolve_child_workflow', { invocationKey, action: 'discard', resultRef: status.value.resultRef });
    if (disposition.error) throw new Error('child disposition failed');
    return waitForHost();
  }
  const match = prompt.match(/write your [^\n]+ as JSON to `([^`]+)`/);
  if (!match || path.isAbsolute(match[1]) || match[1].split('/').includes('..')) throw new Error('completion contract unavailable');
  fs.mkdirSync(path.dirname(match[1]), { recursive: true });
  fs.writeFileSync(match[1], JSON.stringify({ status: 'success', outputs: { summary: 'Generated child completed and disposition acknowledged' } }));
  process.stdout.write(JSON.stringify({ type: 'result', subtype: 'success', is_error: false, result: 'Child completed', usage: { input_tokens: 1, output_tokens: 1 } }) + '\n');
  mcp.stdin.end();
}
main().catch(() => {
  process.stderr.write('qualification model protocol failed\n');
  mcp.kill();
  process.exitCode = 1;
});
