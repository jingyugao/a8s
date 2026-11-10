.PHONY: build describe

build:
	cd ../.. && go build -trimpath -o dist/a8s-codex-agentc ./agentc/codex/cmd/agentc

# HOST is the pinned native Codex CLI, not a sandbox image.
describe: build
	../../dist/a8s-codex-agentc --describe --host "$(HOST)"
