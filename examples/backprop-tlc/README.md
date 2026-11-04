# Independent finite-graph check

`positive.json` and `broken.json` are editable **finite transition graphs**, not
programs or agent-authored proof reports. `Spec.tla` is a separately reviewed
property over `Candidate.state`: the reachable integer state must not equal 2.
This intentionally small first adapter does **not** accept arbitrary candidate
TLA+, claims, scripts, assumptions, or proof text.

## Operator setup (offline)

Install a closed Java runtime and the
[TLC v1.8.0 jar](https://github.com/tlaplus/tlaplus/releases/tag/v1.8.0) in
operator-owned locations outside every candidate checkout. No production
download or PATH lookup occurs. The exact jar SHA-256 is
`e20447644bfabd29d6bd0fc2c0eb1473fb301c1a0e4dfe1961938ce5a191edbf`.
Its actual banner is `2026.11.03.180953 (rev: 309e489)`; the report records
both the release and build identity.

Review the two specification files, then use an existing Python installation:

```text
python examples/backprop-tlc/prepare.py --config <runner-config>/verification/example --java-home <absolute-java-runtime> --jar <absolute-tla2tools.jar>
```

The script only reads installed tools, hashes the full runtime, and writes the
reviewed spec/config/manifest. It does not install or download anything.
`runtimeDigest` is SHA-256 of compact JSON of a sorted array of objects with
`path` then `digest` fields, containing every regular runtime file. Paths are
slash-relative to the runtime root; symlinks are rejected. This is the same
algorithm used when the runner snapshots the runtime. Never let a candidate
agent edit the generated manifest, trusted config, tool directories, or runner
scratch root. Ordinary self execution does not create a new OS security
boundary against another process running as the same user.

The manifest selects exactly one invariant and pins the exact specification and
configuration bytes. Configuration must be exactly:

```text
INIT Init
NEXT Next
INVARIANT Safe
CHECK_DEADLOCK FALSE
```

The trailing LF is significant. The specification must begin exactly with
`---- MODULE Spec ----` and `EXTENDS Candidate` on separate LF-terminated lines,
and must not declare/import any additional modules. This closes the dependency
set over the generated graph and pinned integer standard library, excluding
filesystem/process/Java-override imports and unpinned ambient input reads.
No constraints, simulation mode, substituted
modules, admitted proof obligations, or TLA+ `ASSUME`/`AXIOM` constructs are
supported. Additional manifest assumptions must be explicitly operator-reviewed;
the candidate graph cannot add them.

## Workflow opt-in

Use the local **self** runner (remote/pod execution fails closed). Add this
deterministic task to an enrolled workflow, with no `command` or `script`:

```yaml
- name: verify-model
  type: deterministic
  run:
    workspace: repo-readonly
  inputs:
    kind: tlc
    tlcManifest: verification/example/manifest.json
    tlcModel: examples/backprop-tlc/positive.json
    timeout: 30s
    maxOutputBytes: "1048576"
```

The manifest path is contained in the runner's selected configuration directory,
not the checkout. The graph path is contained in the candidate workspace.
Config and candidate roots must be disjoint; runner-owned scratch must be
outside the candidate root. This kind requires no capabilities and refuses
credential capabilities, user commands, environment overrides, result files,
and shell-report opt-in. The existing child-process trust refusal, network
policy, credential-path protection, process-tree cleanup, memory limits,
environment filtering, and output scrubbing remain enforced by `ShellExecutor`.
This checker kind disables instance-extra ambient passthrough/default variables
and neutralizes Java launcher-option variables; it does not accept a replacement
execution environment. It does not relax a runner's declared isolation policy.
Timeout is at most one minute (30 seconds by default); each output stream is
at most 1 MiB. Input/spec/manifest files are at most 1 MiB. Runtime snapshots
are bounded to 4096 files, 512 MiB total, and 256 MiB per file.

The runner reads fresh graph bytes for each attempt, validates nonempty initial
states and declared transition endpoints (at most 1024 states/8192 edges),
and generates `Candidate.tla` without any candidate expression interpolation.
It copies the reviewed spec/config, pinned jar, and **entire** pinned Java
runtime into private runner scratch before execution. Dependency hashes cover
the copied bytes. Source changes after this point cannot change the checked
snapshot; results never attest to a later checkout.

Successful adapter execution reports `verificationOutcome`, one of `proven`,
`counterexample`, or `inconclusive`. **Stage success means report completion,
not approval or proof of the Go program.** All three outcomes retain the
runner's actual checker stdout, generated module, execution status/argv/times,
subject, spec/config bundle, full dependency inventory, and versioned adapter
context as journal artifacts before the verification annotation. The annotation
uses the captured checker stdout as its source; Backprop accepts it only after
the runner records successful stage completion. Missing/mismatched tools or
changed reviewed inputs fail preparation without a conclusive report. Timeouts,
crashes, evaluation errors, truncation, simulation, and incomplete exploration
cannot become `proven`. Cancellation retains the original failed/canceled
result. A real invariant failure must name the requested invariant, include a
state trace, finish normally, and have TLC's safety-violation exit code 12.

## Reviewed native TLA+ mode

The same executor can check an **entirely operator-reviewed native model**,
without graph translation. This is a separate manifest opt-in, not permission
for an implementation agent to supply TLA+, checker flags, or an invariant:

```json
{
  "mode": "reviewed-native",
  "module": "TerminalRecovery",
  "property": "terminal-replay-stable",
  "invariant": "TerminalReplayStable",
  "specification": "TerminalRecovery.tla",
  "specDigest": "sha256:<exact reviewed source digest>",
  "configuration": "TerminalRecovery.cfg",
  "configDigest": "sha256:<exact reviewed configuration digest>",
  "assumptions": ["four terminal outcomes and MaxCrashes=2; no operator reopen or Go refinement"]
}
```

The snippet elides the same required `jar`, `runtime`, `runtimeDigest`, and
`java` pins used above. Copy the native files into the disjoint trusted config
tree and set their exact byte hashes as an **operator review action**; the
graph-only `prepare.py` helper does not authorize or convert native assets.
Use only `kind: tlc` and `tlcManifest` in the workflow inputs, plus optional
timeout/output bounds. **Omit `tlcModel`**: native mode refuses that key even
when its value is empty. Source/config filenames in the manifest are contained
paths under trusted config, never candidate workspace paths.

The native source must start with its matching module header followed by one
`EXTENDS` line importing only `Naturals` and/or `Integers`. Additional/nested
modules, `INSTANCE`, assumptions, axioms, theorem/proof constructs, filesystem
and process imports are rejected. A native root module also cannot collide
with a Java override shipped in the pinned checker jar. There is no user
classpath, candidate Java bytecode, or arbitrary module/dependency search.
The conservative keyword checks also reject these constructs inside comments
or string literals.

Native configuration accepts one optional `CONSTANTS` block with scalar
boolean/nonnegative integer/simple string values or flat finite sets, exactly
one `SPECIFICATION <operator>`, one `INVARIANT(S)` identifier list that includes
the requested invariant, and `CHECK_DEADLOCK FALSE`. Values and invariant lists
must fit on individual lines. Comments, substitutions, constraints, symmetry,
simulation and temporal-property directives are unsupported. Other configured
invariants remain enforced: a `TypeOK` failure does **not** become a
`TerminalReplayStable` counterexample or a proven result. Empty initial-state
sets, incomplete exploration, or checker errors are inconclusive.

For issue #8314, the native configuration is checked unchanged, including
`SPECIFICATION Spec`, constants and **both** `TypeOK` and
`TerminalReplayStable`. The bad configuration's `TrustCheckpoint = TRUE`
is a separately reviewed, explicitly pinned **model-only mutation**, not a
candidate override. Good and bad reports have the same raw source `subject`,
but different source/config specification and manifest context digests.
They are not represented as a repaired Go implementation.

Native reports record adapter `tlc-reviewed-native-v1`, manifest mode and
assumptions, the actual TLA+ source bytes as `subject`, the exact source/config
bundle as `specification`, full tool/runtime pins, execution provenance and
captured TLC stdout. They use the same runner annotation and Backprop acceptance
path as graph mode. Success claims only the requested **finite-model**
invariant under those exact bounds; no Go correctness/refinement, deadlock,
liveness, or unbounded-proof claim is added.

## Reproduce the checks

Set `JAVA_HOME`/`PATH` to your installed runtime, `GOOBERS_TEST_JAVA_HOME` to its
absolute root, and `GOOBERS_TEST_TLC_JAR` to the absolute pinned jar path:

```text
GOTOOLCHAIN=go1.26.10 go test ./internal/executor -run '^(TestTLC|TestVerification)' -count=1
GOTOOLCHAIN=go1.26.10 TESTDEP_STRICT=1 go test -tags=integration ./internal/executor -run '^TestIntegrationTLC(ReviewedNative)?$' -count=1 -v
```

The integration tests declare Java through `testdep.Require`; they never
download tools. They exercise registry dispatch, actual checker execution,
runner-owned publication, and acceptance by the Backprop graph:

| Input | Actual result |
|---|---|
| `positive.json` | `proven`: 3 generated / 2 distinct states, 0 queued, depth 2 |
| `broken.json` | `counterexample`: exit 12, 3 states, trace `0 -> 1 -> 2` |
| Positive graph, 1 ns execution budget | `inconclusive`: process timeout |

The native integration test reads the actual source and configurations from
`test/formal/recovery` in this checkout, without a graph translation or duplicate
model. CI provisions the pinned checker and a closed copy of Java, sets the two
tool variables above, and requires both integration tests to pass rather than
silently skip:

```text
GOTOOLCHAIN=go1.26.10 TESTDEP_STRICT=1 go test -tags=integration ./internal/executor -run '^TestIntegrationTLCReviewedNative$' -count=1 -v
```

Actual native results through dispatch, disk journal and Backprop projection:

| Native configuration | Actual result |
|---|---|
| `TerminalRecovery.cfg` | `proven`: 140 generated / 104 distinct states, 0 queued, depth 9 |
| `TerminalRecoveryBad.cfg` | `counterexample`: exit 12, 51 generated / 42 distinct, five-state trace |
| Good native model, 1 ns execution budget | `inconclusive`: process timeout |

This is finite-state invariant checking, **not an unbounded or collision-free
mathematical proof**, and not a refinement proof of Go code. Reports explicitly
retain TLC's 64-bit fingerprint collision caveat and exclusion of deadlock and
liveness checking. Native mode checks the independent recovery model's actual
reviewed TLA+ bytes; neither mode establishes a refinement relationship to Go.

## Initial experiment boundary

The initial experiment is offline checker acceptance, not a claim that Backprop
improves repair quality. Its success threshold is all six documented graph/native
cases producing their expected outcomes and zero false conclusive outcomes in
the refusal/error tests (changed pins, missing dependencies, invalid obligations,
timeouts and incomplete output). Execution artifacts retain runtime and checker
status for subsequent cost/maintenance comparisons. A live repair-quality or
causal-attribution experiment needs a separate held-out baseline.
