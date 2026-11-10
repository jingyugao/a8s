# Runtime dependencies. The platform image already supplies original Codex and a8s.
ARG PLATFORM_IMAGE
FROM node@sha256:752ea8a2f758c34002a0461bd9f1cee4f9a3c36d48494586f60ffce1fc708e0e AS node
FROM ${PLATFORM_IMAGE}
COPY --from=node /usr/local/bin/node /usr/local/bin/node
