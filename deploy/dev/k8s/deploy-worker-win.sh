#!/usr/bin/env bash
# Goobernetes Spike 2 — deploy the Windows half of an OS-spanning run.
# SPIKE ARTIFACT.
#
# Seeds the LINUX instance's config onto a Windows node and runs one worker
# against "<queue>-windows". No daemon, no API, no claim ledger: this pod exists
# only to be the other end of a task queue.
#
# Required env:
#   IMAGE_WIN      windows image ref, e.g. <acr>.azurecr.io/goobers-win:spike
#   KEYVAULT_NAME  vault holding goobers-testbed-token + copilot-token
set -euo pipefail
: "${IMAGE_WIN:?}" "${KEYVAULT_NAME:?}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTANCE_DIR="$HERE/../instance"   # the LINUX instance, deliberately
NS=goobers
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

echo "==> secrets (from Key Vault; a plain Secret, since the CSI driver's"
echo "    Windows story is a separate question this spike does not need)"
REPO_TOKEN="$(az keyvault secret show --vault-name "$KEYVAULT_NAME" -n goobers-testbed-token --query value -o tsv)"
COPILOT_TOKEN="$(az keyvault secret show --vault-name "$KEYVAULT_NAME" -n copilot-token --query value -o tsv)"
kubectl create secret generic goobers-secrets-win -n "$NS" \
  --from-literal=goobers-testbed-token="$REPO_TOKEN" \
  --from-literal=copilot-token="$COPILOT_TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f -
unset REPO_TOKEN COPILOT_TOKEN

echo "==> config seed (byte-identical to the Linux instance's)"
tar czf "$TMP/config.tgz" -C "$INSTANCE_DIR" instance.yaml config
kubectl create configmap goobers-worker-win-config -n "$NS" \
  --from-file="config.tgz=$TMP/config.tgz" \
  --dry-run=client -o yaml | kubectl apply -f -

echo "==> manifests"
sed -e "s|\${IMAGE_WIN}|$IMAGE_WIN|g" "$HERE/worker-win.yaml" | kubectl apply -f -
kubectl -n "$NS" rollout restart deployment/goobers-worker-win >/dev/null 2>&1 || true

echo "==> waiting for rollout (cold Windows image pull takes minutes)"
kubectl -n "$NS" rollout status deployment/goobers-worker-win --timeout=25m

echo
echo "Logs: kubectl -n $NS logs deploy/goobers-worker-win -c worker -f"
