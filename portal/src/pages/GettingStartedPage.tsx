import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { RecoveryCommand } from "../components/RecoveryAction";
import {
  GuidedClient,
  GuidedRequestError,
  type DiagnosticsEnvelope,
  type DiagnosticsFinding,
  type GuidedEnvelopeResult,
  type GuidedInitResult,
  type GuidedJobDetail,
  type GuidedState,
  type OnboardingActionEnvelope,
  type StatusEnvelope,
} from "../guided/client";

// The guided walkthrough (#437). Every write action on this page is a thin
// wrapper over a documented CLI command executed by the getting-started
// server; the exact command is always shown next to the button. Manual steps
// — creating the disposable GitHub repository, pushing the sample, editing
// placeholders, exporting tokens — are presented explicitly and are never
// performed on the user's behalf (#2449).

const defaultClient = new GuidedClient();

const statePollIntervalMs = 5_000;
const jobPollIntervalMs = 2_000;

/** The quickstart workflow's stage names, mapped to a best-effort mini
 *  progress row by grepping run output for the CLI's stage transition lines.
 *  Absence of any match simply hides the row. */
const knownRunStages = [
  "query-backlog",
  "implement",
  "review",
  "local-ci",
  "push-branch",
  "open-pr",
] as const;

type GuidedQuery =
  | { status: "loading" }
  | { status: "unavailable" }
  | { status: "ready"; state: GuidedState };

type StepStatus = "pending" | "active" | "done" | "failed";

type BusyAction = "stub-sample" | "init" | "validate" | "run" | null;

export function GettingStartedPage({ client = defaultClient }: { client?: GuidedClient } = {}) {
  const [query, setQuery] = useState<GuidedQuery>({ status: "loading" });

  // UI-only progress bits (which manual steps the user confirmed, whether the
  // welcome step was acknowledged) live in sessionStorage; everything the
  // server can attest to (sample/instance existence, job state, env tokens)
  // comes from /guided/state and wins over anything stored here.
  const [welcomeDone, setWelcomeDone] = useSessionFlag("goobers-guided-welcome-done");
  const [pushDone, setPushDone] = useSessionFlag("goobers-guided-push-done");
  const [placeholdersDone, setPlaceholdersDone] = useSessionFlag("goobers-guided-placeholders-done");
  const [validateOk, setValidateOk] = useSessionFlag("goobers-guided-validate-ok");

  const [busy, setBusy] = useState<BusyAction>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const [stubResult, setStubResult] =
    useState<GuidedEnvelopeResult<OnboardingActionEnvelope> | null>(null);
  const [initResult, setInitResult] = useState<GuidedInitResult | null>(null);
  const [validateResult, setValidateResult] =
    useState<GuidedEnvelopeResult<DiagnosticsEnvelope> | null>(null);
  const [statusResult, setStatusResult] =
    useState<GuidedEnvelopeResult<StatusEnvelope> | null>(null);

  const [workTracking, setWorkTracking] = useState("");
  const [useMainToken, setUseMainToken] = useState(false);
  const [forceRerun, setForceRerun] = useState(false);

  const [activeJobId, setActiveJobId] = useState<string | null>(null);
  const [jobDetail, setJobDetail] = useState<GuidedJobDetail | null>(null);

  const refreshState = useCallback(async () => {
    try {
      const state = await client.getState();
      setQuery({ status: "ready", state });
      return state;
    } catch (error) {
      // Any failure to read /guided/state — a 404 from the operational
      // dashboard or daemon portal, or no server at all — means the guided
      // experience is not being served here; render the instructions instead
      // of crashing (per the portal's inline-degradation idiom).
      void error;
      setQuery((previous) =>
        previous.status === "ready" ? previous : { status: "unavailable" },
      );
      return null;
    }
  }, [client]);

  // Server truth is polled: env-token badges and sample/instance existence
  // update live as the user works in a terminal alongside this page.
  useEffect(() => {
    void refreshState();
    const timer = setInterval(() => void refreshState(), statePollIntervalMs);
    return () => clearInterval(timer);
  }, [refreshState]);

  const serverState = query.status === "ready" ? query.state : null;

  // Adopt the server's most recent job (page reload mid-run, or a run started
  // from another tab): polling it restores the output tail and completion.
  useEffect(() => {
    if (serverState?.job && activeJobId === null) {
      setActiveJobId(serverState.job.id);
    }
  }, [serverState, activeJobId]);

  useEffect(() => {
    if (!activeJobId) {
      return;
    }
    let cancelled = false;
    let timer: ReturnType<typeof setInterval> | null = null;
    const poll = async () => {
      try {
        const detail = await client.getJob(activeJobId);
        if (cancelled) {
          return;
        }
        setJobDetail(detail);
        if (detail.done) {
          if (timer !== null) {
            clearInterval(timer);
            timer = null;
          }
          void refreshState();
        }
      } catch {
        // Job vanished (server restarted): stop polling; /guided/state remains
        // the source of truth for what to render.
        if (timer !== null) {
          clearInterval(timer);
          timer = null;
        }
      }
    };
    void poll();
    timer = setInterval(() => void poll(), jobPollIntervalMs);
    return () => {
      cancelled = true;
      if (timer !== null) {
        clearInterval(timer);
      }
    };
  }, [activeJobId, client, refreshState]);

  const job = jobDetail ?? serverState?.job ?? null;
  const runDone = job !== null && job.done && job.exitCode === 0;
  const runFailed = job !== null && job.done && job.exitCode !== null && job.exitCode !== 0;

  // Success step: the Time-to-First-PR readout is computed by `goobers status
  // --json` from local journal timestamps. It is a local number — nothing on
  // this page reports it (or anything else) anywhere.
  useEffect(() => {
    if (!runDone || statusResult !== null) {
      return;
    }
    let cancelled = false;
    void client
      .getStatus()
      .then((result) => {
        if (!cancelled) {
          setStatusResult(result);
        }
      })
      .catch(() => {
        // Leave the success step's fallback copy in place.
      });
    return () => {
      cancelled = true;
    };
  }, [runDone, statusResult, client]);

  const sampleDone = (serverState?.sampleExists ?? false) || stubResult?.exitCode === 0;
  const initDone = serverState?.instanceExists ?? false;
  const stubFailed = stubResult !== null && stubResult.exitCode !== 0 && !sampleDone;
  const validateFailed = validateResult !== null && validateResult.exitCode !== 0;

  const doneFlags = [
    welcomeDone,
    sampleDone,
    pushDone,
    initDone,
    placeholdersDone,
    validateOk && !validateFailed,
    runDone,
    false,
  ];
  const failedFlags = [false, stubFailed, false, false, false, validateFailed, runFailed, false];
  const firstOpen = doneFlags.indexOf(false);
  const stepStatus = (index: number): StepStatus => {
    if (doneFlags[index]) {
      return "done";
    }
    if (index === firstOpen) {
      return failedFlags[index] ? "failed" : "active";
    }
    return failedFlags[index] ? "failed" : "pending";
  };

  const runAction = async <T,>(
    kind: Exclude<BusyAction, null>,
    action: () => Promise<T>,
    apply: (result: T) => void,
  ) => {
    setBusy(kind);
    setActionError(null);
    try {
      const result = await action();
      apply(result);
    } catch (error) {
      setActionError(error instanceof Error ? error.message : String(error));
    } finally {
      setBusy(null);
      void refreshState();
    }
  };

  const samplePath = serverState?.samplePath ?? "./getting-started-task-api";
  const instancePath = serverState?.instancePath ?? "./tutorial-instance";

  if (query.status === "loading") {
    return (
      <section aria-live="polite" className="daemon-state" role="status">
        <span aria-hidden="true" className="loading-mark" />
        <div>
          <h1>Loading the guided experience</h1>
          <p>Reading the walkthrough's local state.</p>
        </div>
      </section>
    );
  }

  if (query.status === "unavailable") {
    return (
      <>
        <header className="page-heading">
          <p className="page-kicker">Guided onboarding</p>
          <h1>Getting Started</h1>
        </header>
        <section className="empty-state">
          <img alt="" src="/goober-mascot.png" />
          <div>
            <h2>The guided experience is not running here</h2>
            <p>
              This portal is serving the operational dashboard, which has no guided action
              endpoints. Launch the walkthrough from a terminal in an empty working directory —
              it opens this page served by its own local process.
            </p>
            <RecoveryCommand command="goobers getting-started" />
          </div>
        </section>
      </>
    );
  }

  const state = query.state;

  return (
    <>
      <header className="page-heading">
        <p className="page-kicker">Guided onboarding</p>
        <h1>Getting Started</h1>
        <p>
          From an empty folder to your first autonomous pull request against a disposable
          sample repository. Every button below runs the exact CLI command shown beside it —
          this page is a wrapper over the CLI, nothing more.
        </p>
      </header>

      {actionError && (
        <section className="daemon-state daemon-state-error guided-action-error" role="alert">
          <div>
            <h1>Action failed to start</h1>
            <p>{actionError}</p>
          </div>
        </section>
      )}

      <ol className="guided-steps">
        <GuidedStep index={0} status={stepStatus(0)} title="Welcome & prerequisites">
          <p>
            This walkthrough creates three things, all local and disposable: a sample project
            folder at <code>{samplePath}</code>, a GitHub repository that <strong>you</strong>{" "}
            create for it in step 3, and a tutorial instance at <code>{instancePath}</code>{" "}
            that drives one autonomous workflow against that repository. Everything this page
            does is the printed CLI command shown in each step's chip; the manual steps are
            called out explicitly and stay yours.
          </p>
          <ul className="guided-checklist">
            <li>A GitHub account (the sample repo will live under it).</li>
            <li>Copilot CLI installed and signed in.</li>
            <li>
              Node.js &gt;= 20 and npm on <code>PATH</code> (the sample's CI uses them).
            </li>
            <li>
              <code>export GOOBERS_GITHUB_TOKEN=...</code> — and optionally{" "}
              <code>GOOBERS_GITHUB_ISSUES_TOKEN</code> for seeding starter issues.
            </li>
          </ul>
          <div aria-label="Token environment status" className="guided-badges">
            <EnvBadge name="GOOBERS_GITHUB_TOKEN" present={state.env.goobersGithubToken} />
            <EnvBadge
              name="GOOBERS_GITHUB_ISSUES_TOKEN"
              present={state.env.goobersGithubIssuesToken}
            />
          </div>
          <p className="guided-note">
            Token badges update live from this machine's environment; token values never
            reach this page — only whether each variable is set.
          </p>
          {!welcomeDone && (
            <button
              className="reconnect-button"
              onClick={() => setWelcomeDone(true)}
              type="button"
            >
              Start the walkthrough
            </button>
          )}
        </GuidedStep>

        <GuidedStep index={1} status={stepStatus(1)} title="Materialize the sample">
          <p>
            Writes the embedded <code>getting-started-task-api</code> sample to{" "}
            <code>{samplePath}</code>. With a work-tracking repo named, it also seeds the
            starter labels and issues there.
          </p>
          <RecoveryCommand
            command={stubSampleCommand(samplePath, workTracking, useMainToken, forceRerun)}
          />
          <div className="guided-fields">
            <label className="guided-field">
              <span>Work-tracking repo (optional, owner/repo)</span>
              <input
                onChange={(event) => setWorkTracking(event.target.value)}
                placeholder="your-org/your-repo"
                type="text"
                value={workTracking}
              />
            </label>
            <label className="guided-check">
              <input
                checked={useMainToken}
                onChange={(event) => setUseMainToken(event.target.checked)}
                type="checkbox"
              />
              <span>
                Seed with <code>GOOBERS_GITHUB_TOKEN</code> instead (sets{" "}
                <code>--token-env</code>; seeding otherwise uses{" "}
                <code>GOOBERS_GITHUB_ISSUES_TOKEN</code>)
              </span>
            </label>
            {stubFailed && (
              <label className="guided-check">
                <input
                  checked={forceRerun}
                  onChange={(event) => setForceRerun(event.target.checked)}
                  type="checkbox"
                />
                <span>
                  Re-run with <code>--force</code> (replaces conflicting files at the
                  destination)
                </span>
              </label>
            )}
          </div>
          <button
            className="reconnect-button"
            disabled={busy !== null}
            onClick={() =>
              void runAction(
                "stub-sample",
                () =>
                  client.stubSample({
                    ...(workTracking.trim() !== ""
                      ? { workTracking: workTracking.trim() }
                      : {}),
                    ...(useMainToken ? { tokenEnv: "GOOBERS_GITHUB_TOKEN" } : {}),
                    ...(forceRerun ? { force: true } : {}),
                  }),
                setStubResult,
              )
            }
            type="button"
          >
            {busy === "stub-sample" ? "Materializing…" : "Materialize the sample"}
          </button>
          {stubResult && <StubSampleResult result={stubResult} />}
        </GuidedStep>

        <GuidedStep
          index={2}
          manual
          status={stepStatus(2)}
          title="Create the disposable GitHub repo & push"
        >
          <p>
            <strong>Manual step.</strong> The sample needs a remote to open pull requests
            against. This repository is yours and disposable — Goobers never creates remotes,
            never pushes, and never touches a repository you did not explicitly name. Run
            these in a terminal (pick any owner/repo you own):
          </p>
          <ol className="guided-commands">
            <li>
              <code>gh repo create &lt;owner&gt;/&lt;repo&gt; --private</code>
            </li>
            <li>
              <code>git -C {samplePath} init</code>
            </li>
            <li>
              <code>git -C {samplePath} add .</code>
            </li>
            <li>
              <code>git -C {samplePath} commit -m &quot;getting-started sample&quot;</code>
            </li>
            <li>
              <code>git -C {samplePath} branch -M main</code>
            </li>
            <li>
              <code>
                git -C {samplePath} remote add origin
                https://github.com/&lt;owner&gt;/&lt;repo&gt;.git
              </code>
            </li>
            <li>
              <code>git -C {samplePath} push -u origin main</code>
            </li>
          </ol>
          <p className="guided-note">
            Then either enter the owner/repo in step 2 above and re-run seeding to create the
            starter issues there, or continue — you can seed later.
          </p>
          <label className="guided-check">
            <input
              checked={pushDone}
              onChange={(event) => setPushDone(event.target.checked)}
              type="checkbox"
            />
            <span>I created the repository and pushed the sample</span>
          </label>
        </GuidedStep>

        <GuidedStep index={3} status={stepStatus(3)} title="Initialize the tutorial instance">
          <p>
            Creates the tutorial instance from the quickstart template at{" "}
            <code>{instancePath}</code>.
          </p>
          <RecoveryCommand command={`goobers init --template=quickstart ${instancePath}`} />
          <button
            className="reconnect-button"
            disabled={busy !== null}
            onClick={() =>
              void runAction("init", () => client.initInstance(), setInitResult)
            }
            type="button"
          >
            {busy === "init" ? "Initializing…" : "Initialize the instance"}
          </button>
          {initResult && initResult.exitCode !== 0 && (
            <GuidedStderr label="init failed" text={initResult.stderr || initResult.stdout} />
          )}
          {initDone && (
            <p className="guided-note">
              <strong>What you just created:</strong> <code>instance.yaml</code> and the{" "}
              <code>config/</code> tree are the instance's declarative desired state — which
              repositories it works on, which gaggles, workflows, and goobers exist. The
              daemon reconciles running state against these files; they are the single source
              of truth. Edits are plain file edits, reviewable and versionable like any other
              code.
            </p>
          )}
        </GuidedStep>

        <GuidedStep index={4} manual status={stepStatus(4)} title="Point it at your repo">
          <p>
            <strong>Manual step.</strong> The quickstart template ships with{" "}
            <code>your-org/your-repo</code> placeholders. Edit them to name the repository
            you created in step 3:
          </p>
          <ul className="guided-checklist">
            <li>
              <code>tutorial-instance/instance.yaml</code> — set{" "}
              <code>repos[0].owner</code> (<code>your-org</code>) and{" "}
              <code>repos[0].name</code> (<code>your-repo</code>).
            </li>
            <li>
              <code>tutorial-instance/config/gaggles/example/gaggle.yaml</code> — set{" "}
              <code>spec.project.owner</code>/<code>spec.project.name</code> and{" "}
              <code>spec.backlog.project</code> (<code>your-org/your-repo</code>).
            </li>
            <li>
              Make sure <code>GOOBERS_GITHUB_TOKEN</code> is exported in the shell that
              launched <code>goobers getting-started</code> — the instance's token ref reads
              it from the environment.
            </li>
          </ul>
          <label className="guided-check">
            <input
              checked={placeholdersDone}
              onChange={(event) => setPlaceholdersDone(event.target.checked)}
              type="checkbox"
            />
            <span>I edited the placeholders and exported the token</span>
          </label>
        </GuidedStep>

        <GuidedStep index={5} status={stepStatus(5)} title="Check everything">
          <p>
            Runs the full diagnostic pass over the tutorial instance: config validity, the
            harness toolchain, and reachability of your repository.
          </p>
          <RecoveryCommand
            command={`goobers validate --json --check-harness --check-repos ${instancePath}`}
          />
          <button
            className="reconnect-button"
            disabled={busy !== null}
            onClick={() =>
              void runAction(
                "validate",
                () => client.validate({ checkHarness: true, checkRepos: true }),
                (result) => {
                  setValidateResult(result);
                  setValidateOk(result.exitCode === 0);
                },
              )
            }
            type="button"
          >
            {busy === "validate" ? "Checking…" : "Run the checks"}
          </button>
          {validateResult && <ValidateResult result={validateResult} />}
        </GuidedStep>

        <GuidedStep
          index={6}
          status={stepStatus(6)}
          title="Run your first autonomous workflow"
        >
          <p>
            Starts one quickstart workflow run: an agent picks up a starter issue, implements
            it, reviews it, runs CI, and opens a pull request on your repository.
          </p>
          <RecoveryCommand command={`goobers run quickstart ${instancePath}`} />
          <button
            className="reconnect-button"
            disabled={busy !== null || (job !== null && !job.done)}
            onClick={() =>
              void runAction(
                "run",
                () => client.startRun(),
                ({ jobId }) => {
                  setJobDetail(null);
                  setStatusResult(null);
                  setActiveJobId(jobId);
                },
              )
            }
            type="button"
          >
            {job !== null && !job.done
              ? "Running…"
              : runFailed
                ? "Retry the run"
                : "Start the run"}
          </button>
          {job !== null && (
            <RunProgress detail={jobDetail} failed={runFailed} job={job} />
          )}
        </GuidedStep>

        <GuidedStep index={7} status={runDone ? "active" : "pending"} title="Success">
          {runDone ? (
            <SuccessStep
              instancePath={instancePath}
              runId={job?.runId ?? null}
              status={statusResult}
            />
          ) : (
            <p className="guided-note">
              Finishes the walkthrough once your first run completes: your local
              Time-to-First-PR readout and where to go next.
            </p>
          )}
        </GuidedStep>
      </ol>
    </>
  );
}

function GuidedStep({
  children,
  index,
  manual = false,
  status,
  title,
}: {
  children: React.ReactNode;
  index: number;
  manual?: boolean;
  status: StepStatus;
  title: string;
}) {
  return (
    <li className="guided-step content-section" data-state={status}>
      <div className="guided-step-header">
        <span aria-hidden="true" className="guided-step-index">
          {index + 1}
        </span>
        <h2>{title}</h2>
        {manual && <span className="guided-manual-mark">manual</span>}
        <span className={`guided-step-state guided-step-state-${status}`}>
          {stepStatusLabel[status]}
        </span>
      </div>
      <div className="guided-step-body">{children}</div>
    </li>
  );
}

const stepStatusLabel: Record<StepStatus, string> = {
  pending: "Pending",
  active: "Active",
  done: "Done",
  failed: "Failed",
};

function EnvBadge({ name, present }: { name: string; present: boolean }) {
  return (
    <span
      className={present ? "guided-badge guided-badge-set" : "guided-badge guided-badge-missing"}
    >
      <code>{name}</code> {present ? "set" : "not set"}
    </span>
  );
}

function stubSampleCommand(
  samplePath: string,
  workTracking: string,
  useMainToken: boolean,
  force: boolean,
): string {
  let command = `goobers onboarding stub-sample --destination ${samplePath} --json`;
  if (workTracking.trim() !== "") {
    command += ` --work-tracking ${workTracking.trim()}`;
  }
  if (useMainToken) {
    command += " --token-env GOOBERS_GITHUB_TOKEN";
  }
  if (force) {
    command += " --force";
  }
  return command;
}

const pendingIssuePattern = /^issue:(.+) \(pending: (.+)\)$/;

function StubSampleResult({
  result,
}: {
  result: GuidedEnvelopeResult<OnboardingActionEnvelope>;
}) {
  if (result.exitCode !== 0) {
    return (
      <GuidedStderr
        label="stub-sample failed (likely a conflicting file at the destination)"
        text={result.stderr || "The sample was not materialized."}
      />
    );
  }
  const created = result.envelope?.created ?? [];
  const skipped = result.envelope?.skipped ?? [];
  const pending = skipped
    .map((entry) => pendingIssuePattern.exec(entry))
    .filter((match): match is RegExpExecArray => match !== null);
  const plainSkipped = skipped.filter((entry) => !pendingIssuePattern.test(entry));
  return (
    <div className="guided-result">
      {created.length > 0 && (
        <div>
          <p className="section-kicker">Created</p>
          <ul className="guided-entry-list">
            {created.map((entry) => (
              <li key={entry}>
                <code>{entry}</code>
              </li>
            ))}
          </ul>
        </div>
      )}
      {plainSkipped.length > 0 && (
        <div>
          <p className="section-kicker">Skipped (already present)</p>
          <ul className="guided-entry-list">
            {plainSkipped.map((entry) => (
              <li key={entry}>
                <code>{entry}</code>
              </li>
            ))}
          </ul>
        </div>
      )}
      {pending.length > 0 && (
        <div>
          <p className="section-kicker">Pending seeds</p>
          <ul className="guided-entry-list">
            {pending.map((match) => (
              <li className="guided-pending" key={match[0]}>
                <code>issue:{match[1]}</code>
                <span className="guided-pending-reason">pending: {match[2]}</span>
              </li>
            ))}
          </ul>
          <p className="guided-note">
            Pending is not an error: these starter issues were held back (usually because no
            seeding token was available or no work-tracking repo was named). Export the token
            or name the repo and re-run this step to seed them.
          </p>
        </div>
      )}
    </div>
  );
}

function ValidateResult({ result }: { result: GuidedEnvelopeResult<DiagnosticsEnvelope> }) {
  const findings = result.envelope?.findings ?? [];
  if (result.exitCode === 0) {
    return (
      <div className="guided-result guided-all-clear" role="status">
        <p>
          <strong>All systems go.</strong> Configuration, harness, and repository checks
          passed
          {findings.length > 0 ? ` with ${findings.length} advisory finding(s) below.` : "."}
        </p>
        {findings.length > 0 && <FindingsTable findings={findings} />}
      </div>
    );
  }
  return (
    <div className="guided-result">
      {findings.length > 0 ? (
        <FindingsTable findings={findings} />
      ) : (
        <GuidedStderr label="validate failed" text={result.stderr || "No findings returned."} />
      )}
    </div>
  );
}

function FindingsTable({ findings }: { findings: DiagnosticsFinding[] }) {
  return (
    <div aria-label="Validation findings" className="data-table guided-findings" role="table">
      <div className="data-header guided-findings-row" role="row">
        <span role="columnheader">Severity</span>
        <span role="columnheader">Code</span>
        <span role="columnheader">File</span>
        <span role="columnheader">Message</span>
      </div>
      {findings.map((finding, index) => (
        <div
          className="data-row guided-findings-row"
          key={`${finding.code}-${finding.file}-${index}`}
          role="row"
        >
          <span role="cell">{finding.severity}</span>
          <span className="mono" role="cell">
            {finding.code}
          </span>
          <span className="mono" role="cell">
            {finding.file}
          </span>
          <span role="cell">{finding.message}</span>
        </div>
      ))}
    </div>
  );
}

function RunProgress({
  detail,
  failed,
  job,
}: {
  detail: GuidedJobDetail | null;
  failed: boolean;
  job: { done: boolean; exitCode: number | null; runId: string | null };
}) {
  const output = detail?.output ?? [];
  const stages = useMemo(() => runStageStates(output), [output]);
  return (
    <div className="guided-result">
      {job.runId && (
        <p className="guided-run-link">
          <a href={`#/run/${encodeURIComponent(job.runId)}`}>
            Watch run {job.runId} live →
          </a>
        </p>
      )}
      {stages && (
        <div aria-label="Run stage progress" className="guided-stage-row">
          {knownRunStages.map((stage) => (
            <span
              className={`guided-stage guided-stage-${stages[stage]}`}
              data-state={stages[stage]}
              key={stage}
            >
              {stage}
            </span>
          ))}
        </div>
      )}
      {output.length > 0 && <OutputTail lines={output} />}
      {failed && (
        <p className="guided-note guided-failed-note" role="alert">
          The run exited with code {job.exitCode}. Inspect the output above, fix what it
          points at (most often the placeholder edits in step 5 or the token export in step
          1), and retry.
        </p>
      )}
    </div>
  );
}

function runStageStates(
  output: string[],
): Record<(typeof knownRunStages)[number], "pending" | "running" | "done"> | null {
  const states = Object.fromEntries(knownRunStages.map((stage) => [stage, "pending"])) as Record<
    (typeof knownRunStages)[number],
    "pending" | "running" | "done"
  >;
  let sawAny = false;
  for (const line of output) {
    const match = /^stage (\S+) (started|finished)\b/.exec(line);
    if (!match) {
      continue;
    }
    const stage = match[1] as (typeof knownRunStages)[number];
    if (!(stage in states)) {
      continue;
    }
    sawAny = true;
    states[stage] = match[2] === "finished" ? "done" : "running";
  }
  return sawAny ? states : null;
}

/** The live output tail, autoscrolled to the newest line. */
function OutputTail({ lines }: { lines: string[] }) {
  const pre = useRef<HTMLPreElement>(null);
  useEffect(() => {
    if (pre.current) {
      pre.current.scrollTop = pre.current.scrollHeight;
    }
  }, [lines]);
  return (
    <pre className="code-block guided-output" ref={pre}>
      {lines.join("\n")}
    </pre>
  );
}

function SuccessStep({
  instancePath,
  runId,
  status,
}: {
  instancePath: string;
  runId: string | null;
  status: GuidedEnvelopeResult<StatusEnvelope> | null;
}) {
  const milliseconds = status?.envelope?.timeToFirstPR?.milliseconds;
  return (
    <div className="guided-result">
      <p>
        {typeof milliseconds === "number" ? (
          <strong>Your first autonomous PR opened in {formatElapsed(milliseconds)}.</strong>
        ) : (
          <strong>Your first autonomous run finished.</strong>
        )}{" "}
        Time-to-First-PR is computed locally by <code>goobers status --json</code> from this
        instance's own journal — it is a first-run success indicator for you, and it never
        leaves this machine.
      </p>
      <RecoveryCommand command={`goobers status --json ${instancePath}`} />
      {runId && (
        <p className="guided-run-link">
          <a href={`#/run/${encodeURIComponent(runId)}`}>Revisit run {runId} →</a>
        </p>
      )}
      <p className="section-kicker">Next steps</p>
      <ul className="guided-checklist">
        <li>
          <code>goobers dashboard</code> — the operational portal over any instance.
        </li>
        <li>
          <code>docs/guides/quickstart.md</code> — graduating from the disposable sample to
          production examples on repositories you own.
        </li>
        <li>
          <code>goobers agent-kit install</code> — install the agent toolkit into your own
          config source.
        </li>
      </ul>
    </div>
  );
}

function formatElapsed(milliseconds: number): string {
  const minutes = Math.round(milliseconds / 60_000);
  if (minutes < 1) {
    return `${Math.max(1, Math.round(milliseconds / 1_000))} seconds`;
  }
  return minutes === 1 ? "1 minute" : `${minutes} minutes`;
}

function GuidedStderr({ label, text }: { label: string; text: string }) {
  return (
    <div className="guided-result" role="alert">
      <p className="guided-failed-note">
        <strong>{label}</strong>
      </p>
      <pre className="code-block guided-output">{text}</pre>
    </div>
  );
}

/** A sessionStorage-persisted boolean for UI-only progress bits. Server truth
 *  from /guided/state always wins over these where both exist. */
function useSessionFlag(key: string): [boolean, (value: boolean) => void] {
  const [value, setValue] = useState<boolean>(() => {
    try {
      return window.sessionStorage.getItem(key) === "true";
    } catch {
      return false;
    }
  });
  const update = useCallback(
    (next: boolean) => {
      setValue(next);
      try {
        window.sessionStorage.setItem(key, String(next));
      } catch {
        // Storage unavailable: the flag just won't survive a reload.
      }
    },
    [key],
  );
  return [value, update];
}
