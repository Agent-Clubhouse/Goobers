#!/bin/sh
# Run once against the operator's selected cluster. Never replace a key under
# an existing version. Keep this Secret and all its versions with history backups.
set -eu
namespace=${1:-goobers-system}
secret=${2:-goobers-temporal-codec-key}
if kubectl get secret "$secret" -n "$namespace" >/dev/null 2>&1; then
  echo "Secret already exists; refusing to regenerate Temporal history key" >&2
  exit 1
fi
umask 077
key_dir=$(mktemp -d)
trap 'rm -rf "$key_dir"' EXIT
trap 'exit 1' HUP INT TERM
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "$key_dir/v1.pem"
printf '%s\n' v1 > "$key_dir/active"
# create (never apply) also refuses races or inaccessible existing Secrets.
kubectl create secret generic "$secret" -n "$namespace" \
  --from-file="active=$key_dir/active" --from-file="v1.pem=$key_dir/v1.pem"
