#!/usr/bin/env bash
# Goobernetes Spike 0-W deploy — second, isolated instance on Windows nodes.
# SPIKE ARTIFACT.
#
# Required env:
#   IMAGE_WIN      windows image ref, e.g. <acr>.azurecr.io/goobers-win:spike
#   KEYVAULT_NAME  vault holding goobers-testbed-token + copilot-token
set -euo pipefail
: "${IMAGE_WIN:?}" "${KEYVAULT_NAME:?}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTANCE_DIR="$HERE/../instance-win"
NS=goobers-win
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

echo "==> namespace"
kubectl get ns "$NS" >/dev/null 2>&1 || kubectl create ns "$NS"

echo "==> secrets (values sourced from Key Vault, never from this repo)"
# Windows takes a plain Secret rather than the CSI driver — a deliberate spike
# simplification, see instance-win/instance.yaml. Origin is still Key Vault.
REPO_TOKEN="$(az keyvault secret show --vault-name "$KEYVAULT_NAME" -n goobers-testbed-token --query value -o tsv)"
COPILOT_TOKEN="$(az keyvault secret show --vault-name "$KEYVAULT_NAME" -n copilot-token --query value -o tsv)"
kubectl create secret generic goobers-secrets -n "$NS" \
  --from-literal=goobers-testbed-token="$REPO_TOKEN" \
  --from-literal=copilot-token="$COPILOT_TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f -
unset REPO_TOKEN COPILOT_TOKEN

echo "==> config seed"
tar czf "$TMP/config.tgz" -C "$INSTANCE_DIR" instance.yaml config
kubectl create configmap goobers-instance-config -n "$NS" \
  --from-file="config.tgz=$TMP/config.tgz" \
  --dry-run=client -o yaml | kubectl apply -f -

echo "==> manifests"
sed -e "s|\${IMAGE_WIN}|$IMAGE_WIN|g" "$HERE/goobers-win.yaml" | kubectl apply -f -
kubectl -n "$NS" rollout restart deployment/goobers-win >/dev/null 2>&1 || true

echo "==> waiting for rollout (Windows image pull on a cold node takes minutes)"
kubectl -n "$NS" rollout status deployment/goobers-win --timeout=25m

echo
echo "Logs:  kubectl -n $NS logs deploy/goobers-win -c daemon -f"
echo "Run:   kubectl -n $NS exec deploy/goobers-win -c daemon -- goobers.exe run default-implement C:\\instance"
