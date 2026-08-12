#!/usr/bin/env bash
# Capture everything worth keeping from a live spike cluster BEFORE teardown.
# SPIKE ARTIFACT. The first end-to-end cloud run at each rung is history — the
# cluster is disposable, the evidence is not.
#
# Usage:  ./deploy/dev/k8s/exfil.sh <rung-label> [dest-root]
#   e.g.  ./deploy/dev/k8s/exfil.sh spike-0
#
# Default dest-root is the operator journal, deliberately outside the product repo.
set -euo pipefail

RUNG="${1:?usage: exfil.sh <rung-label> [dest-root]}"
DEST_ROOT="${2:-$HOME/source/Goobers-Review/Goobernetes-Spike/runs}"
NS="${NS:-goobers}"
DEST="$DEST_ROOT/$RUNG"
STAMP="$(date -u +%Y-%m-%dT%H-%M-%SZ)"

mkdir -p "$DEST"
echo "==> exfil '$RUNG' -> $DEST  (captured $STAMP)"

POD="$(kubectl -n "$NS" get pod -l app=goobers -o jsonpath='{.items[0].metadata.name}')"
echo "    pod: $POD"

# --- provenance -------------------------------------------------------------
{
  echo "# Goobernetes exfil — $RUNG"
  echo "captured_utc:   $STAMP"
  echo "cluster:        $(kubectl config current-context)"
  echo "namespace:      $NS"
  echo "pod:            $POD"
  echo "node:           $(kubectl -n "$NS" get pod "$POD" -o jsonpath='{.spec.nodeName}')"
  echo "node_os:        $(kubectl get node "$(kubectl -n "$NS" get pod "$POD" -o jsonpath='{.spec.nodeName}')" -o jsonpath='{.status.nodeInfo.osImage}')"
  echo "k8s_version:    $(kubectl version -o json 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)["serverVersion"]["gitVersion"])' 2>/dev/null || echo unknown)"
  echo "image:          $(kubectl -n "$NS" get pod "$POD" -o jsonpath='{.spec.containers[0].image}')"
  echo "image_digest:   $(kubectl -n "$NS" get pod "$POD" -o jsonpath='{.status.containerStatuses[0].imageID}')"
  echo "goobers_version: $(kubectl -n "$NS" exec "$POD" -c daemon -- goobers --version 2>/dev/null || echo unavailable)"
  echo "pod_started:    $(kubectl -n "$NS" get pod "$POD" -o jsonpath='{.status.startTime}')"
} > "$DEST/PROVENANCE.txt"
cat "$DEST/PROVENANCE.txt"

# --- the journals (the actual prize) ---------------------------------------
# Streamed as a tar from inside the pod rather than `kubectl cp`, which is
# unreliable for trees. SQLite DBs are excluded: they are large, rebuildable
# projections, and not the record of what happened. The journal is.
echo "==> run journals + instance log"
kubectl -n "$NS" exec "$POD" -c daemon -- \
  tar czf - -C /instance \
    --exclude='*.db' --exclude='*.db-wal' --exclude='*.db-shm' \
    --exclude='workcopies' \
    gaggles scheduler 2>/dev/null > "$DEST/instance-journal.tgz" || {
      echo "    (tar returned nonzero — partial capture kept)"; }
ls -lh "$DEST/instance-journal.tgz"

# Unpack a browsable copy alongside the archive.
rm -rf "$DEST/instance"
mkdir -p "$DEST/instance"
tar xzf "$DEST/instance-journal.tgz" -C "$DEST/instance" 2>/dev/null || true
echo "    runs captured:"
find "$DEST/instance" -type d -name 'runs' -exec sh -c 'ls -1 "$1" 2>/dev/null | sed "s|^|      |"' _ {} \; 2>/dev/null | head -20

# --- logs and cluster state -------------------------------------------------
echo "==> logs"
for c in daemon dashboard seed; do
  kubectl -n "$NS" logs "$POD" -c "$c" --timestamps > "$DEST/log-$c.txt" 2>/dev/null \
    && echo "    log-$c.txt ($(wc -l < "$DEST/log-$c.txt" | tr -d ' ') lines)" || true
  kubectl -n "$NS" logs "$POD" -c "$c" --timestamps --previous > "$DEST/log-$c-previous.txt" 2>/dev/null || rm -f "$DEST/log-$c-previous.txt"
done

kubectl -n "$NS" describe pod "$POD"           > "$DEST/describe-pod.txt" 2>&1 || true
kubectl -n "$NS" get events --sort-by=.lastTimestamp > "$DEST/events.txt" 2>&1 || true
kubectl -n "$NS" get all,pvc,secret,secretproviderclass -o wide > "$DEST/cluster-state.txt" 2>&1 || true

echo
echo "==> done. $DEST"
du -sh "$DEST"
