# Monitoring: Prometheus + Grafana

Surface a Goobers daemon's health and workforce activity in Prometheus and a
ready-made Grafana dashboard.

The daemon does not expose a `/metrics` endpoint. Instead, a small **exporter**
scrapes the daemon's read API (`/health`, `/runs`, `/gaggles`) plus — optionally
— the project's backlog, and re-publishes them as Prometheus gauges. Everything
downstream is standard Prometheus.

```
 goobers daemon
 read API :8085 ──▶ exporter :9779 ──▶ Prometheus ──▶ Grafana (grafana-dashboard.json)
 + backlog (opt)     (this dir)
```

It is **read-only telemetry** — nothing here can change the workforce.

## Metrics

| Metric | Meaning |
|---|---|
| `goobers_up` | daemon read API reachable |
| `goobers_healthy` / `goobers_ready` | daemon health / ready booleans |
| `goobers_scheduler_last_tick_age_seconds` | dead-man switch — grows if the scheduler stalls |
| `goobers_journal_age_seconds` | staleness of the run journal |
| `goobers_degraded_subsystems` | count of degraded read-model subsystems |
| `goobers_runs_active` | active (non-terminal) runs |
| `goobers_runs_phase{phase}` | runs by phase (running/completed/failed/escalated/aborted) |
| `goobers_workflow_active_runs{workflow}` | active runs per workflow (lane) |
| `goobers_gaggles_total` | configured gaggles |
| `goobers_backlog_issues{status}` | open issues by status (approved/claimed/in_review/needs_human) |
| `goobers_open_prs` | open PRs on the project repo |

The backlog metrics (`goobers_backlog_issues`, `goobers_open_prs`) require the
optional backlog vars below; without them the exporter runs daemon-only.

## Setup

### 1. Run the exporter

Build and run it next to the daemon. `--network host` lets it reach the daemon's
loopback read API:

```sh
docker build -t goobers-exporter deploy/monitoring  # base image + deps are pinned; see Dependency management
docker run -d --name goobers-exporter --network host \
  -e GOOBERS_API=http://127.0.0.1:8085/api/v1 \
  goobers-exporter
# metrics now on http://127.0.0.1:9779/metrics
```

To include the backlog funnel + open-PR count, add your provider's API (the
example is Gitea; the same shape works for a GitHub API base + `owner/repo`):

```sh
  -e BACKLOG_API=http://your-forge:3000/api/v1 \
  -e BACKLOG_REPO=owner/repo \
  -e BACKLOG_TOKEN=<read-only token for issues/PRs> \
```

### 2. Scrape it from Prometheus

```yaml
# prometheus.yml
scrape_configs:
  - job_name: goobers
    static_configs:
      - targets: ["EXPORTER_HOST:9779"]
```

### 3. Import the Grafana dashboard

`grafana-dashboard.json` is a self-contained board: fleet health, the dead-man
gauge, active runs, the needs-human count, open PRs, runs-by-phase, per-lane
activity, and the backlog funnel.

**Dashboards → New → Import → Upload JSON file**, then choose your Prometheus
datasource when prompted (the board references it as the `${DS_PROMETHEUS}`
variable, so nothing is hard-coded).

## Configuration reference (exporter)

| Var | Default | Purpose |
|---|---|---|
| `GOOBERS_API` | `http://127.0.0.1:8085/api/v1` | Daemon read API base |
| `BACKLOG_API` | *(unset)* | Provider API base — enables backlog metrics with the two below |
| `BACKLOG_REPO` | *(unset)* | `owner/name` of the project repo |
| `BACKLOG_TOKEN` | *(unset)* | Read token for issues/PRs |
| `PORT` | `9779` | Port the `/metrics` endpoint listens on |
| `INTERVAL` | `30` | Seconds between daemon scrapes |

## Files

| File | What |
|---|---|
| `exporter.py` | the exporter (Python, `prometheus_client` + `requests`) |
| `Dockerfile` | builds the exporter image |
| `requirements.txt` | hash-locked dependency closure (see below) |
| `grafana-dashboard.json` | importable Grafana dashboard |

## Dependency management

This image is part of a shipped deployment, so what goes into it is pinned
twice over (#4569) — once for the base image, once for the Python packages.

**Base image — pinned by digest.** The `PYTHON_IMAGE` build arg names
`python:3.12-slim` *and* the sha256 of the multi-arch image index it resolved
to. A bare `python:3.12-slim` tag moves with every CPython patch release and
every Debian security rebuild, so two builds a week apart shipped different
interpreters and different OS packages with nothing recording the change.

**Python packages — pinned by version and hash.** `requirements.txt` is a
complete transitive closure: the two direct dependencies (`prometheus_client`,
`requests`) plus everything `requests` pulls in (`certifi`,
`charset-normalizer`, `idna`, `urllib3`). Every artifact pip may install is
listed by sha256, and the build runs `pip install --require-hashes --no-deps`,
so pip refuses anything whose bytes are not the reviewed bytes. `--no-deps` is
correct *only* because the closure is complete — if a new dependency is added
without being locked, the build fails rather than quietly resolving it.

**What this buys.** A rebuild installs exactly the reviewed versions or it
fails loudly. It does not make the image bit-for-bit identical across builds —
layer metadata and `.pyc` timestamps still vary — so treat the digest of a
*published* image, not a locally rebuilt one, as the identity to promote.

### Refreshing the pins

Refreshing is a deliberate, reviewed act, not something a build does for you.

To move the base image:

```sh
TOKEN=$(curl -s 'https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/python:pull' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')
curl -sI -H "Authorization: Bearer $TOKEN" \
  -H 'Accept: application/vnd.oci.image.index.v1+json' \
  'https://registry-1.docker.io/v2/library/python/manifests/3.12-slim' \
  | grep -i '^docker-content-digest'
```

To move a package, bump its version and replace **every** `--hash` line for it
with the sha256 of each artifact of the new version, from
`https://pypi.org/pypi/<name>/<version>/json`. Leaving one stale hash in place
does not fail the build — pip only needs the artifact it selects to match
*some* listed hash — so replace the whole block, not one line.

Verify a change with a clean build and a negative control:

```sh
docker build --no-cache -t goobers-exporter deploy/monitoring   # must pass
# then corrupt every hash of one package in a scratch copy: the build must fail
```
