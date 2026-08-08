#!/usr/bin/env python3
"""Prometheus exporter for a Goobers daemon.

Scrapes the daemon's read API (health, runs, gaggles) and, optionally, the
project's Gitea backlog, and exposes them as Prometheus gauges. Designed to run
as a container with --network host so it can reach the daemon's loopback API,
into any Prometheus/Grafana + Home Assistant stack.

Env:
  GOOBERS_API   read API base, default http://127.0.0.1:8085/api/v1
  BACKLOG_API     issues/PRs API base — Gitea or GitHub-v3-compatible (optional)
  BACKLOG_REPO    owner/name (optional; enables backlog + open-PR metrics)
  BACKLOG_TOKEN   read token for issues/PRs (optional)
  PORT          metrics port, default 9779
  INTERVAL      scrape interval seconds, default 30
"""
import os
import time
from collections import Counter
from datetime import datetime, timezone

import requests
from prometheus_client import start_http_server, Gauge

GOOBERS_API = os.environ.get("GOOBERS_API", "http://127.0.0.1:8085/api/v1")
BACKLOG_API = os.environ.get("BACKLOG_API", os.environ.get("GITEA_API", ""))
BACKLOG_REPO = os.environ.get("BACKLOG_REPO", os.environ.get("GITEA_REPO", ""))
BACKLOG_TOKEN = os.environ.get("BACKLOG_TOKEN", os.environ.get("GITEA_TOKEN", ""))
PORT = int(os.environ.get("PORT", "9779"))
INTERVAL = int(os.environ.get("INTERVAL", "30"))

up = Gauge("goobers_up", "Daemon read API reachable")
healthy = Gauge("goobers_healthy", "Daemon reports healthy")
ready = Gauge("goobers_ready", "Daemon reports ready")
tick_age = Gauge("goobers_scheduler_last_tick_age_seconds", "Age of last scheduler tick (dead-man switch)")
journal_age = Gauge("goobers_journal_age_seconds", "Age of last journal update")
degraded = Gauge("goobers_degraded_subsystems", "Count of degraded read-model subsystems")
runs_active = Gauge("goobers_runs_active", "Active (non-terminal) runs")
runs_phase = Gauge("goobers_runs_phase", "Runs by phase (recent window)", ["phase"])
wf_active = Gauge("goobers_workflow_active_runs", "Active runs per workflow", ["workflow"])
gaggles_total = Gauge("goobers_gaggles_total", "Configured gaggles")
backlog = Gauge("goobers_backlog_issues", "Open issues by goobers status", ["status"])
open_prs = Gauge("goobers_open_prs", "Open pull requests on the project repo")


def _age_seconds(ts, now):
    if not ts:
        return 0.0
    try:
        t = datetime.fromisoformat(ts.replace("Z", "+00:00"))
        return max(0.0, (now - t).total_seconds())
    except Exception:
        return 0.0


def poll():
    now = datetime.now(timezone.utc)
    # --- health ---
    try:
        h = requests.get(f"{GOOBERS_API}/health", timeout=8).json()
        up.set(1)
        healthy.set(1 if h.get("healthy") else 0)
        ready.set(1 if h.get("ready") else 0)
        fr = h.get("freshness", {}) or {}
        tick_age.set(float(fr.get("lastTickAgeMillis", 0)) / 1000.0)
        journal_age.set(_age_seconds(fr.get("journalUpdatedAt"), now))
        degraded.set(len((h.get("readState", {}) or {}).get("degraded", []) or []))
    except Exception:
        up.set(0)
        healthy.set(0)
        ready.set(0)
    # --- runs ---
    try:
        runs = requests.get(f"{GOOBERS_API}/runs?limit=200", timeout=8).json().get("runs", [])
        active = [r for r in runs if not r.get("terminal")]
        runs_active.set(len(active))
        pc = Counter(r.get("phase") for r in runs)
        for ph in ("running", "completed", "failed", "escalated", "aborted"):
            runs_phase.labels(phase=ph).set(pc.get(ph, 0))
        wf_active.clear()
        for wf, c in Counter(r.get("workflow") for r in active).items():
            if wf:
                wf_active.labels(workflow=wf).set(c)
    except Exception:
        pass
    # --- gaggles ---
    try:
        items = requests.get(f"{GOOBERS_API}/gaggles", timeout=8).json().get("items", [])
        gaggles_total.set(len(items))
    except Exception:
        pass
    # --- gitea backlog (optional) ---
    if BACKLOG_API and BACKLOG_REPO and BACKLOG_TOKEN:
        hdr = {"Authorization": "token " + BACKLOG_TOKEN}
        try:
            iss = requests.get(
                f"{BACKLOG_API}/repos/{BACKLOG_REPO}/issues?state=open&type=issues&limit=100",
                headers=hdr, timeout=8).json()
            for name, label in (("approved", "approved"), ("claimed", "goobers:claimed"),
                                ("in_review", "goobers/status:in-review"), ("needs_human", "goobers:needs-human")):
                backlog.labels(status=name).set(
                    sum(1 for i in iss if any(l["name"] == label for l in i.get("labels", []))))
            prs = requests.get(f"{BACKLOG_API}/repos/{BACKLOG_REPO}/pulls?state=open&limit=100",
                               headers=hdr, timeout=8).json()
            open_prs.set(len(prs))
        except Exception:
            pass


if __name__ == "__main__":
    start_http_server(PORT)
    while True:
        poll()
        time.sleep(INTERVAL)
