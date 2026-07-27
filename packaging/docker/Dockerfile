# Goobers container image (docs/design/k8s-infra-shape.md §2).
#
# One image carries the goobers binary (daemon `up`, worker-style stage
# invocation via args) plus the tier-3 operator binary, so the reference
# manifests under deploy/reference/ can run every control-plane component
# from a single build. Build from the repo root:
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
ARG RUNTIME_IMAGE=docker.io/library/alpine:3.22

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
# git + CA bundle: the daemon manages real clones/worktrees and speaks HTTPS
# to the git/backlog provider; tzdata resolves instance-local schedule
# triggers.
RUN apk add --no-cache git ca-certificates tzdata
COPY --from=build /out/goobers /out/goobers-operator /usr/local/bin/
# 65532 mirrors the conventional distroless "nonroot" uid/gid so pod
# securityContexts (runAsNonRoot, runAsUser: 65532) hold across base images.
RUN addgroup -g 65532 nonroot \
    && adduser -D -H -u 65532 -G nonroot nonroot \
    && mkdir -p /instance \
    && chown nonroot:nonroot /instance
USER 65532:65532
WORKDIR /instance
ENTRYPOINT ["goobers"]
# Daemon by default. Override args for worker-style invocation (task-queue
# workers once v2-cloud-scale A1.6 lands), or set the container command to
# ["goobers-operator"] for the operator Deployment.
CMD ["up"]
