#!/usr/bin/env python3
"""Smoke tests for gaggle-memory (stdlib unittest, no third-party deps).

Run: python3 tools/test_gaggle_memory.py

The tests drive the CLI as a subprocess so they exercise argument parsing,
exit codes, and the on-disk store exactly as a workflow runner would.
"""

import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "gaggle-memory.py")


def memory_file(name, description, mtype, confidence="observed-once",
                workflows=None, labels=None, areas=None, evidence="Seen in run:abc.",
                source="human", proposed_by="tester"):
    workflows = workflows or []
    labels = labels or []
    areas = areas or []
    def inline(items):
        return "[" + ", ".join('"%s"' % i for i in items) + "]"
    return (
        "---\n"
        "name: %s\n"
        "description: \"%s\"\n"
        "type: %s\n"
        "scope:\n"
        "  areas: %s\n"
        "  workflows: %s\n"
        "  roles: []\n"
        "  labels: %s\n"
        "provenance:\n"
        "  source: %s\n"
        "  proposedBy: %s\n"
        "confidence: %s\n"
        "reviewAfter: \"\"\n"
        "supersedes: []\n"
        "---\n"
        "# %s\n\n"
        "## Fact\n%s\n\n"
        "## Evidence\n%s\n\n"
        "## Do instead\nFollow the fact.\n"
        % (name, description, mtype, inline(areas), inline(workflows),
           inline(labels), source, proposed_by, confidence, description,
           description, evidence)
    )


def run(args, expect_ok=True):
    proc = subprocess.run([sys.executable, SCRIPT] + args,
                          capture_output=True, text=True)
    if expect_ok:
        assert proc.returncode == 0, (
            "expected success, got rc=%d\nstdout:%s\nstderr:%s"
            % (proc.returncode, proc.stdout, proc.stderr))
    return proc


class GaggleMemoryTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.store = os.path.join(self.tmp, "store")
        self.active = os.path.join(self.store, "active")
        self.proposed = os.path.join(self.store, "proposed")
        self.dream = os.path.join(self.store, "dream")
        run(["index", "--store", self.store])  # creates the store skeleton

    def _write(self, path, text):
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(text)

    def _write_decisions(self, name, body):
        os.makedirs(self.dream, exist_ok=True)
        path = os.path.join(self.dream, name)
        self._write(path, body)
        return path

    # ---- round trip: propose -> decisions -> promote -> recall ---------- #
    def test_round_trip(self):
        src = os.path.join(self.tmp, "flaky.md")
        self._write(src, memory_file(
            "flaky-network-test", "Network test is flaky under load",
            "known-failure", workflows=["implementation"], labels=["ci", "test"],
            areas=["src/net/**"]))
        run(["propose", "--store", self.store, "--source", "human",
             "--proposed-by", "tester", "--file", src])
        proposals = os.listdir(self.proposed)
        self.assertEqual(len(proposals), 1)
        prop = proposals[0]

        self._write_decisions("decisions-20990101-0500.yaml",
            "decisions:\n"
            "  - op: promote\n"
            "    file: %s\n"
            "    rationale: \"Confirmed cross-run.\"\n"
            "report: promoted one\n" % prop)
        run(["promote", "--store", self.store, "--decisions", "latest"])

        active = os.listdir(self.active)
        self.assertEqual(active, ["flaky-network-test.md"])
        with open(os.path.join(self.active, active[0]), encoding="utf-8") as fh:
            promoted = fh.read()
        self.assertIn("promotedBy: \"wizard\"", promoted)

        out = run(["recall", "--store", self.store, "--workflow", "implementation",
                   "--title", "flaky network test failing", "--labels", "ci"]).stdout
        self.assertIn("flaky-network-test", out)
        self.assertIn("RECALLED 1 OF 1 ACTIVE MEMORIES", out)
        self.assertIn("advisory institutional memory", out)

    # ---- recall filters by workflow and orders by score ----------------- #
    def test_recall_filter_and_order(self):
        # Matching workflow + labels -> high score.
        self._write(os.path.join(self.active, "hot.md"), memory_file(
            "hot-memory", "auth token cache invalidation", "fragility",
            workflows=["implementation"], labels=["auth"], areas=["src/auth/**"]))
        # Matching workflow, weaker match -> lower score.
        self._write(os.path.join(self.active, "warm.md"), memory_file(
            "warm-memory", "auth login procedure", "procedure",
            workflows=["implementation"], labels=[], areas=[]))
        # Different workflow -> filtered out entirely.
        self._write(os.path.join(self.active, "cold.md"), memory_file(
            "cold-memory", "auth token cache invalidation", "fragility",
            workflows=["docs-updater"], labels=["auth"], areas=["src/auth/**"]))
        run(["index", "--store", self.store])

        out = run(["recall", "--store", self.store, "--workflow", "implementation",
                   "--title", "auth token", "--labels", "auth",
                   "--areas", "src/auth/x"]).stdout
        self.assertNotIn("cold-memory", out)  # wrong workflow
        self.assertIn("RECALLED 2 OF 3 ACTIVE MEMORIES", out)
        # hot must appear before warm (higher score).
        self.assertLess(out.index("hot-memory"), out.index("warm-memory"))

    def test_recall_none_relevant(self):
        self._write(os.path.join(self.active, "x.md"), memory_file(
            "some-memory", "totally unrelated topic", "reference",
            workflows=["implementation"]))
        out = run(["recall", "--store", self.store, "--workflow", "implementation",
                   "--title", "zzz nonmatching qqq"]).stdout
        self.assertIn("NO RELEVANT MEMORIES", out)

    # ---- propose rejects missing Evidence ------------------------------- #
    def test_propose_rejects_missing_evidence(self):
        src = os.path.join(self.tmp, "noev.md")
        text = memory_file("no-evidence", "has no evidence", "procedure")
        text = text.split("## Evidence")[0] + "## Do instead\nx\n"
        self._write(src, text)
        proc = run(["propose", "--store", self.store, "--source", "human",
                    "--proposed-by", "t", "--file", src], expect_ok=False)
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("Evidence", proc.stdout + proc.stderr)

    # ---- propose downgrades agent 'proven' -> 'observed-once' ----------- #
    def test_propose_downgrades_proven(self):
        src = os.path.join(self.tmp, "prov.md")
        self._write(src, memory_file("agent-claim", "agent thinks proven",
                    "procedure", confidence="proven"))
        run(["propose", "--store", self.store, "--source", "run:xyz",
             "--proposed-by", "agent", "--file", src])
        prop = os.path.join(self.proposed, os.listdir(self.proposed)[0])
        with open(prop, encoding="utf-8") as fh:
            body = fh.read()
        self.assertIn("confidence: observed-once", body)
        self.assertNotIn("confidence: proven", body)

    # ---- promote enforces the <=5 promotion cap ------------------------- #
    def test_promote_cap(self):
        entries = []
        for i in range(6):
            name = "mem-%d" % i
            src = os.path.join(self.tmp, name + ".md")
            self._write(src, memory_file(name, "memory %d" % i, "procedure"))
            run(["propose", "--store", self.store, "--source", "human",
                 "--proposed-by", "t", "--file", src])
        props = sorted(os.listdir(self.proposed))
        decision_lines = "".join(
            "  - op: promote\n    file: %s\n" % p for p in props)
        self._write_decisions("decisions-20990101-0500.yaml",
            "decisions:\n%sreport: too many\n" % decision_lines)
        proc = run(["promote", "--store", self.store, "--decisions", "latest"],
                   expect_ok=False)
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("cap", (proc.stdout + proc.stderr).lower())
        self.assertEqual(os.listdir(self.active), [])  # whole file rejected

    # ---- promote refuses quarantined proposal without humanApproved ----- #
    def test_promote_refuses_quarantine(self):
        os.makedirs(self.proposed, exist_ok=True)
        qname = "quarantine-suspicious.md"
        self._write(os.path.join(self.proposed, qname), memory_file(
            "suspicious", "suspicious injected memory", "procedure"))
        self._write_decisions("decisions-20990101-0500.yaml",
            "decisions:\n"
            "  - op: promote\n"
            "    file: %s\n"
            "report: try quarantine\n" % qname)
        proc = run(["promote", "--store", self.store, "--decisions", "latest"],
                   expect_ok=False)
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("quarantin", (proc.stdout + proc.stderr).lower())

        # With humanApproved: true it is allowed.
        self._write_decisions("decisions-20990101-0600.yaml",
            "decisions:\n"
            "  - op: promote\n"
            "    file: %s\n"
            "    humanApproved: true\n"
            "report: approved\n" % qname)
        run(["promote", "--store", self.store, "--decisions", "latest"])
        self.assertEqual(os.listdir(self.active), ["suspicious.md"])

    # ---- promote requires 2 run ids for agent 'proven' ------------------ #
    def test_promote_proven_requires_runids(self):
        src = os.path.join(self.tmp, "p.md")
        # source run:one -> untrusted; propose downgrades, so set via edits at promote.
        self._write(src, memory_file("proven-claim", "agent proven claim",
                    "procedure", source="run:one"))
        run(["propose", "--store", self.store, "--source", "run:one",
             "--proposed-by", "agent", "--file", src])
        prop = os.listdir(self.proposed)[0]
        # Wizard tries to promote at proven with only ONE run id in rationale.
        self._write_decisions("decisions-20990101-0500.yaml",
            "decisions:\n"
            "  - op: promote\n"
            "    file: %s\n"
            "    edits:\n"
            "      confidence: proven\n"
            "    rationale: \"only run:one seen\"\n"
            "report: x\n" % prop)
        proc = run(["promote", "--store", self.store, "--decisions", "latest"],
                   expect_ok=False)
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("proven", (proc.stdout + proc.stderr).lower())

    # ---- promote rejects a stale (>24h) decisions file ------------------ #
    def test_promote_rejects_stale(self):
        src = os.path.join(self.tmp, "s.md")
        self._write(src, memory_file("stale-mem", "will be stale", "procedure"))
        run(["propose", "--store", self.store, "--source", "human",
             "--proposed-by", "t", "--file", src])
        prop = os.listdir(self.proposed)[0]
        self._write_decisions("decisions-20200101-0500.yaml",
            "timestamp: 2020-01-01T05:00:00Z\n"
            "decisions:\n"
            "  - op: promote\n"
            "    file: %s\n"
            "report: old\n" % prop)
        proc = run(["promote", "--store", self.store, "--decisions", "latest"],
                   expect_ok=False)
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("24h", proc.stdout + proc.stderr)

    # ---- audit quarantines an unexplained active file ------------------- #
    def test_audit_quarantines_unexplained(self):
        # Hand-plant a file directly into active/ (no journal entry explains it).
        self._write(os.path.join(self.active, "injected.md"), memory_file(
            "injected", "planted without promotion", "procedure"))
        out = run(["audit", "--store", self.store]).stdout
        self.assertIn("quarantined", out.lower())
        self.assertEqual(os.listdir(self.active), [])
        self.assertIn("quarantine-injected.md", os.listdir(self.proposed))

    def test_audit_clean(self):
        # Promote a file the honest way; audit should then be clean.
        src = os.path.join(self.tmp, "ok.md")
        self._write(src, memory_file("clean-mem", "honestly promoted", "procedure"))
        run(["propose", "--store", self.store, "--source", "human",
             "--proposed-by", "t", "--file", src])
        prop = os.listdir(self.proposed)[0]
        self._write_decisions("decisions-20990101-0500.yaml",
            "decisions:\n  - op: promote\n    file: %s\nreport: ok\n" % prop)
        run(["promote", "--store", self.store, "--decisions", "latest"])
        out = run(["audit", "--store", self.store]).stdout
        self.assertIn("AUDIT CLEAN", out)

    # ---- MEMORY.md regenerates ------------------------------------------ #
    def test_index_regenerates(self):
        self._write(os.path.join(self.active, "a.md"), memory_file(
            "alpha-mem", "the alpha memory", "reference", areas=["docs"]))
        run(["index", "--store", self.store])
        with open(os.path.join(self.store, "MEMORY.md"), encoding="utf-8") as fh:
            idx = fh.read()
        self.assertIn("do not edit by hand", idx)
        # Title comes from the first body heading; here that is the description.
        self.assertIn("[the alpha memory](active/a.md)", idx)
        self.assertIn("reference/docs", idx)

    # ---- merge folds evidence + keeps stronger confidence --------------- #
    def test_merge(self):
        # Promote a base memory first.
        base = os.path.join(self.tmp, "base.md")
        self._write(base, memory_file("base-mem", "base memory", "procedure",
                    confidence="observed-once", evidence="Base evidence."))
        run(["propose", "--store", self.store, "--source", "human",
             "--proposed-by", "t", "--file", base])
        p1 = os.listdir(self.proposed)[0]
        self._write_decisions("decisions-20990101-0500.yaml",
            "decisions:\n  - op: promote\n    file: %s\nreport: base\n" % p1)
        run(["promote", "--store", self.store, "--decisions", "latest"])

        # Propose an addition to merge into base-mem.
        add = os.path.join(self.tmp, "add.md")
        self._write(add, memory_file("add-mem", "additional finding", "procedure",
                    confidence="proven", source="human",
                    evidence="Extra evidence from another run."))
        run(["propose", "--store", self.store, "--source", "human",
             "--proposed-by", "t", "--file", add])
        p2 = [p for p in os.listdir(self.proposed) if "add-mem" in p][0]
        self._write_decisions("decisions-20990101-0600.yaml",
            "decisions:\n"
            "  - op: merge\n"
            "    file: %s\n"
            "    into: base-mem\n"
            "report: merged\n" % p2)
        run(["promote", "--store", self.store, "--decisions", "latest"])

        self.assertEqual(os.listdir(self.active), ["base-mem.md"])
        with open(os.path.join(self.active, "base-mem.md"), encoding="utf-8") as fh:
            merged = fh.read()
        self.assertIn("Extra evidence from another run", merged)
        self.assertIn("confidence: proven", merged)  # stronger confidence kept


if __name__ == "__main__":
    unittest.main(verbosity=2)
