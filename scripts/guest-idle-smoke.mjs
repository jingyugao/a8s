// Run in the retained AgentDeployment's server Pod. No temporary Pods are used.
import assert from 'node:assert/strict';
import {randomBytes} from 'node:crypto';
import {performance} from 'node:perf_hooks';
import {setTimeout as delay} from 'node:timers/promises';

const name = process.env.A8S_IDLE_DEPLOYMENT || 'idle-simple';
const instance = process.env.A8S_IDLE_INSTANCE;
assert(instance, 'provide the committed AgentDeployment instance ID');
const run = randomBytes(5).toString('hex');
const code = `MEM-${randomBytes(8).toString('hex')}`;
const tenant = process.env.A8S_TENANT || "local";
const project = process.env.A8S_PROJECT || `agent-${name}`;
const resource = {scope:`${tenant}/${project}`,deploymentId:name,workspaceId:`${name}-workspace`};
const headers = {Authorization:`Bearer ${process.env.A8S_TOKEN}`,'Content-Type':'application/json'};
const namespace = process.env.A8S_IDLE_NAMESPACE || process.env.A8S_KUBERNETES_NAMESPACE || "a8s-test";
const guestURL = `http://${instance}.${namespace}.svc:8094`;
const report = {run,instance,memoryCode:code,startedAt:new Date().toISOString(),samples:[]};
let threadId, lastResultId, streamAbort;
let streamPump;
async function http(base, route, body) {
 const response = await fetch(base+route,{method:body===undefined?'GET':'POST',headers,body:body===undefined?undefined:JSON.stringify(body),signal:AbortSignal.timeout(40000)});
 const value = await response.json();
 if(!response.ok) throw Error(`${route}: ${response.status}: ${JSON.stringify(value)}`);
 return value;
}
const status = async() => (await http(guestURL,'/v1/status')).slot;
const rpc = (method, fields={}) => http('http://127.0.0.1:8090',`/a8s.execution.v1.ExecutionService/${method}`,{resource:{...resource,threadId},...fields});
async function sample() {
 const s=await status();
 report.samples.push({at:new Date().toISOString(),phase:s.phase,workerPid:s.workerPid,activity:s.activity,activityError:s.activityError});
 return s;
}
async function turn(label,text) {
 const begin=performance.now();
 const accepted=await rpc('SubmitInput',{requestKey:`idle-${run}-${label}`,text});
 const result={requestId:accepted.record.requestId,turnId:accepted.record.turnId,acceptedMs:Math.round(performance.now()-begin)};
 const deadline=performance.now()+150000;
 while(performance.now()<deadline) {
  const {result: value}=await rpc('GetInputResult',{requestId:result.requestId});
  await sample();
  if(value.state==='Succeeded') { lastResultId=result.requestId; return {...result,elapsedMs:Math.round(performance.now()-begin),output:value.outputText}; }
  if(['Failed','Cancelled','Rejected'].includes(value.state))throw Error(`${label}: ${value.state}`);
  await delay(400);
 }
 throw Error(`${label}: outcome unknown; inspect ${result.requestId}`);
}
async function waitPhase(phase,timeout) {
 const begin=performance.now();
 while(performance.now()-begin<timeout){if(lastResultId){const cached=await rpc('GetInputResult',{requestId:lastResultId});assert.equal(cached.result.state,'Succeeded');}const s=await sample();if(s.phase===phase)return {slot:s,elapsedMs:Math.round(performance.now()-begin)};assert.notEqual(s.phase,'Quarantined');await delay(250);}
 throw Error(`did not reach ${phase}`);
}
try {
 report.initial=await status();
 const opened=await rpc('OpenThread',{requestKey:`idle-${run}-open`});threadId=opened.record.threadId;assert(threadId);report.threadId=threadId;
 report.runningBefore=await status();
 await delay(12000);
 report.emptyThread=await sample();
 assert.equal(report.emptyThread.phase,'Running');assert.equal(report.emptyThread.activity?.known,false);
 report.emptyThreadRetained=true;
 const file=`idle-${run}.done`;
 const child=`require('node:fs').writeFileSync(${JSON.stringify(file)},JSON.stringify({uid:process.getuid(),at:Date.now()}));`;
 const source=`const cp=require('node:child_process');const p=cp.spawn(process.execPath,['-e',${JSON.stringify(`setTimeout(()=>{${child}},45000);`)}],{detached:true,stdio:'ignore'});p.unref();console.log(JSON.stringify({backgroundPid:p.pid,uid:process.getuid()}));setTimeout(()=>console.log('foreground complete'),14000);`;
 const quote=value=>"'"+value.replaceAll("'","'\\''")+"'";
 const firstText=`这是运行时回收验证。本对话记忆码为 ${code}，只在对话上下文中记住，不写入文件。请用 Shell 原样执行下面命令一次，等前台14秒结束就报告成功；不要等待后台进程，不要追加检查或清理。\nnode -e ${quote(source)}`;
 report.firstTurn=await turn('background',firstText);
 console.error('first turn complete',report.firstTurn.elapsedMs);
 const payload=Buffer.from(JSON.stringify({resource:{...resource,threadId},inputRequestId:report.firstTurn.requestId}));
 const frame=Buffer.alloc(5);frame.writeUInt32BE(payload.length,1);streamAbort=new AbortController();
 const stream=await fetch('http://127.0.0.1:8090/a8s.execution.v1.ExecutionService/Subscribe',{method:'POST',headers:{...headers,'Content-Type':'application/connect+json','Connect-Protocol-Version':'1'},body:Buffer.concat([frame,payload]),signal:streamAbort.signal});
 assert.equal(stream.status,200);const reader=stream.body.getReader();let initial='';
 while(!initial.includes('ResyncRequired')) {const part=await reader.read();assert(!part.done,'event subscription must authenticate and open');initial+=Buffer.from(part.value).toString();assert(initial.length<65536);}
 report.subscriptionOpened=true;
 streamPump=(async()=>{while(!(await reader.read()).done){};report.subscriptionEnded=true;})().catch(error=>{report.subscriptionError=String(error);});
 assert(report.samples.some(s=>(s.activity?.activeTurns??0)>0),'foreground turn must be observed');
 for(let i=0;i<13;i++){const s=await sample();assert.equal(s.phase,'Running','background job must keep runtime alive beyond idleTimeout');await delay(1000);}
 assert(report.samples.some(s=>(s.activity?.backgroundTasks??0)>0),'detached process must be observed');
 report.sleep=await waitPhase('Suspended',70000);
 assert.equal(report.sleep.slot.historyFlushed,true);assert.equal(report.sleep.slot.processReleased,true);assert.equal(report.sleep.slot.forced??false,false);
 assert.deepEqual(report.sleep.slot.binding,report.runningBefore.binding);
 for(let i=0;i<8;i++){const cached=await rpc('GetInputResult',{requestId:report.firstTurn.requestId});assert.equal(cached.result.state,'Succeeded');assert.equal((await status()).phase,'Suspended');await delay(250);}
 report.resultPollingKeptSuspended=true;
 await Promise.race([streamPump,delay(2000)]);assert(report.subscriptionEnded,'idle reclamation must release a passive subscription');
 const awake=performance.now();
 report.secondTurn=await turn('resume','上轮我让你仅在对话中记住的记忆码是什么？只回复那个记忆码，不用工具。');
 report.wakeAndReplyMs=Math.round(performance.now()-awake);
 assert(report.secondTurn.output.includes(code),'same native thread must remember prior conversation');
 report.runningAfter=await status();
 const replay=await http(guestURL,'/a8s.agentc.v1.AgentcService/SubmitInput',{binding:report.runningAfter.binding,requestId:report.firstTurn.requestId,threadId,text:firstText});
 assert.equal(replay.result.turnId,report.firstTurn.turnId);report.nativeReceiptReplayed=true;
 assert.deepEqual(report.runningAfter.binding,report.runningBefore.binding);assert.notEqual(report.runningAfter.workerPid,report.runningBefore.workerPid);
 // Replay the original accepted command after wake. Native receipts remain in
 // the same RuntimeDirectory, so neither the model turn nor shell runs twice.
 report.replay=await rpc('SubmitInput',{requestKey:`idle-${run}-background`,text:`intentionally different payload must conflict`}).then(()=>({unexpected:true}),err=>({conflict:String(err).includes('409')||String(err).includes('conflict')}));
 assert(report.replay.conflict,'same request key with a changed payload must be rejected');
 report.finalSleep=await waitPhase('Suspended',30000);
 report.passed=true;
} catch(error) {report.error=String(error);process.exitCode=1;}
if(streamAbort)streamAbort.abort();
console.log(JSON.stringify(report,null,2));
