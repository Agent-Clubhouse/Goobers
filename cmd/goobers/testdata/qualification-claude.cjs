#!/usr/bin/env node
// Deterministic model substitute for the real parent/child transport journey.
// It launches the supplied Goobers MCP server; it never reads or sends grants.
const fs = require('node:fs');
const path = require('node:path');
const { spawn } = require('node:child_process');
const readline = require('node:readline');
const args = process.argv.slice(2);
const codex = path.basename(process.argv[1]) === 'codex';
if (args.includes('--version')) {
  process.stdout.write('2.1.0 (qualification fixture)\n');
  process.exit(0);
}
if (args[0] === 'auth' && args[1] === 'status') {
  process.stdout.write('{"loggedIn":true,"authMethod":"api_key"}\n');
  process.exit(0);
}
const prompt = fs.readFileSync(0, 'utf8');
const agenticChild = prompt.includes('QUALIFICATION_AGENTIC_CHILD_TASK');
const registered = codex ? codexRegistration() : claudeRegistration();
if (!registered) throw new Error('Goobers MCP server missing');
const mcp = spawn(registered.command, registered.args, { stdio: ['pipe', 'pipe', 'pipe'], ...(codex ? { env: codexMCPEnvironment(registered) } : {}) });
if (codex) process.stdout.write(JSON.stringify({ type: 'thread.started', thread_id: require('node:crypto').randomUUID() }) + '\n');
// Deliberately do not echo MCP configuration, arguments, stderr or response data.
const pending = new Map();
let nextID = 0;
let diagnosticPhase = 'initialize';
function phase(value) {
  diagnosticPhase = value;
  process.stderr.write(`qualification phase: ${value}\n`);
}
function claudeRegistration() {
  const index = args.indexOf('--mcp-config');
  if (index < 0) throw new Error('qualification requires real Goobers MCP registration');
  return JSON.parse(args[index + 1]).mcpServers['goobers-io'];
}
function codexRegistration() {
  if (args[0] !== 'exec' || !args.includes('--json') || process.env.CODEX_API_KEY !== 'sk-qualification-model-only' || process.env.ANTHROPIC_API_KEY || process.env.CLAUDE_CODE_OAUTH_TOKEN) throw new Error('Codex invocation or model credential differs');
  const config = fs.readFileSync(path.join(process.env.CODEX_HOME, 'config.toml'), 'utf8');
  const server = {};
  let selected = false;
  for (const line of config.split('\n')) {
    if (line.startsWith('[')) { selected = line === '[mcp_servers."goobers-io"]'; continue; }
    if (!selected || !line.includes(' = ')) continue;
    const index = line.indexOf(' = ');
    try { server[line.slice(0, index)] = JSON.parse(line.slice(index + 3)); }
    catch { throw new Error('invalid private Codex MCP configuration'); }
  }
  if (typeof server.command !== 'string' || !Array.isArray(server.args) || server.enabled !== true || server.required !== true) throw new Error('required Codex MCP registration missing');
  for (const tool of ['validate_child_workflow','start_child_workflow','get_child_workflow','resolve_child_workflow']) {
    if (agenticChild ? server.enabled_tools?.includes(tool) : !server.enabled_tools?.includes(tool)) throw new Error('Codex child tool authority differs');
  }
  return server;
}
function codexMCPEnvironment(server) {
  // Honor the adapter's explicit MCP environment. In particular, the model
  // credential is not an implicit subprocess environment grant.
  const env = {};
  for (const name of ['PATH','HOME','USER','TMPDIR', ...(server.env_vars || [])]) {
    if (process.env[name] !== undefined) env[name] = process.env[name];
  }
  return env;
}
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
async function tool(name, arguments_, retries = 0) {
  phase(`tool-${name}`);
  const response = await request('tools/call', { name, arguments: arguments_ });
  if (response.error || response.result?.isError) {
    // Print only known local classifications, never remote response text.
    const safeCodes = ['class_saturated','child_workflow_not_found','child_workflow_authority_changed','child_workflow_grant_invalid','child_workflow_wrong_parent','child_workflow_custody_unavailable','child_workflow_unavailable'];
    const encoded = JSON.stringify(response);
    const code = safeCodes.find(value => encoded.includes(value)) || 'unclassified';
    phase(`tool-${name}-refused-${code}`);
    // The real API sheds excess concurrent mutations with Retry-After: 1.
    // Repeat the exact source/key/result; never retry an authority refusal.
    if (code === 'class_saturated' && retries < 4) {
      await new Promise(resolve => setTimeout(resolve, 1000));
      return tool(name, arguments_, retries + 1);
    }
    return { error: true, code };
  }
  return { value: JSON.parse(response.result.content.find(item => item.type === 'text').text) };
}
function waitForHost() {
  // The ordinary parent driver must own the durable yield and interrupt us.
  setInterval(() => {}, 1000);
}
function childAuthoringCatalog() {
  const directory = path.join(path.basename(process.argv[1]) === 'codex' ? '.agents' : '.claude', 'skills', 'goobers-child-workflows');
  const skill = fs.readFileSync(path.join(directory, 'SKILL.md'), 'utf8');
  const catalog = JSON.parse(fs.readFileSync(path.join(directory, 'catalog.json'), 'utf8'));
  if (!prompt.includes(directory + '/SKILL.md') || !skill.includes('validate_child_workflow') || !skill.includes('resolve_child_workflow')) throw new Error('parent authoring guidance did not reach the model');
  if (catalog.schemaVersion !== 'goobers-child-authoring/v1' || !catalog.newSubmissionsAvailable || catalog.dslVersion !== '3.1' || !catalog.goobers.some(g => g.name === 'coder') || catalog.maxStates !== 128) throw new Error('parent authoring catalog differs');
  return catalog;
}
async function main() {
  if (!codex && (process.env.ANTHROPIC_API_KEY !== 'sk-ant-qualification-model-only' || process.env.CLAUDE_CODE_OAUTH_TOKEN || process.env.CODEX_API_KEY)) throw new Error('Claude model credential differs');
  const init = await request('initialize', { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'qualification-model', version: '1' } });
  if (init.error) throw new Error('MCP initialization failed');
  mcp.stdin.write(JSON.stringify({ jsonrpc: '2.0', method: 'notifications/initialized' }) + '\n');
  if (agenticChild) return agenticChildJourney();
  let invocationKey = 'qualification-child';
  const mode = fs.readFileSync('qualification-mode', 'utf8').trim();
  if (['parallel','parallel-cancel','parallel-daemon-restart'].includes(mode)) return parallelJourney();
  if (mode.startsWith('generated-parallel')) return generatedParallelJourney(mode);
  const publication = ['publication','publication-lost-reply'].includes(mode);
  if (!publication && !['scratch','merge','replace','discard','cancel','iterate','worker-restart','daemon-restart','agentic'].includes(mode)) throw new Error('unknown qualification mode');
  if (publication && Object.values(process.env).some(value => value.includes('host-only-publication-'))) throw new Error('publication credential reached parent pod');
  const authoring = childAuthoringCatalog();
  const scratch = ['scratch','iterate','worker-restart','daemon-restart'].includes(mode);
  const action = mode === 'agentic' ? 'merge' : scratch || publication ? 'discard' : mode;
  let status = await tool('get_child_workflow', { invocationKey });
  if (['iterate','worker-restart'].includes(mode) && !status.error && status.value.acknowledged) {
    invocationKey = 'qualification-child-2';
    status = await tool('get_child_workflow', { invocationKey });
  }
  if (status.error) {
    if (publication && status.code !== 'child_workflow_not_found') throw new Error('publication status refused');
    const sourceFile = 'generated-child.yaml';
    fs.writeFileSync('parent-before-child.txt', 'parent before child\n');
    let command = scratch ? 'echo real-generated-child' : 'test "$(cat parent-before-child.txt)" = "parent before child" && printf "child return\\n" > child-return.txt && git add child-return.txt && git -c user.name=Qualification -c user.email=qualification@example.invalid commit -m "Child work"';
    if (publication) command = 'test -z "$QUALIFICATION_PUBLICATION_PUSH_TOKEN" && test -z "$QUALIFICATION_PUBLICATION_PR_TOKEN" && test "$(cat parent-before-child.txt)" = "parent before child" && printf "published child return\\n" > child-return.txt';
    if (['cancel','worker-restart','daemon-restart'].includes(mode)) {
      const notify = fs.readFileSync('qualification-notify', 'utf8').trim();
      if (!/^http:\/\/host\.docker\.internal:[0-9]+\/started$/.test(notify)) throw new Error('invalid qualification signal');
      command = `node -e 'require("http").get(${JSON.stringify(notify)}, r => r.resume())'; sleep ${mode === 'cancel' ? 60 : invocationKey === 'qualification-child' ? 45 : 10}`;
    }
    const runner = authoring.runners.find(value => value.os === 'linux' && value.shell);
    if (!runner) throw new Error('catalog has no declared shell runner');
    const runsOn = { os: runner.os, capabilities: runner.capabilities };
    const run = { workspace: (scratch || mode === 'cancel') ? 'scratch' : 'repo', command: ['sh','-c',command] };
    const tasks = [{ name: 'check', type: 'deterministic', goal: 'Verify the generated workstream', timeoutSeconds: 60, runsOn, run }];
    if (mode === 'agentic') {
      const goober = authoring.goobers.find(value => value.harness === (codex ? 'codex' : 'claude-code') && value.capabilities.includes('agent:model'));
      if (!goober || !runner.harnesses.includes(goober.harness)) throw new Error('catalog has no compatible existing Goober');
      tasks[0] = { name: 'check', type: 'agentic', goober: goober.name, goal: 'QUALIFICATION_AGENTIC_CHILD_TASK: verify the captured parent and produce the requested child result', workspace: 'repo', capabilities: goober.capabilities, policyActions: goober.policyActions, timeoutSeconds: 60, runsOn };
    }
    if (publication) {
      if (!authoring.allowPRPublication) throw new Error('catalog did not permit delegated publication');
      tasks[0].next = 'push';
      tasks.push({ name: 'push', type: 'deterministic', goal: 'Publish the delegated child branch', runsOn, capabilities: ['repo:push'], policyActions: ['push-repository-branch'], run: { command: ['goobers','push-branch'], workspace: 'repo' }, next: 'open' });
      tasks.push({ name: 'open', type: 'deterministic', goal: 'Open the delegated child PR', runsOn, capabilities: ['provider:pr:write'], policyActions: ['open-or-update-pr'], run: { command: ['goobers','open-pr'], workspace: 'repo' } });
    }
    fs.writeFileSync(sourceFile, JSON.stringify({ apiVersion: 'goobers.dev/v1alpha1', kind: 'Workflow', dslVersion: authoring.dslVersion, metadata: { name: 'generated-check' }, spec: { gaggle: authoring.gaggle, triggers: [{ type: 'manual' }], start: 'check', tasks } }));
    const validation = await tool('validate_child_workflow', { sourceFile });
    if (validation.error || !validation.value.valid) throw new Error('generated source rejected');
    const accepted = await tool('start_child_workflow', { sourceFile, invocationKey });
    if (accepted.error) throw new Error('child acceptance failed');
    return waitForHost();
  }
  if (!status.value.resultRef) return waitForHost();
  if (mode === 'agentic' && status.value.state !== 'completed') throw new Error('agentic child did not complete');
  if (publication && status.value.state !== (mode === 'publication-lost-reply' ? 'failed' : 'completed')) throw new Error('publication child outcome changed');
  if (mode === 'worker-restart') {
    const expectedState = invocationKey === 'qualification-child' ? 'failed' : 'completed';
    if (status.value.state !== expectedState) throw new Error('worker restart child outcome changed');
  }
  if (!status.value.acknowledged) {
    fs.writeFileSync('parent-after-child.txt', 'parent after child\n');
    const disposition = await tool('resolve_child_workflow', { invocationKey, action, resultRef: status.value.resultRef });
    if (disposition.error) throw new Error('child disposition failed');
    return waitForHost();
  }
  if (!scratch) {
    const hasChild = fs.existsSync('child-return.txt');
    if (hasChild !== (action !== 'discard')) throw new Error('child return disposition mismatch');
    if (hasChild && fs.readFileSync('child-return.txt','utf8') !== 'child return\n') throw new Error('child return bytes mismatch');
    if (mode === 'agentic' && JSON.parse(fs.readFileSync('agentic-child-evidence.json','utf8')).harness !== (codex ? 'codex' : 'claude-code')) throw new Error('agentic child evidence did not return');
    if (fs.existsSync('parent-after-child.txt') !== (action !== 'replace')) throw new Error('parent state disposition mismatch');
    if (fs.readFileSync('parent-before-child.txt','utf8') !== 'parent before child\n') throw new Error('parent fork content lost');
  }
  return complete();
}
async function agenticChildJourney() {
  phase('agentic-child-authority');
  if (!codex && process.env.ANTHROPIC_API_KEY !== 'sk-ant-qualification-model-only') throw new Error('child model credential differs');
  const listed = await request('tools/list', {});
  if (listed.error || !Array.isArray(listed.result?.tools)) throw new Error('child tool catalog missing');
  const names = listed.result.tools.map(value => value.name);
  for (const name of ['validate_child_workflow','start_child_workflow','get_child_workflow','resolve_child_workflow']) {
    if (names.includes(name)) throw new Error('generated agentic child acquired recursive workflow authority');
  }
  if (prompt.includes('Child workflows are enabled for this stage.')) throw new Error('generated agentic child received parent instructions');
  if (fs.readFileSync('parent-before-child.txt','utf8') !== 'parent before child\n') throw new Error('child did not receive the captured parent tree');
  fs.writeFileSync('child-return.txt', 'child return\n');
  fs.writeFileSync('agentic-child-evidence.json', JSON.stringify({ harness: codex ? 'codex' : 'claude-code', tools: names.sort() }));
  return complete();
}
function complete() {
  phase('completion-contract');
  const match = prompt.match(/write your [^\n]+ as JSON to `([^`]+)`/);
  if (!match || path.isAbsolute(match[1]) || match[1].split('/').includes('..')) throw new Error('completion contract unavailable');
  fs.mkdirSync(path.dirname(match[1]), { recursive: true });
  fs.writeFileSync(match[1], JSON.stringify({ status: 'success', outputs: { summary: 'Generated child completed and disposition acknowledged' } }));
  const result = codex ? { type: 'turn.completed', usage: { input_tokens: 1, output_tokens: 1 } } : { type: 'result', subtype: 'success', is_error: false, result: 'Child completed', usage: { input_tokens: 1, output_tokens: 1 } };
  process.stdout.write(JSON.stringify(result) + '\n');
  mcp.stdin.end();
}
async function parallelJourney() {
  const branch = prompt.match(/QUALIFICATION_BRANCH=(left|right|join)/)?.[1];
  if (!branch) throw new Error('parallel branch marker missing');
  phase(`parallel-${branch}-workspace`);
  if (branch === 'join') {
    for (const name of ['left','right']) {
      if (fs.readFileSync(`parent-${name}.txt`, 'utf8') !== `${name} before child\n` ||
          fs.readFileSync(`parent-after-${name}.txt`, 'utf8') !== `${name} after child\n`) throw new Error('parallel return lost branch work');
    }
    if (fs.readFileSync('source.txt','utf8') !== 'parent source\n') throw new Error('parallel join changed base');
    return complete();
  }
  const other = branch === 'left' ? 'right' : 'left';
  if (fs.existsSync(`parent-${other}.txt`)) throw new Error('branch observed sibling workspace');
  const invocationKey = 'qualification-child';
  const status = await tool('get_child_workflow', { invocationKey });
  if (status.error && status.code !== 'child_workflow_not_found') throw new Error('parallel status refused');
  if (status.error) {
    fs.writeFileSync(`parent-${branch}.txt`, `${branch} before child\n`);
    const notify = fs.readFileSync('qualification-notify', 'utf8').trim();
    if (!/^http:\/\/host\.docker\.internal:[0-9]+\/started$/.test(notify)) throw new Error('invalid parallel barrier');
    const mode = fs.readFileSync('qualification-mode','utf8').trim();
    const delay = mode === 'parallel-cancel' ? ' && sleep 60' : mode === 'parallel-daemon-restart' ? ' && sleep 45' : '';
    const command = `node -e 'require("http").get(${JSON.stringify(notify+'?branch='+branch)}, r => r.resume())'${delay}`;
    const sourceFile = `generated-${branch}.yaml`;
    const run = JSON.stringify({ workspace:'scratch', command:['sh','-c',command] });
    fs.writeFileSync(sourceFile, 'apiVersion: goobers.dev/v1alpha1\nkind: Workflow\ndslVersion: "3.1"\nmetadata: {name: generated-check}\nspec:\n  gaggle: example\n  triggers: [{type: manual}]\n  start: check\n  tasks:\n    - name: check\n      type: deterministic\n      goal: Wait for the sibling child\n      timeoutSeconds: 120\n      runsOn: {os: linux, capabilities: [isolated-child]}\n      run: '+run+'\n');
    const validation = await tool('validate_child_workflow', { sourceFile });
    if (validation.error || !validation.value.valid) throw new Error('parallel generated source rejected');
    const accepted = await tool('start_child_workflow', { sourceFile, invocationKey });
    if (accepted.error) throw new Error('parallel child acceptance failed');
    return waitForHost();
  }
  phase(`parallel-status-result-${Boolean(status.value.resultRef)}-ack-${Boolean(status.value.acknowledged)}`);
  if (!status.value.resultRef) return waitForHost();
  if (status.value.state !== 'completed') throw new Error('parallel child did not complete');
  if (!status.value.acknowledged) {
    fs.writeFileSync(`parent-after-${branch}.txt`, `${branch} after child\n`);
    const disposition = await tool('resolve_child_workflow', { invocationKey, action:'discard', resultRef:status.value.resultRef });
    if (disposition.error) throw new Error('parallel child disposition failed');
    return waitForHost();
  }
  return complete();
}
main().catch(error => {
  const known = ['Claude model credential differs', 'agentic child did not complete', 'child return bytes mismatch', 'agentic child evidence did not return', 'generated agentic child acquired recursive workflow authority', 'generated agentic child received parent instructions', 'child model credential differs', 'child return disposition mismatch', 'parent state disposition mismatch', 'parent fork content lost', 'parent authoring catalog differs', 'parent authoring guidance did not reach the model', 'completion contract unavailable'];
  const code = known.includes(error.message) ? error.message.toLowerCase().replaceAll(' ', '-') : error.code === 'ENOENT' ? 'file-not-found' : 'unclassified';
  process.stderr.write(`qualification failure code: ${code}\n`);
  process.stderr.write(`qualification model protocol failed at ${diagnosticPhase}\n`);
  mcp.kill();
  process.exitCode = 1;
});

async function generatedParallelJourney(mode) {
  const delay = mode === 'generated-parallel-cancel' ? 60000 : mode === 'generated-parallel-daemon-restart' ? 45000 : 0;
  const invocationKey = 'qualification-child';
  const status = await tool('get_child_workflow', { invocationKey });
  if (status.error) {
    if (status.code !== 'child_workflow_not_found') throw new Error('parallel generated child lookup refused');
    fs.writeFileSync('parent-before-child.txt', 'parent before child\n');
    const notify = fs.readFileSync('qualification-notify', 'utf8').trim();
    if (!/^http:\/\/host\.docker\.internal:[0-9]+\/started$/.test(notify)) throw new Error('invalid parallel child signal');
    const tasks = ['left', 'right'].map(branch => {
      const observe = `if(require("fs").readFileSync("parent-before-child.txt","utf8")!=="parent before child\\n")throw Error("fork source changed");require("http").get(${JSON.stringify(notify + '?branch=' + branch)},r=>{r.resume();r.on("end",()=>{if(r.statusCode!==204)process.exitCode=1;else if(${delay})setTimeout(()=>{},${delay})})}).on("error",()=>{process.exitCode=1})`;
      return { name: 'inspect-' + branch, type: 'deterministic', goal: 'Inspect the isolated child view', timeoutSeconds: 120, runsOn: { os: 'linux', capabilities: ['isolated-child'] }, run: { workspace: 'repo-readonly', command: ['node','-e',observe] }, next: '@join' };
    });
    tasks.push({ name: 'collate', type: 'deterministic', goal: 'Return the joined child result', runsOn: { os: 'linux', capabilities: ['isolated-child'] }, run: { workspace: 'repo', command: ['sh','-c','printf "child return\\n" > child-return.txt && git add child-return.txt && git -c user.name=Qualification -c user.email=qualification@example.invalid commit -m "Joined child work"'] } });
    const document = { apiVersion: 'goobers.dev/v1alpha1', kind: 'Workflow', dslVersion: '3.1', metadata: { name: 'generated-check' }, spec: { gaggle: 'example', triggers: [{ type: 'manual' }], start: 'fan', tasks, parallels: [{ name: 'fan', join: 'collate', maxConcurrentBranches: 2, failurePolicy: 'all_or_nothing', onFailure: '@abort', branches: [{ name: 'left', start: 'inspect-left' }, { name: 'right', start: 'inspect-right' }] }] } };
    const sourceFile = 'generated-child.yaml';
    fs.writeFileSync(sourceFile, JSON.stringify(document));
    const validation = await tool('validate_child_workflow', { sourceFile });
    if (validation.error || !validation.value.valid) throw new Error('parallel generated source rejected');
    if ((await tool('start_child_workflow', { sourceFile, invocationKey })).error) throw new Error('parallel generated acceptance failed');
    return waitForHost();
  }
  if (!status.value.resultRef) return waitForHost();
  if (status.value.state !== 'completed') throw new Error('parallel generated child failed');
  if (!status.value.acknowledged) {
    fs.writeFileSync('parent-after-child.txt', 'parent after child\n');
    if ((await tool('resolve_child_workflow', { invocationKey, action: 'merge', resultRef: status.value.resultRef })).error) throw new Error('parallel generated return refused');
    return waitForHost();
  }
  if (fs.readFileSync('child-return.txt','utf8') !== 'child return\n' || fs.readFileSync('parent-before-child.txt','utf8') !== 'parent before child\n' || fs.readFileSync('parent-after-child.txt','utf8') !== 'parent after child\n') throw new Error('parallel generated result changed parent content');
  return complete();
}
