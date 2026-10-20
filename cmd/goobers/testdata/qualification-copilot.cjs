#!/usr/bin/env node
// Deterministic Copilot SDK endpoint. Only the external CLI/model is replaced:
// readiness and every journey tool call share the registered real MCP process.
const fs = require('node:fs');
const net = require('node:net');
const { spawn } = require('node:child_process');
const readline = require('node:readline');
const { randomUUID } = require('node:crypto');
const { runCopilotJourney } = require('./qualification-claude.cjs');
const args = process.argv.slice(2);
if (args.includes('--version')) {
  process.stdout.write('GitHub Copilot CLI 1.0.98 (qualification fixture)\n');
  process.exit(0);
}
if (!args.includes('--headless') || args.includes('--usage-output-file')) throw new Error('expected SDK headless invocation');
if (process.env.COPILOT_GITHUB_TOKEN !== 'qualification-copilot-model-token' ||
    ['GH_TOKEN','GITHUB_TOKEN','ANTHROPIC_API_KEY','CLAUDE_CODE_OAUTH_TOKEN','CODEX_API_KEY'].some(key => process.env[key])) throw new Error('Copilot model credential differs');
const childTools = ['validate_child_workflow','start_child_workflow','get_child_workflow','resolve_child_workflow'];
let mcp, sessionID, registration, availableTools, ready = false, connected = false;
let mcpID = 0;
const mcpPending = new Map();
const permissions = new Map();
function fail() {
  // Never print request bodies, arguments, configuration, tokens or responses.
  process.stderr.write('Copilot qualification protocol failed\n');
  if (mcp) mcp.kill();
  process.exitCode = 1;
  server.close();
  for (const socket of sockets) socket.destroy();
}
function mcpRequest(method, params) {
  const id = ++mcpID;
  return new Promise((resolve, reject) => {
    mcpPending.set(id, { resolve, reject });
    mcp.stdin.write(JSON.stringify({ jsonrpc: '2.0', id, method, params }) + '\n');
  });
}
async function openMCP(params) {
  if (mcp || !params.sessionId || Object.keys(params.mcpServers || {}).length !== 1) throw new Error('session configuration differs');
  sessionID = params.sessionId;
  registration = params.mcpServers['goobers-io'];
  availableTools = params.availableTools;
  if (!registration.command || registration.args?.[0] !== 'mcp-io' || registration.args?.[1] !== '--config' || registration.cwd !== params.workingDirectory) throw new Error('MCP launch configuration differs');
  for (const tool of childTools) {
    if (!registration.tools?.includes(tool) || !availableTools?.includes('goobers-io-' + tool)) throw new Error('parent tools missing');
  }
  const config = JSON.parse(fs.readFileSync(registration.args[2], 'utf8'));
  const encoded = JSON.stringify(params);
  if (!config.childWorkflows || encoded.includes(config.childWorkflows.bearerToken) || encoded.includes(config.childWorkflows.endpoint)) throw new Error('grant exposure');
  mcp = spawn(registration.command, registration.args, {
    cwd: params.workingDirectory, env: { ...process.env, ...registration.env }, stdio: ['pipe','pipe','pipe']
  });
  mcp.on('error', fail);
  mcp.stderr.resume();
  readline.createInterface({ input: mcp.stdout }).on('line', line => {
    try {
      const response = JSON.parse(line);
      const pending = mcpPending.get(response.id);
      if (pending) { mcpPending.delete(response.id); pending.resolve(response); }
    } catch { fail(); }
  });
  mcp.on('exit', () => {
    for (const pending of mcpPending.values()) pending.reject(new Error('MCP exited'));
    mcpPending.clear();
  });
  const initialized = await mcpRequest('initialize', { protocolVersion:'2025-11-25', capabilities:{}, clientInfo:{name:'copilot-qualification',version:'1'} });
  if (initialized.error) throw new Error('MCP initialization refused');
  mcp.stdin.write(JSON.stringify({jsonrpc:'2.0',method:'notifications/initialized'}) + '\n');
  return {sessionId:sessionID};
}
function send(socket, value) {
  const body = Buffer.from(JSON.stringify(value));
  socket.write(`Content-Length: ${body.length}\r\n\r\n`);
  socket.write(body);
}
function event(socket, type, data) {
  send(socket, {jsonrpc:'2.0',method:'session.event',params:{sessionId:sessionID,event:{id:randomUUID(),timestamp:new Date().toISOString(),type,data}}});
}
async function authorizedRequest(socket, method, params) {
  if (method !== 'tools/call') return mcpRequest(method, params);
  if (!registration.tools.includes(params.name) || !availableTools.includes('goobers-io-' + params.name)) throw new Error('tool outside session allowlist');
  const requestId = randomUUID();
  const approved = new Promise(resolve => permissions.set(requestId, resolve));
  event(socket, 'permission.requested', {requestId,permissionRequest:{kind:'mcp',serverName:'goobers-io',toolName:params.name,toolTitle:params.name,readOnly:params.name.startsWith('get_')}});
  if (!(await approved)) throw new Error('native permission refused');
  return mcpRequest(method, params);
}
async function dispatch(socket, method, params) {
  if (method === 'connect') {
    if (!process.env.COPILOT_CONNECTION_TOKEN || params.token !== process.env.COPILOT_CONNECTION_TOKEN) throw new Error('connection authority differs');
    connected = true;
    return {protocolVersion:3};
  }
  if (!connected) throw new Error('connect required');
  if (method === 'session.create') return openMCP(params);
  if (params.sessionId !== sessionID) throw new Error('session identity differs');
  switch (method) {
    case 'session.tools.initializeAndValidate': return {};
    case 'session.mcp.list': return {servers:[{name:'goobers-io',status:'connected'}]};
    case 'session.mcp.listTools': {
      const response = await mcpRequest('tools/list', {});
      if (response.error) throw new Error('inventory refused');
      return response.result;
    }
    case 'session.permissions.handlePendingPermissionRequest': {
      const resolve = permissions.get(params.requestId);
      if (!resolve) throw new Error('unknown permission');
      permissions.delete(params.requestId);
      resolve(params.result?.kind === 'approve-once');
      return {success:true};
    }
    case 'session.tools.execute': {
      if (params.name !== 'goobers-io-get_run_info') throw new Error('unexpected readiness tool');
      const response = await authorizedRequest(socket, 'tools/call', {name:'get_run_info',arguments:{}});
      if (response.error || response.result.isError) throw new Error('readiness refused');
      ready = true;
      return {resultType:'success',textResultForLlm:response.result.content[0].text};
    }
    case 'session.send': {
      if (!ready) throw new Error('model preceded readiness');
      // Defer events until the SDK has processed the send acknowledgement.
      setImmediate(() => runCopilotJourney({
        prompt:params.prompt,
        request:(method, params) => authorizedRequest(socket, method, params),
        complete:result => {
          event(socket, 'assistant.message', {messageId:'answer',content:JSON.stringify(result)});
          event(socket, 'session.idle', {});
        }
      }).catch(fail));
      return {messageId:'answer'};
    }
    case 'session.usage.getMetrics': {
      const model = {requests:{count:1,cost:1},usage:{inputTokens:42,outputTokens:2},totalNanoAiu:1e9};
      return {sessionStartTime:new Date().toISOString(),totalNanoAiu:1e9,modelMetrics:{'fixture-model':model},agentMetrics:{main:{totalNanoAiu:1e9,modelMetrics:{'fixture-model':model}}}};
    }
    case 'session.shutdown': return {success:true};
    default: throw new Error('unexpected RPC');
  }
}
const sockets = new Set();
const server = net.createServer(socket => {
  sockets.add(socket);
  let buffer = Buffer.alloc(0);
  socket.on('data', chunk => {
    buffer = Buffer.concat([buffer, chunk]);
    try {
      for (;;) {
        const end = buffer.indexOf('\r\n\r\n');
        if (end < 0) break;
        const match = buffer.subarray(0,end).toString().match(/^Content-Length: ([0-9]+)$/im);
        if (!match) throw new Error('framing');
        const length = Number(match[1]);
        if (length > (8 << 20)) throw new Error('frame bound');
        if (buffer.length < end + 4 + length) break;
        const request = JSON.parse(buffer.subarray(end+4,end+4+length));
        buffer = buffer.subarray(end+4+length);
        // Permission replies arrive while the tool request awaits approval.
        dispatch(socket, request.method, request.params || {}).then(result => {
          if (request.id !== undefined) send(socket, {jsonrpc:'2.0',id:request.id,result});
        }).catch(fail);
      }
    } catch { fail(); }
  });
  socket.on('error', fail);
  socket.on('close', () => { sockets.delete(socket); if (mcp) mcp.kill(); server.close(); process.exit(); });
});
server.listen(0, '127.0.0.1', () => process.stdout.write(`listening on port ${server.address().port}\n`));
