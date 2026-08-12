# Goobernetes spike image (#2839) — SPIKE ARTIFACT, not a shipped image.
#
# packaging/docker/Dockerfile builds the shipped image: goobers + operator on
# alpine with git/ca-certificates/tzdata. That image can run DETERMINISTIC
# stages only — it carries no agent harness, so every agentic stage fails at
# the harness preflight. This image adds what an agentic stage actually needs.
#
# Deliberate differences from the shipped image, each one a finding for #2839:
#   1. Debian (node:22-slim) rather than alpine. The Copilot CLI is an npm
#      package with native deps; musl is an avoidable variable on the rung
#      whose known landmine IS the agent CLI. If a later rung proves alpine
#      works, fold this back.
#   2. Node 22 + @github/copilot + gh, none of which the shipped image has.
#   3. Same uid/gid (65532) and /instance workdir as the shipped image, so the
#      reference manifests' securityContext still holds.
#
# Build from the repo root:
#   docker build -f deploy/dev/image/Dockerfile -t goobers:spike .

ARG GO_IMAGE=docker.io/library/golang:1.26
ARG RUNTIME_IMAGE=docker.io/library/node:22-slim

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=spike
ARG COMMIT=none
ARG DATE=unknown
ENV CGO_ENABLED=0
RUN go build -trimpath \
        -ldflags "-s -w \
            -X github.com/goobers/goobers/internal/version.Version=${VERSION} \
            -X github.com/goobers/goobers/internal/version.Commit=${COMMIT} \
            -X github.com/goobers/goobers/internal/version.Date=${DATE}" \
        -o /out/goobers ./cmd/goobers

FROM ${RUNTIME_IMAGE}

# git: real clones/worktrees. ca-certificates: HTTPS to the provider and the
# model endpoint. tzdata: instance-local cron trigger resolution. curl/gnupg:
# only to add the gh apt repo, removed after.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        git ca-certificates tzdata curl gnupg \
    && mkdir -p -m 755 /etc/apt/keyrings \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
        | gpg --dearmor -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
        > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends gh \
    && apt-get purge -y gnupg \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*

# The agent harness. Pinned by tag so a rebuild is reproducible; bump
# deliberately, and record the version in the rung's exit notes.
ARG COPILOT_CLI_VERSION=latest
RUN npm install -g @github/copilot@${COPILOT_CLI_VERSION} \
    && npm cache clean --force

COPY --from=build /out/goobers /usr/local/bin/goobers

# 65532 mirrors the shipped image so pod securityContexts are interchangeable.
# HOME must be writable and owned: the Copilot CLI keeps state under
# $HOME/.copilot, and a read-only or root-owned HOME is a known headless
# failure mode.
RUN groupadd -g 65532 nonroot \
    && useradd -u 65532 -g 65532 -M -d /home/nonroot -s /usr/sbin/nologin nonroot \
    && mkdir -p /home/nonroot /instance \
    && chown -R 65532:65532 /home/nonroot /instance
ENV HOME=/home/nonroot
USER 65532:65532
WORKDIR /instance
ENTRYPOINT ["goobers"]
CMD ["up"]
