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
# or `make image`. The Portal is built into a reusable static asset artifact
# and embedded in the goobers binary; CGO is off (modernc.org/sqlite is pure
# Go) to match the release/ cross-compile settings — the binaries are static.
#
# Publishing this image from CI on tagged releases is a follow-up; nothing in
# .github/workflows builds or pushes it yet.

# GO_IMAGE is pinned to the exact patch version go.mod declares, never a
# floating minor: a floating tag builds the shipped image with whatever 1.26.x
# the registry serves that day, with no commit recording the change (#3452).
# Bump trigger: a change to the `go` directive in go.mod — never a calendar.
# `go run ./test/gotoolchain` (the go-toolchain merge-gate check) fails while
# the two disagree, so the bump has to land in the same diff.
ARG GO_IMAGE=docker.io/library/golang:1.26.6
ARG PORTAL_NODE_IMAGE=docker.io/library/node:24-bookworm-slim
ARG RUNTIME_IMAGE=docker.io/library/node:22-bookworm-slim
ARG COPILOT_VERSION=1.0.80

FROM ${PORTAL_NODE_IMAGE} AS portal-build
WORKDIR /src
COPY portal/package.json portal/package-lock.json portal/
RUN npm --prefix portal ci --no-audit --no-fund
COPY portal portal
COPY internal/portalassets internal/portalassets
RUN npm --prefix portal run build

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=portal-build /src/internal/portalassets/dist ./internal/portalassets/dist
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
ENV CGO_ENABLED=0
RUN go build -tags embed_portal -trimpath \
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
