#!/bin/sh
# The native loader otherwise extracts executable addons into HOME. Keep these
# in the immutable image so a noexec ephemeral HOME remains usable. Set these
# here because a stage's default-deny environment removes ambient image env.
COPILOT_AUTO_UPDATE=false COPILOT_CLI_DIST_DIR=/opt/goobers-harness \
  exec /opt/goobers-harness/copilot "$@"
