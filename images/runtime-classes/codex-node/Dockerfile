ARG BASE_IMAGE=debian@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8
FROM node@sha256:752ea8a2f758c34002a0461bd9f1cee4f9a3c36d48494586f60ffce1fc708e0e AS node
FROM ${BASE_IMAGE}
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates bash jq skopeo umoci util-linux \
    && rm -rf /var/lib/apt/lists/*
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules/npm/ /usr/local/lib/node_modules/npm/
COPY dist/a8s dist/a8s-server dist/a8s-codex-agentc /usr/local/bin/
COPY --chmod=755 scripts/agent-loader.sh /usr/local/bin/a8s-agent-loader
COPY dist/codex/ /opt/a8s/codex/
RUN ln -s /opt/a8s/codex/bin/codex /usr/local/bin/codex \
    && ln -s /opt/a8s/codex/bin/codex-code-mode-host /usr/local/bin/codex-code-mode-host \
    && ln -s /opt/a8s/codex/bin/codex-exec-server /usr/local/bin/codex-exec-server \
    && ln -s /usr/local/lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
    && ln -s /usr/local/lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
    && mkdir -p /home/agent/.codex
LABEL a8s.io.runtime-class="codex-node"
ENV HOME=/home/agent LANG=C.UTF-8 CODEX_HOME=/home/agent/.codex \
    PATH=/opt/a8s/codex/codex-path:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
WORKDIR /runtime
EXPOSE 8090 8094 8095
ENTRYPOINT ["/usr/local/bin/codex-exec-server", "--listen", "ws://0.0.0.0:8095"]
