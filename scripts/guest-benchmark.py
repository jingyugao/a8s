# /// script
# requires-python = ">=3.11"
# dependencies = []
# ///
"""Measure guest claims on the deployed Agent, then verify UID isolation/restart.

Run with uv run --script scripts/guest-benchmark.py --model-smoke.
The existing a8s-test secrets stay in memory; output contains no credentials.
"""

import argparse
import base64
import json
import secrets
import select
import socket
import statistics
import subprocess
import threading
import time
import urllib.error
import urllib.request
from datetime import datetime
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--context", default="k3s")
    parser.add_argument("--namespace", default="a8s-test")
    parser.add_argument("--samples", type=int, default=5)
    parser.add_argument("--model-smoke", action="store_true")
    parser.add_argument("--output", type=Path, default=Path("tmp/guest-benchmark.json"))
    args = parser.parse_args()
    if not 1 <= args.samples <= 20:
        parser.error("samples must be between 1 and 20")
    kube = ["kubectl", "--context", args.context, "-n", args.namespace]

    def kubectl(*argv):
        return subprocess.run(
            [*kube, *argv], check=True, capture_output=True, text=True, timeout=30
        ).stdout

    secret = json.loads(kubectl("get", "secret", "a8s-server", "-o", "json"))
    token = base64.b64decode(secret["data"]["A8S_TOKEN"]).decode()
    pods = json.loads(kubectl("get", "pods", "-l", "app=guest-smoke", "-o", "json"))[
        "items"
    ]
    pods = [
        p
        for p in pods
        if not p["metadata"].get("deletionTimestamp")
        and p["status"]["phase"] == "Running"
    ]
    if len(pods) != 1:
        raise RuntimeError("exactly one running guest-smoke Pod is required")
    pod = pods[0]
    pod_name = pod["metadata"]["name"]
    ready_at = next(
        c["lastTransitionTime"]
        for c in pod["status"]["conditions"]
        if c["type"] == "Ready" and c["status"] == "True"
    )
    timestamp = datetime.fromisoformat
    report = {
        "pod": pod_name,
        "podUID": pod["metadata"]["uid"],
        "runtimeImage": pod["spec"]["containers"][0]["image"],
        "creationToPodReadyMs": int(
            (
                timestamp(ready_at) - timestamp(pod["metadata"]["creationTimestamp"])
            ).total_seconds()
            * 1000
        ),
        "samples": [],
    }
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    forwarding = subprocess.Popen(
        [
            *kube,
            "port-forward",
            "--address",
            "127.0.0.1",
            f"pod/{pod_name}",
            f"{port}:8090",
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )
    endpoint = f"http://127.0.0.1:{port}"

    def request(path, data=None, slot=None, expected=200):
        headers = {
            "Authorization": "Bearer " + token,
            "Content-Type": "application/json",
        }
        if slot:
            headers |= {
                "A8s-Workspace": slot["request"]["workspaceId"],
                "A8s-Generation": str(slot["request"]["generation"]),
            }
        req = urllib.request.Request(
            endpoint + path,
            data=json.dumps(data).encode() if data is not None else None,
            headers=headers,
        )
        try:
            response = urllib.request.urlopen(req, timeout=45)
        except urllib.error.HTTPError as error:
            response = error
        value = json.loads(response.read())
        if response.code != expected:
            raise RuntimeError(f"{path}: status={response.code}, result={value}")
        return value

    def rpc(method, data, slot, expected=200):
        return request(
            "/a8s.agentc.v1.AgentcService/" + method,
            {"binding": slot["binding"]} | data,
            slot,
            expected,
        )

    def as_uid(uid, script):
        return kubectl(
            "exec",
            pod_name,
            "--",
            "/usr/bin/setpriv",
            f"--reuid={uid}",
            f"--regid={uid}",
            "--clear-groups",
            "--inh-caps=-all",
            "--ambient-caps=-all",
            "--bounding-set=-all",
            "--no-new-privs",
            "node",
            "-e",
            script,
        ).strip()

    def stop(slot):
        value = request(
            "/v1/stop",
            {
                "workspaceId": slot["request"]["workspaceId"],
                "generation": slot["request"]["generation"],
                "force": True,
            },
        )
        assert value["phase"] == "Stopped" and value["processReleased"]
        return value

    try:
        # Wait for this forwarder to bind before sending the bearer token. A
        # random free port alone is insufficient evidence that kubectl owns it.
        deadline = time.monotonic() + 15
        while True:
            readable, _, _ = select.select([forwarding.stdout], [], [], 0.1)
            if readable:
                line = forwarding.stdout.readline()
                if line.startswith(f"Forwarding from 127.0.0.1:{port} ->".encode()):
                    break
            if forwarding.poll() is not None or time.monotonic() > deadline:
                raise RuntimeError("kubectl port-forward did not become ready")

        def drain_forwarding_output():
            for _ in forwarding.stdout:
                pass

        threading.Thread(target=drain_forwarding_output, daemon=True).start()
        deadline = time.monotonic() + 15
        while True:
            try:
                status = request("/v1/status")
                break
            except (urllib.error.URLError, ConnectionError):
                if time.monotonic() > deadline or forwarding.poll() is not None:
                    raise
                time.sleep(0.05)
        assert status["warm"]
        current = status["slot"]
        if current["phase"] not in ("Idle", "Stopped"):
            raise RuntimeError(
                "guest slot is occupied; inspect it before running another benchmark"
            )
        generation = current["request"].get("generation", 0)

        def start(workspace, uid):
            nonlocal generation
            generation += 1
            req = {
                "workspaceId": workspace,
                "uid": uid,
                "generation": generation,
                "requestKey": f"{workspace}-{generation}",
            }
            begin = time.perf_counter()
            value = request("/v1/start", req)
            elapsed = (time.perf_counter() - begin) * 1000
            assert value["phase"] == "Running"
            return value, elapsed

        run_id = secrets.token_hex(6)
        uid_base = 1000000 + secrets.randbelow(1000000000)
        report["runId"], report["uidBase"] = run_id, uid_base
        for index in range(args.samples):
            slot, elapsed = start(f"speed-{run_id}-{index}", uid_base + index)
            begin = time.perf_counter()
            opened = rpc("OpenThread", {"requestId": f"speed-open-{generation}"}, slot)
            open_ms = (time.perf_counter() - begin) * 1000
            assert opened["threadId"]
            replay = request("/v1/start", slot["request"])
            assert replay["workerPid"] == slot["workerPid"]
            stopped = stop(slot)
            sample = {
                "workspace": slot["request"]["workspaceId"],
                "claimToReadyMs": round(elapsed, 2),
                "serverStartMs": slot["startMs"],
                "openThreadMs": round(open_ms, 2),
                "claimToThreadMs": round(elapsed + open_ms, 2),
                "stopMs": stopped["stopMs"],
            }
            report["samples"].append(sample)
            print(json.dumps(sample), flush=True)

        other, _ = start("guest-b", 23002)
        as_uid(
            23002,
            'require("node:fs").writeFileSync("/runtime/workspaces/guest-b/work/private.txt","workspace-b-only");',
        )
        stop(other)
        slot, _ = start("guest-a", 23001)
        checks = json.loads(
            as_uid(
                23001,
                'const fs=require("node:fs");let denied=false;try{fs.readFileSync("/runtime/workspaces/guest-b/work/private.txt");}catch(e){denied=e.code==="EACCES";}fs.writeFileSync("/runtime/workspaces/guest-a/work/uid-owned.txt",String(process.getuid()));console.log(JSON.stringify({uid:process.getuid(),otherWorkspaceDenied:denied}));',
            )
        )
        assert checks == {"uid": 23001, "otherWorkspaceDenied": True}
        processes = json.loads(
            as_uid(
                23001,
                'const fs=require("node:fs");const result=[];for(const p of fs.readdirSync("/proc").filter(s=>Number.isInteger(Number(s))&&Number(s)!==process.pid)){try{const rows=fs.readFileSync(`/proc/${p}/status`,"utf8").split(String.fromCharCode(10));const s=Object.fromEntries(rows.map(row=>{const i=row.indexOf(":");return [row.slice(0,i),row.slice(i+1).trim()];}));if(Number.parseInt(s.Uid,10)!==23001)continue;const env=fs.readFileSync(`/proc/${p}/environ`,"utf8").split(String.fromCharCode(0)).map(v=>v.split("=")[0]);result.push({pid:Number(p),name:s.Name,capEff:s.CapEff,noNewPrivs:Number(s.NoNewPrivs),guestTokenPresent:env.includes("A8S_GUEST_TOKEN"),databaseCredentialPresent:env.includes("A8S_DATABASE_URL")});}catch{}}console.log(JSON.stringify(result));',
            )
        )
        assert len(processes) >= 4
        assert all(
            p["capEff"] == "0000000000000000"
            and p["noNewPrivs"] == 1
            and not p["guestTokenPresent"]
            and not p["databaseCredentialPresent"]
            for p in processes
        )
        report["isolation"] = checks | {"processes": processes}
        stale = rpc("OpenThread", {"requestId": "stale-probe"}, other, expected=409)
        assert "generation" in stale["error"]
        report["staleBindingRejected"] = True

        def turn(thread, text, active, key):
            begin = time.perf_counter()
            rpc(
                "SubmitInput",
                {"requestId": key, "threadId": thread, "text": text},
                active,
            )
            deadline = time.monotonic() + 150
            while time.monotonic() < deadline:
                result = rpc(
                    "GetInputResult", {"requestId": key, "threadId": thread}, active
                )["result"]
                if result["state"] == "Succeeded":
                    return {
                        "elapsedMs": round((time.perf_counter() - begin) * 1000, 2),
                        "output": result.get("outputText", ""),
                    }
                if result["state"] in ("Failed", "Cancelled"):
                    raise RuntimeError(f"model turn failed: {result}")
                time.sleep(0.25)
            raise RuntimeError(
                f"input {key} outcome unknown; query it instead of resubmitting"
            )

        if args.model_smoke:
            as_uid(
                23001,
                'const fs=require("node:fs");for(const name of ["remember.txt","uid-proof.txt"]){fs.rmSync("/runtime/workspaces/guest-a/work/"+name,{force:true});}',
            )
            thread = rpc("OpenThread", {"requestId": f"model-open-{generation}"}, slot)[
                "threadId"
            ]
            report["threadId"] = thread
            report["firstTurn"] = turn(
                thread,
                "请使用 Shell 执行 id -u、pwd 和 prettier --version，并把标记 warm-guest-23001 写入当前工作目录的 remember.txt，再把 id -u 的输出写入 uid-proof.txt。只报告实际结果。",
                slot,
                f"first-model-{generation}",
            )
            assert (
                as_uid(
                    23001,
                    'const fs=require("node:fs");console.log(fs.readFileSync("/runtime/workspaces/guest-a/work/remember.txt","utf8").trim());',
                )
                == "warm-guest-23001"
            )
            assert (
                as_uid(
                    23001,
                    'console.log(require("node:fs").readFileSync("/runtime/workspaces/guest-a/work/uid-proof.txt","utf8").trim());',
                )
                == "23001"
            )

        detached = int(
            as_uid(
                23001,
                'const cp=require("node:child_process");const p=cp.spawn("node",["-e",'
                + json.dumps('process.on("SIGTERM",()=>{});setInterval(()=>{},1000)')
                + '],{detached:true,stdio:"ignore"});console.log(p.pid);p.unref();',
            )
        )
        time.sleep(0.05)
        previous = slot
        released = stop(slot)
        report["detachedProcessKilled"] = (
            "gone"
            == kubectl(
                "exec",
                pod_name,
                "--",
                "node",
                "-e",
                f'try{{require("node:fs").statSync("/proc/{detached}");console.log("alive");}}catch{{console.log("gone");}}',
            ).strip()
        )
        assert report["detachedProcessKilled"]
        report["forcedStopMs"] = released["stopMs"]
        slot, resumed_ms = start("guest-a", 23001)
        report["samePodRestartMs"] = round(resumed_ms, 2)
        rpc(
            "OpenThread",
            {"requestId": "old-epoch-after-restart"},
            previous,
            expected=409,
        )
        if args.model_smoke:
            report["resumedTurn"] = turn(
                thread,
                "先使用 Shell 读取当前目录的 remember.txt，并运行 id -u，然后回复读到的标记和 UID。",
                slot,
                f"resume-model-{generation}",
            )
            assert "warm-guest-23001" in report["resumedTurn"]["output"]
        report["finalSlot"] = slot
        timings = [s["claimToThreadMs"] for s in report["samples"]]
        report["claimToThreadSummaryMs"] = {
            "count": len(timings),
            "min": min(timings),
            "median": round(statistics.median(timings), 2),
            "max": max(timings),
        }
        report["succeeded"] = True
    finally:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
        forwarding.terminate()
        try:
            forwarding.wait(timeout=5)
        except subprocess.TimeoutExpired:
            forwarding.kill()
            forwarding.wait()
    print(
        json.dumps(
            {
                "report": str(args.output),
                "summary": report["claimToThreadSummaryMs"],
                "succeeded": report["succeeded"],
            },
            ensure_ascii=False,
        ),
        flush=True,
    )


if __name__ == "__main__":
    main()
