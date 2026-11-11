SHELL := /bin/bash
.SHELLFLAGS := -eo pipefail -c

# Go package roots. Listed explicitly so tmp/ is never built or tested.
PKGS := ./api/... ./artifacts/... ./control/... ./execution/... ./agentc/... \
	./providers/... ./storage/... ./infra/... ./sdk/... ./guest/... ./cmd/...

.PHONY: build test vet fmt gen check codex-runtime binaries image image-publish agentctl

IMAGE ?= registry.example.com/a8s:dev
RUNTIME_CLASS ?= codex-node
RUNTIME_CLASS_DIR := images/runtime-classes/$(RUNTIME_CLASS)

# Platform-owned harness distribution. Agent authors never supply Codex binaries.
codex-runtime:
	bash images/codex/install.sh

agentctl:
	mkdir -p dist
	go build -trimpath -o dist/agentctl ./cmd/agentctl

binaries: agentctl
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/a8s ./cmd/a8s
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/a8s-server ./cmd/a8s-server
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/a8s-codex-agentc ./agentc/codex/cmd/agentc
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/a8s-guest ./cmd/a8s-guest

image: binaries codex-runtime
	docker build --platform linux/amd64 -f $(RUNTIME_CLASS_DIR)/Dockerfile -t $(IMAGE) .

# Platform operators publish a runtime class once; agent builds consume its fixed digest.
image-publish: image
	bash scripts/agent.sh runtime-publish $(RUNTIME_CLASS_DIR)/runtime-class.json --image $(IMAGE)

build:
	go build $(PKGS)

test:
	go test $(PKGS)

vet:
	go vet $(PKGS)

fmt:
	gofmt -l -w $(subst /...,,$(subst ./,,$(PKGS)))

gen:
	buf lint
	buf generate

check: vet test
