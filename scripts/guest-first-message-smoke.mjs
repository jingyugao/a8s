// Measure a warm Pod with no workspace processes, through its first short reply.
// kubectl --context k3s -n a8s-test exec -i deployment/guest-smoke -- \
//   node --input-type=module < scripts/guest-first-message-smoke.mjs > tmp/guest-first-message-smoke.json
import assert from 'node:assert/strict';
import { randomBytes, randomInt } from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import { performance } from 'node:perf_hooks';
import { setTimeout as delay } from 'node:timers/promises';

const runId = randomBytes(6).toString('hex');
const report = { runId, podName: process.env.HOSTNAME, startedAt: new Date().toISOString(), samples: [] };
const uidBase = randomInt(1000000, 1000000000);
let original, active, generation;
const elapsed = begin => Math.round((performance.now() - begin) * 100) / 100;

async function request(route, body, slot) {
  const headers = { Authorization: `Bearer ${process.env.A8S_GUEST_TOKEN}`, 'Content-Type': 'application/json' };
  if (slot) {
    headers['A8s-Workspace'] = slot.request.workspaceId;
    headers['A8s-Generation'] = String(slot.request.generation);
  }
  const response = await fetch(`http://127.0.0.1:8090${route}`, {
    method: body === undefined ? 'GET' : 'POST', headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(40000),
  });
  const value = await response.json();
  if (!response.ok) throw new Error(`${route}: HTTP ${response.status}: ${JSON.stringify(value)}`);
  return value;
}
function rpc(method, data, slot) {
  return request(`/a8s.agentc.v1.AgentcService/${method}`, { binding: slot.binding, ...data }, slot);
}
function files(root) {
  if (!fs.existsSync(root)) return [];
  return fs.readdirSync(root, { withFileTypes: true }).flatMap(entry => {
    const file = path.join(root, entry.name);
    return entry.isDirectory() ? files(file) : [file];
  });
}
function records(file) {
  return fs.readFileSync(file, 'utf8').split('\n').filter(Boolean).flatMap(line => {
    try { return [JSON.parse(line)]; } catch { return []; }
  });
}
async function start(workspaceId, uid) {
  generation++;
  return request('/v1/start', { workspaceId, uid, generation, requestKey: `first-${runId}-${generation}` });
}
async function stop(slot) {
  const value = await request('/v1/stop', { workspaceId: slot.request.workspaceId, generation: slot.request.generation, force: true });
  assert(value.processReleased && value.phase === 'Stopped');
}

try {
  const status = await request('/v1/status');
  assert(status.warm, 'Agent content must already be warm');
  original = status.slot;
  assert.equal(original.phase, 'Running');
  assert.equal(original.request.workspaceId, 'guest-a', 'only the reserved validation workspace may be interrupted');
  generation = original.request.generation;
  const sessions = '/runtime/workspaces/guest-a/native/codex/sessions';
  for (const file of files(sessions).filter(file => file.endsWith('.jsonl'))) {
    const pending = new Set();
    for (const record of records(file)) {
      const payload = record.payload ?? {};
      if (payload.type === 'task_started') pending.add(payload.turn_id);
      if (payload.type === 'task_complete') pending.delete(payload.turn_id);
    }
    assert.equal(pending.size, 0, 'refuse to interrupt an unfinished original conversation');
  }
  await stop(original);
  report.originalWorkspaceStopped = true;

  for (let index = 0; index < 3; index++) {
    const sample = { workspaceId: `first-${runId}-${index}`, requestId: `first-message-${runId}-${index}` };
    report.samples.push(sample);
    const beginEpoch = Date.now();
    const begin = performance.now();
    active = await start(sample.workspaceId, uidBase + index);
    sample.claimToReadyMs = elapsed(begin);
    const openBegin = performance.now();
    sample.threadId = (await rpc('OpenThread', { requestId: `first-open-${runId}-${index}` }, active)).threadId;
    sample.openThreadMs = elapsed(openBegin);
    const submitBegin = performance.now();
    const accepted = await rpc('SubmitInput', {
      requestId: sample.requestId, threadId: sample.threadId,
      text: '你好，只回复“你好”，不要执行命令、读取文件或使用工具。',
    }, active);
    sample.turnId = accepted.result.turnId;
    sample.submitAcceptedMs = elapsed(submitBegin);
    const deadline = performance.now() + 120000;
    while (true) {
      const { result } = await rpc('GetInputResult', {
        requestId: sample.requestId, threadId: sample.threadId, turnId: sample.turnId,
      }, active);
      if (result.state === 'Succeeded') {
        sample.output = result.outputText;
        sample.messageToCompleteMs = elapsed(submitBegin);
        sample.claimToCompleteMs = elapsed(begin);
        break;
      }
      if (result.state === 'Failed' || result.state === 'Cancelled') throw new Error(`turn ${sample.turnId}: ${result.state}`);
      if (performance.now() >= deadline) throw new Error(`${sample.requestId}: outcome unknown; query the original request ID`);
      await delay(100);
    }
    assert(sample.output.includes('你好'));
    const nativeRoot = `/runtime/workspaces/${sample.workspaceId}/native/codex/sessions`;
    const log = files(nativeRoot).find(file => file.includes(sample.threadId));
    assert(log, 'native history must exist');
    for (let attempt = 0; attempt < 100; attempt++) {
      const events = records(log);
      const completed = events.find(record => record.payload?.type === 'task_complete' && record.payload.turn_id === sample.turnId);
      if (completed) {
        const started = events.find(record => record.payload?.type === 'task_started' && record.payload.turn_id === sample.turnId);
        sample.nativeTurnMs = completed.payload.duration_ms;
        sample.nativeFirstTokenMs = completed.payload.time_to_first_token_ms;
        sample.claimToFirstTokenMs = Date.parse(started.timestamp) - beginEpoch + sample.nativeFirstTokenMs;
        sample.toolCalls = events.filter(record => record.type === 'response_item' && ['custom_tool_call', 'function_call', 'local_shell_call', 'web_search_call'].includes(record.payload?.type)).length;
        assert.equal(sample.toolCalls, 0, 'a short greeting must not invoke tools');
        break;
      }
      await delay(20);
    }
    assert(Number.isFinite(sample.nativeFirstTokenMs), 'native first-token metric must be available');
    console.error(JSON.stringify(sample));
    await stop(active);
    active = undefined;
  }
  report.succeeded = true;
} catch (error) {
  report.error = error.message;
  report.succeeded = false;
  process.exitCode = 1;
} finally {
  if (report.originalWorkspaceStopped) {
    try {
      const current = (await request('/v1/status')).slot;
      if (current.phase !== 'Stopped' && current.request.workspaceId?.startsWith(`first-${runId}-`)) await stop(current);
      const released = (await request('/v1/status')).slot;
      assert.equal(released.phase, 'Stopped', 'unconfirmed process release blocks restoring the original workspace');
      generation = released.request.generation;
      const restored = await start(original.request.workspaceId, original.request.uid);
      report.restoredOriginal = { workspaceId: restored.request.workspaceId, generation: restored.request.generation, phase: restored.phase };
    } catch (error) {
      report.cleanupError = error.message;
      report.succeeded = false;
      process.exitCode = 1;
    }
  }
  report.finishedAt = new Date().toISOString();
  process.stdout.write(JSON.stringify(report, null, 2) + '\n');
}
