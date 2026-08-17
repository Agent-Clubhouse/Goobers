# Goobers container image (docs/design/k8s-infra-shape.md §2).
#
# One image carries the goobers binary (daemon `up`, worker-style stage
# invocation via args), the default Copilot agent harness, and the tier-3
# operator binary, so the reference manifests under deploy/reference/ can run
# every control-plane component and stage kind from a single build. Build from
# the repo root:
#
#   docker build -f packaging/docker/Dockerfile -t <registry>/goobers:<tag> .
#
# or `make image`. The portal assets are go:embed'd from the committed
# cmd/goobers/portal-dist tree, so no Node toolchain is needed here, and CGO
# is off (modernc.org/sqlite is pure Go) to match the release/ cross-compile
# settings — the binaries are static.
#
# Publishing this image from CI on tagged releases is a follow-up; nothing in
# .github/workflows builds or pushes it yet.

ARG GO_IMAGE=docker.io/library/golang:1.26
ARG RUNTIME_IMAGE=docker.io/library/node:22-bookworm-slim
ARG COPILOT_VERSION=1.0.80

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
ENV CGO_ENABLED=0
RUN go build -trimpath \
        -ldflags "-s -w \
            -X github.com/goobers/goobers/internal/version.Version=${VERSION} \
            -X github.com/goobers/goobers/internal/version.Commit=${COMMIT} \
            -X github.com/goobers/goobers/internal/version.Date=${DATE}" \
        -o /out/goobers ./cmd/goobers \
    && go build -trimpath -ldflags "-s -w" -o /out/goobers-operator ./cmd/operator

FROM ${RUNTIME_IMAGE}
# git + gh + CA bundle support repository/provider operations; tzdata resolves
# instance-local schedule triggers. Copilot is the default agent harness.
ARG COPILOT_VERSION
RUN apt-get update \
    && apt-get install --no-install-recommends -y ca-certificates gh git tzdata \
    && npm install --global --omit=dev "@github/copilot@${COPILOT_VERSION}" \
    && git --version \
    && gh --version \
    && node --version \
    && copilot --version \
    && npm cache clean --force \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/goobers /out/goobers-operator /usr/local/bin/
# 65532 mirrors the conventional distroless "nonroot" uid/gid so pod
# securityContexts (runAsNonRoot, runAsUser: 65532) hold across base images.
RUN groupadd --gid 65532 nonroot \
    && useradd --uid 65532 --gid nonroot --home-dir /home/nonroot --create-home nonroot \
    && mkdir -p /instance \
    && chown nonroot:nonroot /instance
ENV HOME=/home/nonroot
USER 65532:65532
WORKDIR /instance
ENTRYPOINT ["goobers"]
# Daemon by default. Override args to host a Temporal task-queue worker, or set
# the container command to ["goobers-operator"] for the operator Deployment.
CMD ["up"]
