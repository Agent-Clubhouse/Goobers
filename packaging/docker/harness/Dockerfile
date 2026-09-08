# One native harness and its bundled runtime/assets layered on a release base.
# GOOBERS_BASE_IMAGE must name the base built from this release's context.
ARG GOOBERS_BASE_IMAGE
ARG AZURE_LINUX_IMAGE=mcr.microsoft.com/azurelinux/base/core:3.0@sha256:daa1142fc6b44e27c8112ec6b4c2d579ddb9bc6b3747504e666010a45a51faa4
FROM ${AZURE_LINUX_IMAGE} AS harness-files
RUN tdnf install -y ca-certificates curl tar gzip coreutils findutils
ARG HARNESS
ARG TARGETARCH
COPY prepare.sh copilot.sh claude.sh /inputs/
RUN sh /inputs/prepare.sh "$HARNESS" "$TARGETARCH"

FROM ${GOOBERS_BASE_IMAGE}
COPY --from=harness-files /out/package/ /opt/goobers-harness/
COPY --from=harness-files /out/bin/ /usr/local/bin/
COPY --from=harness-files /out/version /out/source /usr/share/goobers/harness/
USER 65532:65532
ARG HARNESS
# The final user's actual command checks bundled native dependencies as well as
# the version. The wrappers keep package code immutable even on noexec HOME.
RUN /bin/sh -ec 'test "$(id -u)" = 65532; \
    expected=$(cat /usr/share/goobers/harness/version); \
    actual=$("$1" --version); first=$(printf "%s\n" "$actual" | head -n 1); \
    case "$1" in \
      copilot) test "$first" = "GitHub Copilot CLI $expected." ;; \
      claude) test "$actual" = "$expected (Claude Code)" ;; \
      *) exit 1 ;; \
    esac' -- "$HARNESS"
