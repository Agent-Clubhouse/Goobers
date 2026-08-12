#!/usr/bin/env bash
# Goobernetes Spike 0 (#2839) deploy. SPIKE ARTIFACT — not a shipped deploy path.
#
# Instance-specific values come from the environment so they never land in this
# repo. Required:
#   AZURE_CLIENT_ID   user-assigned managed identity clientId (workload identity)
#   AZURE_TENANT_ID   Entra tenant
#   KEYVAULT_NAME     Key Vault holding goobers-testbed-token + copilot-token
#   IMAGE             fully-qualified image ref, e.g. <acr>.azurecr.io/goobers:spike
#
# Usage:  ./deploy/dev/k8s/deploy.sh
set -euo pipefail

: "${AZURE_CLIENT_ID:?}" "${AZURE_TENANT_ID:?}" "${KEYVAULT_NAME:?}" "${IMAGE:?}"

# The build commit the deployed image was built from. service.version carries the
# VERSION build-arg ("spike"), which never changes, so telemetry cannot otherwise
# tell two binaries apart.
GOOBERS_COMMIT="${GOOBERS_COMMIT:-$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --short HEAD 2>/dev/null || echo unknown)}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTANCE_DIR="$HERE/../instance"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "==> namespace"
kubectl get ns goobers >/dev/null 2>&1 || kubectl create ns goobers

echo "==> packaging instance config"
# Archive the instance root (instance.yaml + config/) as the seed payload.
tar czf "$TMP/config.tgz" -C "$INSTANCE_DIR" instance.yaml config
ls -l "$TMP/config.tgz"

echo "==> config seed ConfigMap"
kubectl create configmap goobers-instance-config -n goobers \
  --from-file="config.tgz=$TMP/config.tgz" \
  --dry-run=client -o yaml | kubectl apply -f -

echo "==> manifests"
sed -e "s|\${AZURE_CLIENT_ID}|$AZURE_CLIENT_ID|g" \
    -e "s|\${AZURE_TENANT_ID}|$AZURE_TENANT_ID|g" \
    -e "s|\${KEYVAULT_NAME}|$KEYVAULT_NAME|g" \
    -e "s|\${IMAGE}|$IMAGE|g" \
    -e "s|\${GOOBERS_COMMIT}|$GOOBERS_COMMIT|g" \
    "$HERE/goobers.yaml" | kubectl apply -f -

# Force a new pod so a re-run always picks up fresh config, even when only the
# ConfigMap changed (Deployment spec is unchanged in that case).
kubectl -n goobers rollout restart deployment/goobers >/dev/null 2>&1 || true

echo "==> waiting for rollout"
kubectl -n goobers rollout status deployment/goobers --timeout=5m

echo
echo "Portal:  kubectl -n goobers port-forward deploy/goobers 8081:8081  -> http://127.0.0.1:8081"
echo "API:     kubectl -n goobers port-forward deploy/goobers 8080:8080"
echo "Logs:    kubectl -n goobers logs deploy/goobers -c daemon -f"
