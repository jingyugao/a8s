// Run against the existing deployment; no new Pods or workspace restarts:
// kubectl --context k3s -n a8s-test exec -i deployment/guest-smoke -- \
//   node --input-type=module < scripts/guest-parallel-smoke.mjs > tmp/guest-parallel-smoke.json
import assert from 'node:assert/strict';
import { randomBytes } from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import { performance } from 'node:perf_hooks';
import { setTimeout as delay } from 'node:timers/promises';

const runId = randomBytes(6).toString('hex');
const report = { runId, startedAt: new Date().toISOString(), conversations: [] };
const token = process.env.A8S_GUEST_TOKEN;
let slot;
const states = new Map();

async function request(route, body) {
  const headers = { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' };
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

function rpc(method, data) {
  return request(`/a8s.agentc.v1.AgentcService/${method}`, { binding: slot.binding, ...data });
}

async function turn(conversation, text, round) {
  const result = { requestId: `parallel-${runId}-${conversation.label}-${round}`, transitions: [] };
  conversation[round] = result;
  result.sentAt = new Date().toISOString();
  const begin = performance.now();
  console.error(`submit ${conversation.label}/${round}: thread=${conversation.threadId} request=${result.requestId}`);
  const accepted = await rpc('SubmitInput', {
    requestId: result.requestId, threadId: conversation.threadId, text,
  });
  result.acceptedMs = Math.round(performance.now() - begin);
  result.turnId = accepted.result.turnId;
  const deadline = performance.now() + 150000;
  while (performance.now() < deadline) {
    const { result: observed } = await rpc('GetInputResult', {
      requestId: result.requestId, threadId: conversation.threadId, turnId: result.turnId,
    });
    const state = observed.state;
    if (result.transitions.at(-1)?.state !== state) {
      result.transitions.push({ state, at: new Date().toISOString() });
    }
    states.set(conversation.label, state);
    if (states.size === 2 && [...states.values()].every(value => value === 'Running')) {
      report.bothTurnsObservedRunning = true;
    }
    if (state === 'Succeeded') {
      result.elapsedMs = Math.round(performance.now() - begin);
      result.output = observed.outputText ?? '';
      result.completedAt = new Date().toISOString();
      console.error(`completed ${conversation.label}/${round}: ${result.elapsedMs}ms`);
      return;
    }
    if (state === 'Failed' || state === 'Cancelled') {
      throw new Error(`${conversation.label}/${round} ended with ${state}`);
    }
    await delay(200);
  }
  throw new Error(`${result.requestId}: outcome unknown; query this original request ID without resubmitting`);
}

try {
  assert(token, 'guest API credential must already be provisioned');
  const status = await request('/v1/status');
  slot = status.slot;
  assert(status.warm && slot.phase === 'Running', 'an existing running workspace is required');
  report.workspaceId = slot.request.workspaceId;
  report.generation = slot.request.generation;
  report.workerPid = slot.workerPid;
  report.podName = process.env.HOSTNAME;

  for (const label of ['a', 'b']) {
    report.conversations.push({
      label, memoryCode: `${label.toUpperCase()}-${randomBytes(8).toString('hex')}`,
      proofFile: `parallel-${runId}-${label}.json`,
      openRequestId: `parallel-${runId}-open-${label}`,
    });
  }
  await Promise.all(report.conversations.map(async conversation => {
    const begin = performance.now();
    const opened = await rpc('OpenThread', { requestId: conversation.openRequestId });
    conversation.threadId = opened.threadId;
    conversation.openThreadMs = Math.round(performance.now() - begin);
    assert(conversation.threadId);
  }));
  assert.notEqual(report.conversations[0].threadId, report.conversations[1].threadId);

  const firstBegin = performance.now();
  const firstResults = await Promise.allSettled(report.conversations.map(conversation => {
    // The memory code deliberately never appears in the shell command/proof file.
    const command = `const fs=require("node:fs");const file=${JSON.stringify(conversation.proofFile)};const value={label:${JSON.stringify(conversation.label)},pid:process.pid,uid:process.getuid(),cwd:process.cwd(),startMs:Date.now()};fs.writeFileSync(file,JSON.stringify(value));setTimeout(()=>{value.endMs=Date.now();fs.writeFileSync(file,JSON.stringify(value));console.log(JSON.stringify(value));},8000);`;
    const quoted = "'" + command.replaceAll("'", "'\\''") + "'";
    const text = `这是并行对话验证。本对话的记忆码是 ${conversation.memoryCode}，请仅在本对话上下文中记住，不写入文件，下一轮会问你。现在使用 Shell 直接执行以下命令（等待8秒，用来测量工具是否同时执行），完成后简短报告实际结果。\nnode -e ${quoted}`;
    return turn(conversation, text, 'firstTurn');
  }));
  report.firstRoundWallMs = Math.round(performance.now() - firstBegin);
  for (const result of firstResults) {
    if (result.status === 'rejected') throw result.reason;
  }

  for (const conversation of report.conversations) {
    const file = path.join(slot.directory, conversation.proofFile);
    const info = fs.statSync(file);
    conversation.proof = JSON.parse(fs.readFileSync(file, 'utf8'));
    assert.equal(info.uid, slot.request.uid, 'the model tool must write as the workspace UID');
    assert.equal(conversation.proof.uid, slot.request.uid);
    assert.equal(conversation.proof.cwd, slot.directory);
    assert.equal(conversation.proof.label, conversation.label);
    assert(conversation.proof.endMs - conversation.proof.startMs >= 7900);
  }
  const [a, b] = report.conversations.map(conversation => conversation.proof);
  report.shellOverlapMs = Math.max(0, Math.min(a.endMs, b.endMs) - Math.max(a.startMs, b.startMs));
  assert(report.bothTurnsObservedRunning, 'both native turns must be observed Running concurrently');
  assert(report.shellOverlapMs > 0, 'actual shell execution intervals must overlap');

  states.clear();
  const followupBegin = performance.now();
  const followups = await Promise.allSettled(report.conversations.map(conversation => turn(
    conversation,
    '上一轮告诉你的本对话记忆码是什么？不要使用工具或读取文件，只回复记忆码。',
    'followup',
  )));
  report.followupRoundWallMs = Math.round(performance.now() - followupBegin);
  for (const result of followups) {
    if (result.status === 'rejected') throw result.reason;
  }
  for (const conversation of report.conversations) {
    const other = report.conversations.find(value => value !== conversation);
    assert(conversation.followup.output.includes(conversation.memoryCode), 'thread must retain its own context');
    assert(!conversation.followup.output.includes(other.memoryCode), 'thread must not return the other context');
  }
  report.contextChecksPassed = true;
  const final = await request('/v1/status');
  assert.equal(final.slot.phase, 'Running');
  assert.equal(final.slot.workerPid, slot.workerPid);
  assert.equal(final.slot.request.generation, slot.request.generation);
  report.sameWorkerRetained = true;
  report.succeeded = true;
} catch (error) {
  report.error = error.message;
  report.succeeded = false;
  process.exitCode = 1;
} finally {
  report.finishedAt = new Date().toISOString();
  process.stdout.write(JSON.stringify(report, null, 2) + '\n');
}
