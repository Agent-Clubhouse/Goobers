#!/usr/bin/env node
// Model substitution only. The real stage harness owns MCP, authority,
// stopping/yielding, continuation materialization and completion validation.
const fs = require('node:fs');
const path = require('node:path');
const {spawn} = require('node:child_process');
const readline = require('node:readline');
const args = process.argv.slice(2);
if (args.includes('--version')) {
  console.log('2.1.0 (kind child fixture)'); process.exit(0);
}
if (args[0] === 'auth' && args[1] === 'status') {
  console.log('{"loggedIn":true,"authMethod":"api_key"}'); process.exit(0);
}
const prompt = fs.readFileSync(0, 'utf8');
if (!/KIND_CHILD_(HAPPY|FAILOVER)/.test(prompt)) throw Error('unrecognized fixture invocation');
const contextDir = '.goobers/context';
const receipts = fs.existsSync(contextDir) ? fs.readdirSync(contextDir).filter(n => n.endsWith('-child-result')) : [];
if (receipts.length) {
  if (receipts.length !== 1) throw Error('ambiguous child continuation');
  const receipt = JSON.parse(fs.readFileSync(path.join(contextDir, receipts[0]), 'utf8'));
  if (!receipt.runId || receipt.input?.state !== 'completed') throw Error('child did not complete');
  const match = prompt.match(/write your [^\n]+ as JSON to `([^`]+)`/);
  if (!match || path.isAbsolute(match[1]) || match[1].split('/').includes('..')) throw Error('completion contract missing');
  fs.mkdirSync(path.dirname(match[1]), {recursive: true});
  fs.writeFileSync(match[1], JSON.stringify({status: 'success', summary: 'kind parent consumed child receipt'}));
  console.log(JSON.stringify({type: 'result', subtype: 'success', is_error: false, result: 'receipt consumed', usage: {input_tokens: 1, output_tokens: 1}}));
  process.exit(0);
}
const index = args.indexOf('--mcp-config');
if (index < 0) throw Error('MCP registration missing');
const registration = JSON.parse(args[index + 1]).mcpServers['goobers-io'];
const mcp = spawn(registration.command, registration.args, {stdio: ['pipe', 'pipe', 'pipe']});
mcp.stderr.resume(); // Never echo grants, MCP configuration or remote bodies.
const pending = new Map();
let nextID = 0;
readline.createInterface({input: mcp.stdout}).on('line', line => {
  const response = JSON.parse(line);
  const resolve = pending.get(response.id);
  if (resolve) { pending.delete(response.id); resolve(response); }
});
function request(method, params) {
  const id = ++nextID;
  return new Promise(resolve => {
    pending.set(id, resolve);
    mcp.stdin.write(JSON.stringify({jsonrpc: '2.0', id, method, params}) + '\n');
  });
}
async function tool(name, args) {
  const response = await request('tools/call', {name, arguments: args});
  if (response.error || response.result?.isError) throw Error('child workflow tool refused');
  return JSON.parse(response.result.content.find(c => c.type === 'text').text);
}
async function main() {
  const initialized = await request('initialize', {protocolVersion: '2025-06-18', capabilities: {}, clientInfo: {name: 'kind-child-model', version: '1'}});
  if (initialized.error) throw Error('MCP initialization refused');
  mcp.stdin.write(JSON.stringify({jsonrpc: '2.0', method: 'notifications/initialized'}) + '\n');
  // The driver observes this side effect and the custody heartbeat before
  // deleting the owning worker. It releases the SAME pod only after adoption.
  fs.copyFileSync('/usr/local/share/kind-child/generated-child.yaml', 'generated-child.yaml');
  const validated = await tool('validate_child_workflow', {sourceFile: 'generated-child.yaml'});
  if (!validated.valid) throw Error('generated child rejected');
  const child = await tool('start_child_workflow', {sourceFile: 'generated-child.yaml', invocationKey: 'kind-child'});
  if (!child.runId || !child.acceptanceId) throw Error('child acceptance lacks identity');
  // The production parent handoff adapter must interrupt this stage. A model
  // result is never allowed to forge DispatchStageResult.ChildWait.
  setInterval(() => {}, 1000);
}
main().catch(() => { console.error('kind child model fixture failed'); process.exit(1); });
