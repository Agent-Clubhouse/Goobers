# Domain: daemon-stability

## Summary
The daemon itself is churn-heavy but functional: 29 sessions in 15 days (Jul 24-Aug 8), 13 of them (45%) recovering from a dirty stop, five running '-dirty' locally-modified builds; the telemetry ingest cursor is fresh (4.6KB behind the 355MB journal) and intake.db is healthy. The dominant daemon-level defect is quota blast radius: both gaggles' PATs share one GitHub per-user rate limit AND upstream keys its quota gate by provider name only, so the goobers-site retry storm paused the productive goobers gaggle's scheduling 6,305 times and drove real 403s on Agent-Clubhouse/Goobers demand counting Aug 5-7. Storage is dominated by two unbounded-growth defects: a single Jul 21-22 sweep incident wrote 281MB of error text (33% of telemetry.db, 81% of the never-compacted journal), and scheduler-span ingest has no cursor, re-parsing and re-upserting all 75K spans (55.6MB) every ingest cycle. The read model has a dead queue: dirty_day is produced on every projection commit but its consumer (DirtyDays/RecomputeDay) is wired nowhere, so bucket_day has been empty since the feature shipped; unpublished leaks 1,715 ghost rows. Telemetry silently drops all daemon-lifecycle and lock-contention event types, so none of this instability is visible in scheduler_events; claims.json file-lock contention (244 timeouts at 30s, 4,505 slow acquisitions, ongoing Aug 8) surfaces only as generic executor_error run failures.

## Findings

### [HIGH/confirmed] quota-blast-radius
**Claim:** The goobers-site token's un-circuit-broken retry storm (K2) did not stay contained to goobers-site: it repeatedly halted the productive goobers gaggle's scheduling. Both PATs authenticate the same GitHub user (masra91), which shares one 5,000/hr REST quota across tokens, and upstream's quota gate is additionally keyed by provider name only, so one exhausted window pauses every gaggle. 83% of provider-quota tick skips hit the goobers gaggle, and the scheduler's demand counting got real 403 remaining=0 responses on Agent-Clubhouse/Goobers.

**Evidence:** instance.yaml comment 'Dedicated fine-grained PAT... (same masra91 identity)'; internal/localscheduler/providerquota.go:112 'windows map[apiv1.Provider]providerQuotaWindow'; journal: grep tick.skipped+provider-quota by gaggle => 6305 goobers vs 1320 goobers-site; SQL: SELECT substr(occurred_at,1,10), CASE WHEN message LIKE '%masra91/Goobers-Site%' THEN 'site' WHEN message LIKE '%Agent-Clubhouse/Goobers%' THEN 'goobers' END, COUNT(*) FROM scheduler_errors WHERE code='schedule_demand_count_failed' GROUP BY 1,2 => goobers rows Aug 5/6/7 = 491/180/79; poll.shed 2,165 events since Aug 2 with reason 'provider-quota-budget: provider=github ... remaining=0'

**Fix locus:** upstream-runtime  |  **Related:** K2, #2685, #788d6768; instance mitigation: separate GitHub identity for goobers-site

### [HIGH/confirmed] span-ingest-no-cursor
**Claim:** Scheduler-span ingest is O(all-spans-ever) on every ingest cycle: ingestSchedulerLog reads the entire scheduler/spans/spans.jsonl (currently 55.6MB, 75,012 spans) and delete+reinserts every span into telemetry.db each time it runs (per finished run and at shutdown), while events correctly use a byte-offset cursor. Per-tick work grows without bound and churns the WAL (checkpoint after every ingest per #530).

**Evidence:** internal/telemetry/rollup/ingest.go:690 'spans, err := readSchedulerSpans(schedulerDir)' with no cursor; ingest.go:750-758 deleteSpan+insertSpans per span; internal/telemetry/rollup/reader.go:145-158 readSpanFile does os.ReadFile of whole file; wc -l scheduler/spans/spans.jsonl = 75012 (55,612,779 bytes); contrast events cursor: scheduler_ingest_cursor byte_offset=354943626; callers cmd/goobers/daemon.go:907, cmd/goobers/runnerwiring.go:271

**Fix locus:** upstream-runtime  |  **Related:** same pattern as portal-workflows O(all-runs) bug (fixed); ~20K spans/day added

### [MEDIUM/confirmed] unbounded-error-blobs
**Claim:** scheduler_errors messages have no size cap: 108 stalled_run_sweep_failed rows (Jul 21-22, each an errors.Join of ~1,715 per-directory 'not a run directory' errors, max single message 2.6MB) total 281.5MB — 33% of the 854MB telemetry.db — and the same lines occupy 287.4MB (81%) of the 355MB scheduler/events.jsonl. Neither store is ever pruned: telemetry retention (MaintainSchedulerRetention/Compact) is opt-in and the running instance.yaml has no retention block.

**Evidence:** SQL: SELECT code, COUNT(*), SUM(LENGTH(message)), MAX(LENGTH(message)) FROM scheduler_errors GROUP BY code => stalled_run_sweep_failed|108|281505564|2606533; dbstat: scheduler_errors=270MB (largest table); awk length>1MB on scheduler/events.jsonl => 108 lines, 287417920 bytes; cmd/goobers/telemetryretention.go:70 gated on config.Enabled, instance.yaml (git show main:instance.yaml) has telemetry.enabled only; internal/telemetry/rollup/ingest.go:726 inserts Redact(message) untruncated

**Fix locus:** upstream-runtime  |  **Related:** phantom-dir root cause below; retention also instance-config (enable telemetry.retention)

### [MEDIUM/confirmed] claims-lock-contention
**Claim:** The claims.json file lock is a daemon-wide contention point: 4,505 claim_lock_slow and 244 claims_lock_timeout journal events (Jul 21-Aug 8, peak 1,627 slow on Jul 28, still occurring in the final Aug 8 session with a 10s wait). Each 30s timeout failed a run stage, and on Jul 26-28 the scheduler's own demand counting timed out on the same lock 33 times (schedule_demand_count_failed 'claims lock operation "pr-claim.count" timed out after 30.0s').

**Evidence:** grep -c type:claim_lock_slow / claims_lock_timeout in scheduler/events.jsonl => 4505/244; per-day buckets in journal; sample event seq 55503 (run c0e89d73f51a441f2f76e8cc457b02b2, failureClass infra); SQL: scheduler_errors WHERE code='schedule_demand_count_failed' AND occurred_at<'2026-08-04' => 33 rows all 'claims lock operation pr-claim.count timed out'; last session: seq 278394 waitDuration 10.031074s (pid 84234, Aug 8)

**Fix locus:** upstream-runtime  |  **Related:** claims.json is 329KB with 656 history entries; InstanceLog.Append 1.3s/append memory note

### [MEDIUM/confirmed] telemetry-observability-gap
**Claim:** The telemetry ingest whitelist silently drops every daemon-instability signal: daemon.started/clean_shutdown/dirty_restart, config.reloaded, claim_lock_slow, claims_lock_timeout, and runner.annotation never reach scheduler_events/scheduler_errors, and scheduler_events has no gaggle column so quota skips cannot be attributed per gaggle in SQL. A claims_lock_timeout run failure lands in stage_attempts as generic error_code='executor_error', error_class='unknown', so K2's ~2600 'executor_error' bucket conflates rate-limit and lock-timeout causes.

**Evidence:** internal/telemetry/rollup/ingest.go:715 switch admits only eventInitCompleted..eventError+eventWorkflowStarved; journal has 29 daemon.started etc. vs zero such rows in scheduler_events (SELECT type... GROUP BY shows 10 types); SELECT COUNT(*) FROM stage_attempts WHERE error_code='claims_lock_timeout' => 0; run c0e89d73f51a441f2f76e8cc457b02b2 query-backlog => executor_error/unknown despite journal seq 55503 claims_lock_timeout; scheduler_events schema has no gaggle column while raw journal events carry "gaggle"

**Fix locus:** upstream-runtime  |  **Related:** K2 attribution refinement

### [MEDIUM/confirmed] restart-cadence
**Claim:** Daemon crash/restart cadence Jul 24-Aug 8: 29 starts, 17 clean shutdowns, 13 dirty restarts (~2 sessions/day; 45% of stops unclean). Five sessions ran '-dirty' (uncommitted) local builds in production (ebf8ace9-dirty, fe6a2d63-dirty, 453e283f-dirty, 1d42e809-dirty, f85e7c35-dirty). Two sessions (pids 73881, 18237, both build 0366ec74) wrote up.lock but never journaled daemon.started — crash before first append. The final session (pid 84234, started Aug 8 11:36) also ended without clean_shutdown seconds after a successful config apply, leaving today's up.lock stale. One 2.8-day full outage Jul 30-Aug 1; remaining large event gaps cluster overnight local time, consistent with laptop sleep rather than crashes.

**Evidence:** grep '"type":"daemon\.' scheduler/events.jsonl => 59 events (29/17/13); dirty_restart seqs 158364, 159262 reference pids 73881/18237 with no matching daemon.started; up.lock pid 84234 startedAt 2026-08-08T18:36:12Z, ps -p 84234 exits 1; gap query on scheduler_events shows 4001-min gap 2026-07-30T00:51Z..2026-08-01T19:33Z; journal tail: config.reloaded seq 278557 at 12:05:02-07:00 is the final event

**Fix locus:** process  |  **Related:** up-lock-can-go-stale memory; adopt-build-features workflow

### [MEDIUM/confirmed] dead-bucket-queue
**Claim:** The read-model day-bucket aggregation (#1931 §5.6) is producer-only dead code in the running daemon: markDayDirty runs in every projection transaction, but Store.DirtyDays/RecomputeDay/RecomputeMonth have zero non-test callers anywhere in cmd/ or internal/. Result: bucket_day has 0 rows ever, while dirty_day holds all 23 days since Jul 16 unconsumed (Jul days marked at the Aug 1 rebuild, Aug days at each midnight). Any portal/API surface reading DayBuckets returns empty.

**Evidence:** sqlite3 read.db 'SELECT COUNT(*) FROM bucket_day' => 0; 'SELECT * FROM dirty_day' => 23 rows Jul 16..Aug 8 never deleted; grep -rn 'DirtyDays|RecomputeDay' cmd/ internal/ --include='*.go' | grep -v _test => only internal/readmodel/buckets.go:72/108/169 definitions, no callers

**Fix locus:** upstream-runtime  |  **Related:** #1931, portal read arch epic #1912

### [MEDIUM/confirmed] terminal-claim-reaper
**Claim:** The terminal-claim reaper cannot inspect claims created by backlog-reconcile: reserveBacklogClaimReconciliation synthesizes claim RunIDs of the form '<run>/backlog-reconcile/<pid>/<seq>', which FindRunDir rejects ('invalid run id' — contains slashes), so claimHolderTerminal errors instead of releasing. A leaked reconcile claim (process died between Claim and Release) blocks its backlog item until TTL expiry and emits terminal_claim_inspection_failed every hourly sweep — 64 occurrences Jul 27-Aug 7.

**Evidence:** cmd/goobers/backlogreconcile.go:182-187 runID := fmt.Sprintf("%s/backlog-reconcile/%d/%d", ...); cmd/goobers/claims.go:275-281 claimHolderTerminal -> FindRunDir(entry.RunID); internal/instance/runtime.go:139 filepath.Base(runID) != runID check; SQL: SELECT seq, occurred_at, message FROM scheduler_errors WHERE code='terminal_claim_inspection_failed' ORDER BY seq DESC LIMIT 5 => 'inspect holding run 3e3efe330401b52eb5fc523a2502ba70/backlog-reconcile/7180/7: invalid run id ...', hourly at :41

**Fix locus:** upstream-runtime  |  **Related:** claims.json history shows 435 backlog-reconcile mentions; live entries clean (3, v2-scoped)

### [MEDIUM/probable] askpass-relative-path
**Claim:** K3 mechanism identified in code: githubWorktreeGitEnvironment bakes filepath.Join(workcopiesDir, "auth")+"/goobers-askpass.sh" into GIT_ASKPASS and WriteAskpassScript never absolutizes it, so a Layout constructed from a relative instance root produces exactly the observed failing string 'gaggles/goobers-site/workcopies/auth/goobers-askpass.sh', which git then resolves against the subprocess CWD (the workcopy), not the instance root => ENOENT even though the file exists. Fix: filepath.Abs at write time.

**Evidence:** cmd/goobers/runnerwiring.go:2438 credentials.WriteAskpassScript(filepath.Join(workcopiesDir, "auth")); internal/credentials/git.go:66-71 returns filepath.Join(dir, name) unmodified; failing path in K3 equals ForGaggle("goobers-site").WorkcopiesDir()+"/auth/..." with empty root; historical precedent of wrong roots: daemon sessions Jul 23-24 journaled instanceRoot '/Users/masonallen/source/goobers-instances/goobers' (nested) before instance commit e1786a9 'promote instance root to repo root'

**Fix locus:** upstream-runtime  |  **Related:** K3

### [LOW/confirmed] unpublished-ghost-leak
**Claim:** read.db's unpublished memo table leaks entries for deleted directories: all 1,715 rows were seen at the Aug 1 rebuild for the phantom no-run.yaml dirs (dir_mtime all 2026-07-24T16:37:00Z, touched during the flat-root->gaggles migration), those dirs no longer exist (0 dirs missing run.yaml today, none of the 1,715 in the run table), and ClearUnpublished only fires when a dir is published — vanished dirs are never visited by the walk, so the memos persist forever.

**Evidence:** SQL: SELECT substr(seen_at,1,10), COUNT(*) FROM unpublished GROUP BY 1 => 2026-08-01|1715; SELECT COUNT(*) FROM unpublished u WHERE EXISTS(SELECT 1 FROM run r WHERE r.run_id=u.run_id) => 0; shell loop over gaggles/goobers/runs/*/run.yaml => 0 missing; ls of sample dir 00033c291df81fafb861cf41b14dbb3d => No such file; internal/readmodel/sweep.go:186-196 ClearUnpublished only path that deletes

**Fix locus:** upstream-runtime  |  **Related:** same phantom dirs caused the 281MB error-blob incident (Jul 21-22 stalled_run_sweep_failed)

### [LOW/confirmed] storage-composition
**Claim:** telemetry.db (854MB, freelist 0) decomposes as: scheduler_errors 270MB (the one-off blob incident), span data ~335MB (span_events 197 + spans 53 + 5 span indexes ~84), scheduler_events 28MB, everything else <60MB; current growth ~20K spans + ~20K scheduler events/day (~30-40MB/day) with no retention configured. read.db (233MB used, rebuilt Aug 1) is ~80% indexes: run_stage 18 + run 16 + change 13 = 47MB of data under ~186MB across 26+ composite indexes. intake.db is 10MB but holds only ~16KB of live rows. Sustainable for weeks, not quarters, without enabling retention.

**Evidence:** telemetry dbstat: scheduler_errors|270, span_events|197, spans|53, scheduler_events|28; PRAGMA freelist_count=0, page_count=208549; spans/day query => 15-22K rows/day Aug 5-8; read.db dbstat SUM=233MB, top tables 18/16/13MB with 26 idx_run*/idx_change* entries; intake.db dbstat: all objects 4KB each

**Fix locus:** instance-config  |  **Related:** enable telemetry.retention (defaults: 90d window / 500 max runs); read.db index count is upstream design

### [LOW/confirmed] scheduler-dir-debris
**Claim:** scheduler/ (104 entries) accumulates permanent debris: 66 zero-byte api-read-cache.lock.<hex16> per-list-key lock files (one per distinct Authorization+URL hash, never cleaned, Jul 29-Aug 8), plus operator backup files (claims.json.bak-* x2, blocked.json.bak-* x2) and 3 unconsumed pending-applies/*.response.json from the final minute (two 'stale request ... refusing to dispatch', one successful apply a93a988e->0fa2790c that matches the journal-final config.reloaded seq 278557). .cache-backups/ holds 30MB of Jul 27/29 api-read-cache snapshots; updates/ is empty. All zero-byte *.lock files are flock-style and benign.

**Evidence:** ls scheduler/ | grep -c 'api-read-cache.lock\.' => 66; cmd/goobers/apireadcache.go:330-333 apiReadListLockPath creates per-key lock names with no cleanup path; cat scheduler/pending-applies/*.response.json; du -sh .cache-backups => 30M; config.reloaded seq 278557 newDigest sha256:0fa2790c... equals pending-applies 4040675297.response.json newDigest

**Fix locus:** upstream-runtime  |  **Related:** 

### [INFO/confirmed] ingest-cursor-health
**Claim:** Scheduler journal ingest is currently healthy and the Jul 16-17 ingest crash-loop is historical and fixed: the cursor (byte_offset 354,943,626 / last_seq 278,555) trails the 354,948,284-byte journal by only 4.6KB (the final config.reloaded + shutdown race). The 2,157 telemetry_ingest_scheduler_log_failed errors ('insert scheduler_event seq 5: UNIQUE constraint') looped for 20h on Jul 16-17 under an older build; current code inserts with ON CONFLICT(seq) DO NOTHING (#1411) making recurrence impossible.

**Evidence:** SELECT byte_offset, last_seq FROM scheduler_ingest_cursor => 354943626|278555; ls -la scheduler/events.jsonl => 354948284; SELECT MIN/MAX(occurred_at) FROM scheduler_errors WHERE code='telemetry_ingest_scheduler_log_failed' => 2026-07-16T04:27..2026-07-17T00:46, all identical 'seq 5' message; internal/telemetry/rollup/ingest.go:717-721 ON CONFLICT(seq) DO NOTHING

**Fix locus:** upstream-runtime  |  **Related:** #1411

### [INFO/confirmed] legacy-symlinks
**Claim:** Instance-root runs->gaggles/goobers/runs and workcopies->gaggles/goobers/workcopies are product-generated single-gaggle 'compatibility aliases' from the Jul 25 flat-root migration (instance commit e1786a9), and upstream handles them correctly — RunDirs/WorkcopiesDirs explicitly skip symlinked legacy roots to avoid double-scanning, and MigrateLegacyRuntime recognizes generated aliases. They are now semantically misleading in a two-gaggle instance (root runs/ shows only goobers' 51,568 dirs, hiding goobers-site's 4,248) but pose no daemon-level hazard.

**Evidence:** ls -la instance root: symlinks dated Jul 25 00:43; internal/instance/runtime.go:51-63 and 101-113 skip ModeSymlink legacy roots; runtime.go:472-488 + isGeneratedRuntimeAlias (runtime.go:520+); dir counts: ls gaggles/goobers/runs|wc -l => 51568, gaggles/goobers-site/runs => 4248; daemon.started events Jul 23-24 show pre-migration nested instanceRoot .../goobers-instances/goobers

**Fix locus:** instance-config  |  **Related:** GAG-011; instance repo commit e1786a9

### [INFO/confirmed] sweep-freshness
**Claim:** The read-model repair sweep was live and converging at shutdown: sweep_cursor mid-walk in gaggles/goobers-site/runs (after_name fe953849..., 4,224 entries this cycle) with cycle timestamps at 2026-08-08T18:55:37Z, ~10 minutes before the final journal event — no projection lag beyond the wired-nowhere bucket queue and the ghost unpublished rows reported separately. projection_state: schema v8, epoch 739af675..., built_at 2026-08-01T19:32:17Z (full rebuild after the 2.8-day outage), ready=1, tombstones 0, projection_floor unset (no aging yet).

**Evidence:** SELECT * FROM sweep_cursor => gaggles/goobers-site/runs|fe953849b8cd7412a59f5a3ac4778f63|2026-08-08T18:55:37.540197000Z|same|4224; SELECT * FROM projection_state => 1|8|739af675111e4d4f52e55e981bc759be|0|||2026-08-01T19:32:17.066928000Z|1; SELECT COUNT(*) FROM tombstone => 0

**Fix locus:** unknown  |  **Related:** rebuild timing matches daemon.started pid 3579 2026-08-01T12:33-07:00
