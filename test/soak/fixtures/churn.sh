#!/bin/sh
# Deliberately small real work, not a resource injector or recursive CI job.
set -eu
# Keep user/global hooks, signing, templates, and filters out of this fixture.
unset GIT_CONFIG_PARAMETERS GIT_CONFIG_COUNT GIT_TEMPLATE_DIR
GIT_CONFIG_NOSYSTEM=1
GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_NOSYSTEM GIT_CONFIG_GLOBAL
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT HUP INT TERM
git -c init.templateDir= -C "$scratch" init -q
i=0
while [ "$i" -lt 24 ]; do
  printf 'fixture file %s\n' "$i" > "$scratch/file-$i"
  (exec 3< "$scratch/file-$i"; cat <&3 > /dev/null)
  i=$((i + 1))
done
git -C "$scratch" add .
git -C "$scratch" -c user.name=soak -c user.email=soak@invalid commit -qm fixture
git -C "$scratch" fsck --no-reflogs > /dev/null
sleep 3
if [ "${1:-success}" = failure ]; then
  printf '%s\n' '{"errorCode":"soak_fixture_failure","errorMessage":"soak_fixture_failure","errorRetryable":false,"integrity":"unapproved"}' > result.json
  exit 1
fi
printf '%s\n' '{"fixture":"complete","integrity":"unapproved"}' > result.json
