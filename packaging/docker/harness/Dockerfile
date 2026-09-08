# Prepared by the release engine's imageinputs helper: one verified native
# harness package and fixed runtime launcher, with no build scripts or downloads.
ARG GOOBERS_BASE_IMAGE
FROM ${GOOBERS_BASE_IMAGE}
COPY package/ /opt/goobers-harness/
COPY bin/ /usr/local/bin/
COPY version source /usr/share/goobers/harness/
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
