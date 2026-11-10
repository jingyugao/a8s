# Agentfile assembles AGENTS.md, Skills, CLI and native MCP configuration.
# deploy/agent.yaml declares an AgentDeployment and selects the platform-owned runtime class.
# The platform owns Codex, database/model configuration, storage and Kubernetes templates.
SHELL := /bin/bash
.SHELLFLAGS := -eo pipefail -c
.DEFAULT_GOAL := help
PROJECT := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
ROOT := $(abspath $(PROJECT)/../..)
AGENT := $(PROJECT)/deploy/agent.yaml
MANAGE := bash $(ROOT)/scripts/agent.sh

.PHONY: help build publish push plan create status update delete

help:
	@printf '%s\n' \
	  'make build    Build Agent content; the platform supplies Codex + Node.' \
	  'make publish  Build and publish the content image and metadata package.' \
	  'make create   Publish and create an AgentDeployment using fixed digests.' \
	  'make plan     Resolve published content and write a plan; do not apply it.' \
	  'make status   Inspect the AgentDeployment and its lifecycle operation.' \
	  'make update   Guarded entry point; safe replacement is not yet available.' \
	  'make delete   Active agents are retained; an offline request is saved.' \
	  '              Verified offline agents can release supporting services.' \
	  'Requires a configured platform, Docker, kubectl, and uv.'

build:
	$(MANAGE) build $(AGENT) --context $(PROJECT)

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
