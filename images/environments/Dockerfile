ARG BASE_IMAGE=debian@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8
FROM ${BASE_IMAGE}
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates bash \
    && rm -rf /var/lib/apt/lists/*
COPY dist/a8s dist/a8s-server dist/a8s-codex-agentc /usr/local/bin/
COPY dist/codex/ /opt/a8s/codex/
RUN ln -s /opt/a8s/codex/bin/codex /usr/local/bin/codex \
    && ln -s /opt/a8s/codex/bin/codex-code-mode-host /usr/local/bin/codex-code-mode-host \
    && ln -s /opt/a8s/codex/bin/codex-exec-server /usr/local/bin/codex-exec-server
ENV HOME=/home/agent LANG=C.UTF-8 CODEX_HOME=/tmp/a8s-exec-config \
    PATH=/opt/a8s/codex/codex-path:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
WORKDIR /home/agent
EXPOSE 8090 8094 8095
ENTRYPOINT ["/usr/local/bin/codex-exec-server", "--listen", "ws://0.0.0.0:8095"]
