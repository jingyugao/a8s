#!/usr/bin/env bash
# Platform bootstrap: pull an immutable content image, copy /agent, then start the native harness.
set -euo pipefail
image=${A8S_AGENT_IMAGE:-}
name=${A8S_AGENT_NAME:-agent}
destination=/agent
layout=/opt/a8s/package
state=${A8S_NATIVE_STATE:-/runtime/.a8s/native}
mode=app-server
plain_http=${A8S_REGISTRY_PLAIN_HTTP:-false}
while (($#)); do
  case "$1" in
    --image) image=$2; shift 2 ;;
    --name) name=$2; shift 2 ;;
    --destination) destination=$2; shift 2 ;;
    --package-layout) layout=$2; shift 2 ;;
    --state-dir) state=$2; shift 2 ;;
    --extract-only) mode=extract; shift ;;
    --agentc) mode=agentc; shift ;;
    --plain-http) plain_http=true; shift ;;
    *) printf 'Unknown loader argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done
[[ $image =~ ^[^[:space:]@]+@sha256:[a-f0-9]{64}$ ]] || { echo 'An immutable Agent image reference is required' >&2; exit 2; }
[[ $plain_http == true || $plain_http == false ]] || exit 2
[[ $destination == /* && $layout == /* && $state == /* ]] || exit 2
[[ ! -e $destination && ! -L $destination && ! -e $layout ]] || { echo 'Loader destination already exists' >&2; exit 1; }
stage=$(mktemp -d)
children=()
cleanup() {
  trap - EXIT INT TERM
  for pid in "${children[@]}"; do kill -TERM "$pid" 2>/dev/null || true; done
  if ((${#children[@]})); then
    for ((attempt=0; attempt<50; attempt++)); do
      alive=false
      for pid in "${children[@]}"; do kill -0 "$pid" 2>/dev/null && alive=true; done
      [[ $alive == false ]] && break
      sleep 0.2
    done
    for pid in "${children[@]}"; do kill -KILL "$pid" 2>/dev/null || true; done
    wait "${children[@]}" 2>/dev/null || true
  fi
  rm -rf -- "$stage"
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
tls_verify=true
[[ $plain_http == true ]] && tls_verify=false
# OCI image layers are verified and flattened by standard OCI tooling. No image command is run.
skopeo --command-timeout 120s copy --retry-times 0 --src-tls-verify="$tls_verify" \
  --override-os linux --override-arch "$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')" \
  "docker://$image" "oci:$stage/image:agent" >&2
timeout 120s umoci unpack --rootless --image "$stage/image:agent" "$stage/bundle" >&2
payload="$stage/bundle/rootfs/agent"
[[ -d $payload && ! -L $payload && -f $payload/AGENTS.md && ! -L $payload/AGENTS.md ]] || {
  echo 'Agent image must contain /agent/AGENTS.md' >&2; exit 1;
}
mkdir -p -- "$(dirname -- "$destination")" "$(dirname -- "$layout")"
cp -a --no-preserve=ownership,xattr -- "$payload" "$destination"
find "$destination" -type f -exec chmod a-s {} +
# Control keeps its existing immutable package metadata; CLI and MCP stay in the content image.
a8s --json package init --source "$destination" --name "$name" >&2
a8s --json package build --source "$destination" --output "$layout" >&2
actual_digest=$(a8s --json package inspect --layout "$layout" | jq -er .digest)
if [[ -n ${A8S_PACKAGE_DIGEST:-} && $actual_digest != "$A8S_PACKAGE_DIGEST" ]]; then
  echo 'Agent content differs from the registered package digest' >&2; exit 1
fi
if [[ $mode == extract ]]; then
  jq -n --arg image "$image" --arg digest "$actual_digest" '{image:$image,packageDigest:$digest}'
  exit 0
fi
mkdir -p -- "$state"
exec 9>"$state/loader.lock"
flock -n 9 || { echo 'The native state is already owned by another loader' >&2; exit 1; }
export AGENT_ROOT="$destination"
export PATH="$destination/node_modules/.bin:$PATH"
if [[ $mode == agentc ]]; then
  /usr/local/bin/codex-exec-server --listen ws://127.0.0.1:8095 &
  children+=("$!")
  /usr/local/bin/a8s-codex-agentc --listen=0.0.0.0:8094 --host=/usr/local/bin/codex \
    --runtime-dir="$state" --package-layout="$layout" \
    --pass-env=PATH,HOME,LANG,TZ,AGENT_ROOT,COLONY_MODEL_API_KEY &
  children+=("$!")
  set +e
  wait -n "${children[@]}"
  result=$?
  set -e
  exit "$result"
fi
export CODEX_HOME="$state/codex"
mkdir -p "$CODEX_HOME"
cp -- "$destination/AGENTS.md" "$CODEX_HOME/AGENTS.md"
if [[ -d $destination/.agents/skills ]]; then
  ln -s -- "$destination/.agents/skills" "$CODEX_HOME/skills"
fi
if [[ -f $destination/.codex/config.toml ]]; then
  cp -- "$destination/.codex/config.toml" "$CODEX_HOME/config.toml"
fi
printf '\nallow_login_shell = false\n' > "$CODEX_HOME/config.toml.tmp"
cat "$CODEX_HOME/config.toml" >> "$CODEX_HOME/config.toml.tmp" 2>/dev/null || true
mv -- "$CODEX_HOME/config.toml.tmp" "$CODEX_HOME/config.toml"
/usr/local/bin/codex app-server --listen stdio:// <&0 &
children+=("$!")
wait "${children[0]}"
