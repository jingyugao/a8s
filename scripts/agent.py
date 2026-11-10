# /// script
# requires-python = ">=3.11"
# dependencies = ["PyYAML==6.0.3", "tomli-w==1.2.0"]
# ///
"""Build Agentfiles against platform-owned runtime classes and manage AgentDeployments."""

import argparse
import base64
import hashlib
import json
import os
import re
import shlex
import socket
import subprocess
import time
import tomllib
from contextlib import contextmanager
from decimal import ROUND_CEILING, Decimal
from pathlib import Path
from string import Template

import tomli_w
import yaml

ROOT = Path(__file__).resolve().parents[1]


def run(argv, *, env=None, data=None, timeout=180):
    result = subprocess.run(
        [str(arg) for arg in argv],
        input=data,
        capture_output=True,
        text=True,
        env=env,
        timeout=timeout,
        check=False,
    )
    if result.returncode:
        raise RuntimeError(result.stderr.strip() or result.stdout.strip())
    return result.stdout


def write_json(file, value, *, immutable=False):
    file.parent.mkdir(parents=True, exist_ok=True)
    content = json.dumps(value, ensure_ascii=False, indent=2) + "\n"
    if immutable and file.exists():
        if json.loads(file.read_text()) != value:
            raise RuntimeError(f"原请求已存在且内容不同，保留原请求：{file}")
        return
    file.write_text(content)


def verify_runtime(image, runtime):
    """Verify the platform-owned harness bytes and Node version, without network access."""
    lock = json.loads((ROOT / "images/codex/lock.json").read_text())
    if runtime["harness"] != "codex" or runtime["codexVersion"] != lock["version"]:
        raise ValueError("runtime class 必须使用平台固定版本的原版 Codex")
    hashes = run(
        [
            "docker",
            "run",
            "--rm",
            "--network=none",
            "--entrypoint",
            "sha256sum",
            image,
            *[f"/usr/local/bin/{name}" for name in lock["binaries"]],
        ],
        timeout=30,
    )
    actual = {Path(line.split()[1]).name: line.split()[0] for line in hashes.splitlines()}
    if actual != lock["binaries"]:
        raise RuntimeError("镜像覆盖了平台固定的原版 Codex 二进制")
    version = run(
        ["docker", "run", "--rm", "--network=none", "--entrypoint", "node", image, "--version"], timeout=30
    ).strip()
    if version != "v" + runtime["nodeVersion"] or int(version[1:].split(".")[0]) < 22:
        raise RuntimeError("镜像中的 Node 版本与 runtime class 不一致")


def publish_runtime(file, tag):
    if not tag or not tag.endswith(":dev"):
        raise ValueError("平台源码构建统一使用 :dev 标签")
    runtime = json.loads(file.read_text())
    # Verify the immutable image before publishing the repository's development tag.
    image_id = run(["docker", "image", "inspect", tag, "--format", "{{.Id}}"]).strip()
    verify_runtime(image_id, runtime)
    run(
        [
            "docker",
            "run",
            "--rm",
            "--network=none",
            "--entrypoint",
            "sh",
            image_id,
            "-c",
            "test ! -e /agent && test ! -e /opt/a8s/package && npm --version && npx --version",
        ],
        timeout=30,
    )
    if run(["docker", "image", "inspect", tag, "--format", "{{.Id}}"]).strip() != image_id:
        raise RuntimeError("开发镜像已被其他构建覆盖，请重新构建 runtime class")
    subprocess.run(["docker", "push", tag], check=True, timeout=300)
    repository = tag.rsplit(":", 1)[0]
    inspected = json.loads(run(["docker", "image", "inspect", image_id]))[0]
    references = [ref for ref in inspected["RepoDigests"] if ref.startswith(repository + "@sha256:")]
    if len(references) != 1:
        raise RuntimeError("无法确认 runtime class 的已发布镜像 digest")
    runtime["image"] = references[0]
    write_json(file, runtime)
    return runtime


class Platform:
    def __init__(self, manifest):
        self.agent = yaml.safe_load(manifest.read_text())
        allowed = {
            "apiVersion",
            "kind",
            "name",
            "agentImage",
            "runtimeClassRef",
            "workingDir",
            "env",
            "resources",
            "update",
        }
        if not isinstance(self.agent, dict) or set(self.agent) - allowed:
            raise ValueError("AgentDeployment YAML 使用 a8s/v1，通过 runtimeClassRef 选择平台运行环境")
        if self.agent.get("apiVersion") != "a8s/v1":
            raise ValueError("apiVersion 必须为 a8s/v1")
        if self.agent.get("kind") != "AgentDeployment":
            raise ValueError("kind 必须为 AgentDeployment")
        self.name = self.agent.get("name", "")
        if not isinstance(self.name, str) or not re.fullmatch(r"[a-z][a-z0-9-]{0,39}", self.name):
            raise ValueError("name 使用小写字母开头，最多 40 个小写字母、数字或横线")
        if not isinstance(self.agent.get("agentImage"), str) or not self.agent["agentImage"].strip():
            raise ValueError("agentImage 必须是 Agent 镜像引用")
        ref = self.agent.get("runtimeClassRef", "")
        if not isinstance(ref, str) or not re.fullmatch(r"[a-z][a-z0-9-]*(?:@sha256:[a-f0-9]{64})?", ref):
            raise ValueError("runtimeClassRef 必须是平台内置 runtime class 名称，可附加 @sha256 revision")
        class_name, _, revision = ref.partition("@")
        class_file = ROOT / "images/runtime-classes" / class_name / "runtime-class.json"
        if not class_file.is_file():
            raise ValueError(f"未知 runtime class：{class_name}")
        self.runtime_class = json.loads(class_file.read_text())
        self.base_image = self.runtime_class["image"]
        if self.runtime_class["name"] != class_name or self.runtime_class["harness"] != "codex":
            raise ValueError("runtime class 名称或 harness 不兼容")
        if not re.search(r"@sha256:[a-f0-9]{64}$", self.base_image):
            raise ValueError("runtime class 尚未发布固定 digest 的平台镜像")
        if revision and revision != self.base_image.split("@")[1]:
            raise ValueError("指定的 runtime class revision 与平台目录不一致")
        self.runtime_class_ref = class_name + "@" + self.base_image.split("@")[1]
        if self.agent.get("env", {}) != {}:
            raise ValueError("当前 Provider 尚不支持声明级 env；非秘密默认值请使用 Agentfile ENV")
        if self.agent.get("update", {"strategy": "restart"}) != {"strategy": "restart"}:
            raise ValueError("update.strategy 仅支持 restart；实际安全更新尚未开放")
        self.working_dir = self.agent.get("workingDir", "/runtime")
        if (
            not isinstance(self.working_dir, str)
            or not self.working_dir.startswith("/")
            or ".." in Path(self.working_dir).parts
        ):
            raise ValueError("workingDir 必须是无 .. 的绝对目录")
        self.resources = self.agent.get("resources", {})
        if not isinstance(self.resources, dict) or set(self.resources) - {
            "cpu",
            "memory",
        }:
            raise ValueError("resources 仅支持 cpu、memory")
        cpu = self.resources.get("cpu", "1")
        memory = self.resources.get("memory", "1Gi")
        if not isinstance(cpu, str) or not re.fullmatch(r"[0-9]+(?:\.[0-9]+)?m?", cpu):
            raise ValueError('resources.cpu 使用 "1" 或 "500m" 等 CPU 数量')
        if not isinstance(memory, str) or not re.fullmatch(r"[0-9]+(?:\.[0-9]+)?(?:[KMGT]i|[kMGT])?", memory):
            raise ValueError('resources.memory 使用 "512Mi"、"1Gi" 等内存数量')
        self.cpu = Decimal(cpu.rstrip("m")) / (1000 if cpu.endswith("m") else 1)
        quantity = re.fullmatch(r"([0-9]+(?:\.[0-9]+)?)(.*)", memory)
        units = {
            "": 1,
            "k": 1000,
            "M": 1000**2,
            "G": 1000**3,
            "T": 1000**4,
            "Ki": 1024,
            "Mi": 1024**2,
            "Gi": 1024**3,
            "Ti": 1024**4,
        }
        self.memory_mib = int(
            (Decimal(quantity[1]) * units[quantity[2]] / (1024**2)).to_integral_value(rounding=ROUND_CEILING)
        )
        if self.cpu <= 0 or self.memory_mib <= 0:
            raise ValueError("CPU 和内存必须大于零")
        self.directory = manifest.resolve().parent
        self.config = {}
        for line in (ROOT / "deploy/k3s/platform.env").read_text().splitlines():
            if line.strip() and not line.lstrip().startswith("#"):
                key, value = line.split("=", 1)
                self.config[key] = os.environ.get(key, shlex.split(value)[0])
        self.ns = self.config["A8S_PLATFORM_NAMESPACE"]
        self.prefix = f"agent-{self.name}"
        self.scope = {
            "tenantId": self.config["A8S_PLATFORM_TENANT"],
            "projectId": self.prefix,
        }
        self.workspace = f"{self.name}-workspace"
        self.state = ROOT / "tmp/agents" / self.name
        self.state.mkdir(parents=True, exist_ok=True)
        self.lock = json.loads((ROOT / "images/codex/lock.json").read_text())
        self.cli = Path(os.environ.get("A8S_PLATFORM_CLI", ROOT / "dist/a8s"))
        self.env = os.environ.copy()
        self.endpoint = None

    def k(self, *args, data=None):
        return run(
            [
                "kubectl",
                "--context",
                self.config["A8S_PLATFORM_CONTEXT"],
                "-n",
                self.ns,
                *args,
            ],
            data=data,
        )

    def cli_json(self, *args):
        self.ensure_cli()
        return json.loads(run([self.cli, "--json", *args], env=self.env))

    def ensure_cli(self):
        if not self.cli.is_file():
            # A checkout of the example does not require Go or a local Codex installation.
            cache = ROOT / "tmp/agent-tools" / self.base_image.split("@")[1].split(":")[1]
            cache.mkdir(parents=True, exist_ok=True)
            self.cli = cache / "a8s"
            if not self.cli.is_file():
                result = subprocess.run(
                    [
                        "docker",
                        "run",
                        "--rm",
                        "--entrypoint",
                        "cat",
                        self.base_image,
                        "/usr/local/bin/a8s",
                    ],
                    capture_output=True,
                    timeout=180,
                    check=False,
                )
                if result.returncode:
                    raise RuntimeError(result.stderr.decode(errors="replace"))
                self.cli.write_bytes(result.stdout)
                self.cli.chmod(0o755)

    def build(self):
        if not (self.directory / "Agentfile").is_file():
            raise ValueError("Agent 目录必须包含 Agentfile；Dockerfile 由平台 runtime class 维护")
        tag = self.agent["agentImage"]
        if not tag.endswith(":dev"):
            raise ValueError("本地源码构建统一使用 :dev 标签；发布时解析固定 digest")
        print(f"构建独立 Agent 内容镜像 {tag}", flush=True)
        argv = [
            "docker",
            "build",
            "--platform",
            self.runtime_class["platform"],
            "-f",
            str(self.directory / "Agentfile"),
            "-t",
            tag,
            str(self.directory),
        ]
        subprocess.run(argv, check=True, timeout=900)
        image_id = run(["docker", "image", "inspect", tag, "--format", "{{.Id}}"])
        (self.state / "image-id.txt").write_text(image_id)
        return {"image": tag, "built": True}

    def api(self, group, command, *, body=None, flags=(), missing=False):
        self.ensure_cli()
        argv = [
            self.cli,
            "--json",
            "--endpoint",
            self.endpoint,
            "--tenant",
            self.scope["tenantId"],
            "--project",
            self.scope["projectId"],
            group,
            command,
        ]
        if body is not None:
            file = self.state / f"{group}-{command}-request.json"
            write_json(
                file,
                body,
                immutable=group == "deployment" and command in ("create", "update", "offline"),
            )
            argv += ["--file", file]
        result = subprocess.run(
            [str(arg) for arg in [*argv, *flags]],
            env=self.env,
            capture_output=True,
            text=True,
            timeout=200,
            check=False,
        )
        response = json.loads(result.stdout) if result.stdout else {}
        write_json(self.state / f"{group}-{command}-response.json", response)
        if result.returncode:
            if missing and response.get("error", {}).get("code") == "not_found":
                return None
            detail = response.get("error", {}).get("message") or result.stderr.strip()
            raise RuntimeError(detail or "平台 API 请求结果未知；请查询原 Operation")
        return response

    @contextmanager
    def connection(self):
        token = self.env.get("A8S_TOKEN")
        if not token:
            encoded = self.k("get", "secret", "a8s-server", "-o", "jsonpath={.data.A8S_TOKEN}")
            token = base64.b64decode(encoded).decode()
        self.env["A8S_TOKEN"] = token
        with socket.socket() as probe:
            probe.bind(("127.0.0.1", 0))
            port = probe.getsockname()[1]
        proc = subprocess.Popen(
            [
                "kubectl",
                "--context",
                self.config["A8S_PLATFORM_CONTEXT"],
                "-n",
                self.ns,
                "port-forward",
                "--address=127.0.0.1",
                f"service/{self.prefix}-a8s-server",
                f"{port}:8090",
            ],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            text=True,
        )
        try:
            deadline = time.monotonic() + 20
            while True:
                if proc.poll() is not None:
                    raise RuntimeError(proc.stderr.read().strip() or "平台端口转发失败")
                try:
                    with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                        break
                except OSError:
                    if time.monotonic() >= deadline:
                        raise TimeoutError("平台连接未在 20 秒内就绪") from None
                    time.sleep(0.1)
            self.endpoint = f"http://127.0.0.1:{port}"
            yield
        finally:
            if proc.poll() is None:
                proc.terminate()
                try:
                    proc.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()
            proc.stderr.close()

    def prepare(self):
        registry_flags = ["--plain-http"] if self.config["A8S_PLATFORM_REGISTRY_PLAIN_HTTP"] == "true" else []
        resolved = self.cli_json(
            "package",
            "resolve",
            "--artifact",
            "--ref",
            self.agent["agentImage"],
            *registry_flags,
        )
        self.image = resolved["reference"]
        self.source = self.state / "images" / resolved["digest"].split(":")[1]
        marker = self.source / "source.json"
        if not marker.is_file():
            self.source.mkdir(parents=True, exist_ok=True)
            if any(self.source.iterdir()):
                raise RuntimeError("镜像加载目录含未完成内容；保留原现场，请核对后重试")
            flags = ["--plain-http"] if registry_flags else []
            run(
                [
                    "docker",
                    "run",
                    "--rm",
                    "--user",
                    f"{os.getuid()}:{os.getgid()}",
                    "--volume",
                    f"{self.source}:/output",
                    "--entrypoint",
                    "a8s-agent-loader",
                    self.base_image,
                    "--image",
                    self.image,
                    "--name",
                    self.name,
                    "--destination",
                    "/output/agent",
                    "--package-layout",
                    "/output/package",
                    "--extract-only",
                    *flags,
                ],
                timeout=180,
            )
            write_json(marker, {"image": self.image})
        if json.loads(marker.read_text()) != {"image": self.image}:
            raise RuntimeError("镜像加载目录身份不一致")
        package = self.cli_json("package", "inspect", "--layout", self.source / "package")
        self.digest = package["digest"]
        self.package = f"{self.config['A8S_PLATFORM_PACKAGE_REPOSITORY']}@{self.digest}"
        mcp_file = self.source / "agent/.codex/config.toml"
        if mcp_file.exists() and not mcp_file.resolve().is_relative_to((self.source / "agent").resolve()):
            raise ValueError("MCP 配置必须位于 Agent 内容目录内")
        mcp = mcp_file.read_text() if mcp_file.exists() else ""
        agent_config = tomllib.loads(mcp)
        if set(agent_config) - {"mcp_servers"}:
            raise ValueError("Agent 的 .codex/config.toml 仅装载 mcp_servers，模型、认证和权限由平台提供")
        for server in agent_config.get("mcp_servers", {}).values():
            if "command" in server:
                server.setdefault("environment_id", "remote")
                server.setdefault("cwd", self.working_dir)
        mcp = tomli_w.dumps(agent_config)
        host = json.loads(self.k("get", "configmap", "a8s-host-config", "-o", "json"))["data"]["config.toml"]
        combined = host.rstrip() + "\n\n" + mcp
        tomllib.loads(combined)  # Duplicate server names or malformed TOML fail before submission.
        intent = {
            "agent": self.agent,
            "image": self.image,
            "runtimeClassRef": self.runtime_class_ref,
            "hostConfig": combined,
        }
        self.intent = hashlib.sha256(json.dumps(intent, sort_keys=True).encode()).hexdigest()
        self.component = f"{self.runtime_class['name']}-{self.intent[:12]}"
        values = {
            "A8S_EXAMPLE_PREFIX": self.prefix,
            "A8S_EXAMPLE_NAMESPACE": self.ns,
            "A8S_EXAMPLE_STORAGE_CLASS": self.config["A8S_PLATFORM_STORAGE_CLASS"],
            "A8S_TENANT": self.scope["tenantId"],
            "A8S_PROJECT": self.scope["projectId"],
            "A8S_EXAMPLE_PLAIN_HTTP": self.config["A8S_PLATFORM_REGISTRY_PLAIN_HTTP"],
            "A8S_IMAGE_REFERENCE": self.base_image,
            "A8S_PACKAGE_REFERENCE": self.package,
            "A8S_PACKAGE_DIGEST": self.digest,
        }
        rendered = Template((ROOT / "deploy/k3s/agent-instance.yaml").read_text()).substitute(values)
        documents = list(yaml.safe_load_all(rendered))
        documents.append(
            {
                "apiVersion": "v1",
                "kind": "ConfigMap",
                "metadata": {
                    "name": f"{self.prefix}-host-config",
                    "namespace": self.ns,
                },
                "data": {"config.toml": combined},
            }
        )
        for document in documents:
            if document["kind"] != "Deployment":
                continue
            template = document["spec"]["template"]
            template["metadata"]["annotations"] = {"a8s.io/agent-intent": self.intent}
            container = template["spec"]["containers"][0]
            for item in container["env"]:
                if item["name"] == "A8S_WORKSPACE_DIRECTORY":
                    item["value"] = self.working_dir
                elif item["name"] == "A8S_AGENTC_URL":
                    item["value"] = f"http://{{instance}}.{self.ns}.svc:8094"
            launch = {
                "command": ["/usr/local/bin/a8s-agent-loader", "--agentc"],
                "environment": {
                    "A8S_AGENT_IMAGE": self.image,
                    "A8S_AGENT_NAME": self.name,
                    "A8S_REGISTRY_PLAIN_HTTP": self.config["A8S_PLATFORM_REGISTRY_PLAIN_HTTP"],
                },
                "secretRefs": ["a8s-model"],
                "configMap": f"{self.prefix}-host-config",
                "controlPort": 8094,
            }
            container["env"].extend(
                [
                    {"name": "A8S_EXEC_SERVER_URL", "value": "ws://127.0.0.1:8095"},
                    {"name": "A8S_SANDBOX_LAUNCH", "value": json.dumps(launch)},
                ]
            )
        self.kubernetes = self.state / "kubernetes.yaml"
        self.kubernetes.write_text(yaml.safe_dump_all(documents, sort_keys=False, allow_unicode=True))
        image = {"reference": self.base_image, "digest": self.base_image.split("@")[1]}
        self.registration = {
            "agentc": {
                "scope": self.scope,
                "id": self.component,
                "artifact": image,
                "hostDigest": "sha256:" + self.lock["binaries"]["codex"],
                "protocolVersion": "a8s.agentc.v1",
                "capabilities": [
                    "positive_receipt_lookup",
                    "instance_dispatch_journal",
                    "live_events",
                ],
            }
        }
        self.runtime = {
            "runtimeRevision": {
                "scope": self.scope,
                "id": self.component,
                "agentcId": self.component,
                "sandbox": {
                    "provider": "kubernetes",
                    "image": image,
                    "environmentRevision": self.runtime_class["name"] + "-" + self.base_image.split(":")[-1][:12],
                    "architecture": "amd64",
                    "resources": {
                        "cpu": str(self.cpu),
                        "memory_mib": str(self.memory_mib),
                    },
                },
            }
        }
        self.create = {
            "scope": self.scope,
            "id": self.name,
            "workspaceId": self.workspace,
            "storageReference": f"{self.prefix}-a8s-workspace",
            "requestKey": "register-v1",
            "spec": {
                "packageReference": self.package,
                "runtimeRevisionId": self.component,
            },
        }
        for name, value in [
            ("runtime-class", {**self.runtime_class, "reference": self.runtime_class_ref}),
            ("agentc", self.registration),
            ("runtime", self.runtime),
            ("deployment", self.create),
        ]:
            write_json(self.state / f"plan-{name}.json", value)

    def publish(self):
        image_id = run(
            [
                "docker",
                "image",
                "inspect",
                self.agent["agentImage"],
                "--format",
                "{{.Id}}",
            ]
        )
        marker = self.state / "image-id.txt"
        if not marker.is_file() or marker.read_text() != image_id:
            raise RuntimeError("开发镜像已被其他构建覆盖；先执行 make build")
        print(f"发布 Agent 镜像 {self.agent['agentImage']}", flush=True)
        subprocess.run(["docker", "push", self.agent["agentImage"]], check=True, timeout=300)
        self.prepare()
        registry_flags = ["--plain-http"] if self.config["A8S_PLATFORM_REGISTRY_PLAIN_HTTP"] == "true" else []
        output = run(
            [
                self.cli,
                "--json",
                "package",
                "publish",
                "--timeout=120s",
                "--layout",
                self.source / "package",
                "--ref",
                self.package,
                *registry_flags,
            ],
            env=self.env,
        )
        write_json(self.state / "published-package.json", json.loads(output))
        return {"image": self.image, "package": self.package}

    def register(self):
        self.prepare()
        agentc = self.k("get", "statefulset", f"{self.prefix}-a8s-agentc", "--ignore-not-found", "-o", "json")
        if agentc:
            raise RuntimeError("仍存在旧的外置 Agentc 资源；请先核对原绑定，不覆盖运行中的实例")
        existing = self.k(
            "get",
            "deployment",
            f"{self.prefix}-a8s-server",
            "--ignore-not-found",
            "-o",
            "name",
        )
        if existing:
            with self.connection():
                current = self.api("deployment", "get", flags=["--id", self.name], missing=True)
                if current:
                    if current["deployment"]["spec"] != self.create["spec"]:
                        raise RuntimeError("Agent 已存在且内容变化；当前安全更新尚未完成，原运行实例保持不变")
                    return current
        registry_flags = ["--plain-http"] if self.config["A8S_PLATFORM_REGISTRY_PLAIN_HTTP"] == "true" else []
        self.cli_json("package", "resolve", "--ref", self.package, *registry_flags)
        self.k("get", "secret", "a8s-server", "a8s-model", "-o", "name")
        self.k("get", "configmap", "a8s-host-config", "-o", "name")
        self.k("apply", "-f", self.kubernetes)
        self.k(
            "rollout",
            "status",
            f"deployment/{self.prefix}-a8s-server",
            "--timeout=150s",
        )
        with self.connection():
            self.api("agentc", "register", body=self.registration)
            self.api("runtime", "create", body=self.runtime)
            return self.api(
                "deployment",
                "create",
                body=self.create,
                flags=["--wait", "--wait-timeout=180s"],
            )

    def status(self):
        with self.connection():
            response = self.api("deployment", "get", flags=["--id", self.name])
            operation = response["deployment"].get("operationId")
            if operation:
                response["operation"] = self.api(
                    "operation",
                    "get",
                    flags=["--deployment", self.name, "--id", operation],
                )["operation"]
            return response

    def unregister(self):
        with self.connection():
            listing = self.api("deployment", "list")
            deployments = listing.get("deployments", [])
            if listing.get("nextPageToken") or len(deployments) > 1:
                raise RuntimeError("平台 scope 存在额外部署，停止清理")
            if deployments:
                deployment = deployments[0]
                if deployment["id"] != self.name:
                    raise RuntimeError("部署身份不一致，停止清理")
                if deployment.get("activeInstanceId") or deployment.get("desiredState") != "DESIRED_STATE_OFFLINE":
                    request = {
                        "scope": self.scope,
                        "id": self.name,
                        "desiredState": "DESIRED_STATE_OFFLINE",
                        "expectedResourceVersion": deployment["resourceVersion"],
                        "requestKey": "unregister-v1",
                    }
                    write_json(self.state / "offline-request.json", request, immutable=True)
                    raise RuntimeError("当前安全排空和原生历史保留能力未完成；已保存下线请求，未提交或删除资源")
                operation = self.api(
                    "operation",
                    "get",
                    flags=[
                        "--deployment",
                        self.name,
                        "--id",
                        deployment["operationId"],
                    ],
                )["operation"]
                if operation["status"] != "OPERATION_STATUS_SUCCEEDED" or operation.get("cleanupPending"):
                    raise RuntimeError("原 Operation 尚未完成清理，保留资源")
            resources = [
                f"{kind}/{self.prefix}-{suffix}"
                for kind, suffix in [
                    ("deployment", "a8s-server"),
                    ("statefulset", "a8s-agentc"),
                    ("service", "a8s-server"),
                    ("service", "a8s-agentc"),
                    ("rolebinding", "a8s-server"),
                    ("role", "a8s-server"),
                    ("serviceaccount", "a8s-server"),
                    ("configmap", "host-config"),
                ]
            ]
            self.k("delete", *resources, "--ignore-not-found", "--timeout=120s")
            observer = f"{self.ns}-{self.prefix}-node-observer"
            self.k(
                "delete",
                f"clusterrolebinding/{observer}",
                f"clusterrole/{observer}",
                "--ignore-not-found",
            )
        return {
            "removed": self.name,
            "retained": [
                "workspace PVC",
                "native-state PVC",
                "database history",
                "shared configuration",
            ],
        }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "action",
        choices=[
            "build",
            "publish",
            "plan",
            "create",
            "register",
            "apply",
            "status",
            "update",
            "delete",
            "unregister",
            "runtime-publish",
        ],
    )
    parser.add_argument("file", type=Path, help="one AgentDeployment YAML")
    parser.add_argument("--image", help="platform runtime image tag; only used by runtime-publish")
    arguments = parser.parse_args()
    if arguments.action == "runtime-publish":
        print(json.dumps(publish_runtime(arguments.file, arguments.image), ensure_ascii=False, indent=2))
        return
    platform = Platform(arguments.file)
    if arguments.action == "build":
        response = platform.build()
    elif arguments.action == "publish":
        response = platform.publish()
    elif arguments.action == "plan":
        platform.prepare()
        response = {
            "agent": platform.name,
            "plan": str(platform.state),
            "submitted": False,
        }
    elif arguments.action in ("create", "register", "apply", "update"):
        if arguments.action == "update":
            raise RuntimeError("当前安全更新未完成；请保留原 Agent，不能直接替换其工作负载")
        response = platform.register()
    elif arguments.action == "status":
        response = platform.status()
    else:
        response = platform.unregister()
    print(json.dumps(response, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, ValueError, OSError, subprocess.SubprocessError) as error:
        raise SystemExit(str(error)) from None
