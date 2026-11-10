# Agentfile assembles AGENTS.md, Skills, CLI and native MCP configuration.
# Dockerfile supplies runtime dependencies; agent.yaml registers the agent.
# The platform owns Codex, database/model configuration, storage and Kubernetes templates.
SHELL := /bin/bash
.SHELLFLAGS := -eo pipefail -c
.DEFAULT_GOAL := help
ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/../..)
AGENT := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/agent.yaml)
MANAGE := bash $(ROOT)/scripts/agent.sh

.PHONY: help build publish push plan create status update delete

help:
	@printf '%s\n' \
	  'make build    构建 Agent 镜像；Codex 继承自平台内置镜像' \
	  'make publish  构建并发布镜像与 Agent 包，登记时固定 digest' \
	  'make create   发布并创建 Agent；相同内容可重复核对' \
	  'make plan     查看已发布镜像的创建计划，不修改集群' \
	  'make status   查询原 Deployment 和 Operation' \
	  'make update   更新入口；当前安全更新尚未完成，会明确拒绝' \
	  'make delete   删除入口；运行中 Agent 会保存下线请求并明确拒绝' \
	  '             已确认下线或尚未创建 Agent 时，可清理专属支撑服务，保留数据' \
	  '管理端需要 Docker、kubectl、uv；不需要本机安装 Codex 或 Go。'

build:
	$(MANAGE) build $(AGENT)

publish: build
	$(MANAGE) publish $(AGENT)

push: publish

plan:
	$(MANAGE) plan $(AGENT)

create: publish
	$(MANAGE) create $(AGENT)

status:
	$(MANAGE) status $(AGENT)

update:
	$(MANAGE) update $(AGENT)

delete:
	$(MANAGE) delete $(AGENT)
