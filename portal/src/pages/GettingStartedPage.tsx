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
  type GuidedProbeResult,
  type GuidedState,
  type OnboardingActionEnvelope,
  type StatusEnvelope,
} from "../guided/client";

// The guided walkthrough (#437), extended with the connect-your-repository
// path (#2449, onboarding ladder §3 R3). Every write action on this page is a
// thin wrapper over a documented CLI command executed by the getting-started
// server; the exact command is always shown next to the button. Manual steps
// — creating the disposable GitHub repository, pushing the sample, editing
// placeholders, exporting tokens — are presented explicitly and are never
// performed on the user's behalf.

const defaultClient = new GuidedClient();

const statePollIntervalMs = 5_000;
const jobPollIntervalMs = 2_000;

/** Per-branch workflow stage names, mapped to a best-effort mini progress row
 *  by grepping run output for the CLI's stage transition lines. Absence of
 *  any match simply hides the row. */
const quickstartRunStages = [
  "query-backlog",
  "implement",
  "review",
  "local-ci",
  "push-branch",
  "open-pr",
] as const;

const defaultImplementRunStages = [
  "query-backlog",
  "implement",
  "push-branch",
  "open-pr",
] as const;

type GuidedQuery =
  | { status: "loading" }
  | { status: "unavailable" }
  | { status: "ready"; state: GuidedState };

type StepStatus = "pending" | "active" | "done" | "failed";

type BusyAction = "stub-sample" | "init" | "connect" | "validate" | "run" | null;

/** The two rungs of the path chooser: connect the repository you already work
 *  in (the spine, PO ruling), or the disposable sample tutorial. */
type GuidedPath = "own-repo" | "sample";

const guidedPathStorageKey = "goobers-guided-path";
const defaultConnectTokenEnv = "GOOBERS_GITHUB_TOKEN";
const repoShapePattern = /^[A-Za-z0-9](?:[A-Za-z0-9._-]*)\/[A-Za-z0-9._-]+$/;

export function GettingStartedPage({ client = defaultClient }: { client?: GuidedClient } = {}) {
  const [query, setQuery] = useState<GuidedQuery>({ status: "loading" });

  // UI-only progress bits (which manual steps the user confirmed, whether the
  // welcome step was acknowledged, which path was chosen) live in
  // sessionStorage; everything the server can attest to (sample/instance
  // existence, the connected repository, job state, env tokens) comes from
  // /guided/state and wins over anything stored here.
  const [welcomeDone, setWelcomeDone] = useSessionFlag("goobers-guided-welcome-done");
  const [pushDone, setPushDone] = useSessionFlag("goobers-guided-push-done");
  const [placeholdersDone, setPlaceholdersDone] = useSessionFlag("goobers-guided-placeholders-done");
  const [tokenExported, setTokenExported] = useSessionFlag("goobers-guided-token-exported");
  const [validateOk, setValidateOk] = useSessionFlag("goobers-guided-validate-ok");
  const [storedPath, setStoredPath] = useSessionValue(guidedPathStorageKey);

  const [busy, setBusy] = useState<BusyAction>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const [stubResult, setStubResult] =
    useState<GuidedEnvelopeResult<OnboardingActionEnvelope> | null>(null);
  const [initResult, setInitResult] = useState<GuidedInitResult | null>(null);
  const [connectResult, setConnectResult] =
    useState<GuidedEnvelopeResult<OnboardingActionEnvelope> | null>(null);
  const [validateResult, setValidateResult] =
    useState<GuidedEnvelopeResult<DiagnosticsEnvelope> | null>(null);
  const [statusResult, setStatusResult] =
    useState<GuidedEnvelopeResult<StatusEnvelope> | null>(null);
  // #2638: the sample path's read-only pre-run eligibility probe. null means
  // "not checked yet (or reset for a fresh check)" — distinct from a probe
  // result whose own eligibleCount is null (no issues token exported yet).
  const [probeResult, setProbeResult] = useState<GuidedProbeResult | null>(null);
  const [probeChecking, setProbeChecking] = useState(false);

  const [workTracking, setWorkTracking] = useState("");
  const [useMainToken, setUseMainToken] = useState(false);
  const [forceRerun, setForceRerun] = useState(false);

  const [connectRepo, setConnectRepo] = useState("");
  const [connectTokenEnv, setConnectTokenEnv] = useState(defaultConnectTokenEnv);
  const [connectSeed, setConnectSeed] = useState(true);
  const [connectReplace, setConnectReplace] = useState(false);

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

  // Server truth is polled: sample/instance existence and the connected
  // repository reflect the filesystem on every poll. The env-token badges do
  // NOT — they read the getting-started server's own process environment,
  // fixed at server launch (#2639). A token exported in any terminal after
  // the server started, including the one that launched it, never reaches
  // this process; only exporting before launch (or restarting the server
  // after exporting) changes what these badges report.
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
  const connectedRepo = serverState?.connected?.repo ?? null;

  // The chosen path: the stored choice wins (switching stays possible after a
  // connect); with nothing stored, server truth infers it on reload — a
  // connected repository means the own-repo path, an existing sample means
  // the tutorial path.
  const path: GuidedPath | null =
    storedPath === "own-repo" || storedPath === "sample"
      ? storedPath
      : connectedRepo !== null
        ? "own-repo"
        : (serverState?.sampleExists ?? false)
          ? "sample"
          : null;

  // #2638: a run that exits 0 only means the workflow reached PhaseCompleted
  // — the SAME terminal phase a genuine no-eligible-work backlog-query tick
  // short-circuits to (issue #233's shared runner/backlog-query contract,
  // untouched here). exitCode alone cannot distinguish "opened a PR" from
  // "found nothing to do", so this reads the same stage-transition lines
  // RunProgress already greps for progress display: if the terminal
  // "open-pr" stage never started, query-backlog (or whatever gated it)
  // never handed off — nothing was implemented or opened, regardless of the
  // clean exit.
  const runStages = path === "own-repo" ? defaultImplementRunStages : quickstartRunStages;
  const runOutput = jobDetail?.output ?? [];
  const runStageProgress = useMemo(
    () => runStageStates(runOutput, runStages),
    [runOutput, runStages],
  );
  const runOutcome: "success" | "no-work" | "failed" | null =
    job === null || !job.done
      ? null
      : job.exitCode !== 0
        ? "failed"
        : runStageProgress?.["open-pr"] === "pending" || runStageProgress?.["open-pr"] === undefined
          ? "no-work"
          : "success";
  const runFinishedOk = runOutcome === "success" || runOutcome === "no-work";
  const runFailed = runOutcome === "failed";

  // Success step: the Time-to-First-PR readout is computed by `goobers status
  // --json` from local journal timestamps. It is a local number — nothing on
  // this page reports it (or anything else) anywhere. Only fetched on an
  // actual success — a no-work run has no first PR to time.
  useEffect(() => {
    if (runOutcome !== "success" || statusResult !== null) {
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
  }, [runOutcome, statusResult, client]);

  const sampleDone = (serverState?.sampleExists ?? false) || stubResult?.exitCode === 0;
  const initDone = serverState?.instanceExists ?? false;
  const stubFailed = stubResult !== null && stubResult.exitCode !== 0 && !sampleDone;
  const initFailed = initResult !== null && initResult.exitCode !== 0 && !initDone;
  const connectDone = connectedRepo !== null || connectResult?.exitCode === 0;
  const connectFailed = connectResult !== null && connectResult.exitCode !== 0 && !connectDone;
  const validateFailed = validateResult !== null && validateResult.exitCode !== 0;
  const validateDone = validateOk && !validateFailed;

  // #2638: a read-only "how many eligible issues are there right now" check,
  // run before the sample quickstart's Run button is used — surfacing "0
  // eligible issues" as a pre-run warning instead of letting the user
  // discover it only after a run completes with nothing to show. Sample
  // path only (own-repo's label conventions are the user's own, out of
  // scope here — see the server-side handler's comment).
  const checkBacklogProbe = useCallback(async () => {
    setProbeChecking(true);
    try {
      const result = await client.probeBacklog();
      setProbeResult(result);
    } catch {
      // A failed probe is a soft warning, not a blocking error — leave
      // whatever was there (nothing, on a first check) and let the user
      // just start the run if they want to.
    } finally {
      setProbeChecking(false);
    }
  }, [client]);

  useEffect(() => {
    if (
      path !== "sample" ||
      !validateDone ||
      probeResult !== null ||
      probeChecking ||
      (job !== null && !job.done)
    ) {
      return;
    }
    void checkBacklogProbe();
  }, [path, validateDone, probeResult, probeChecking, job, checkBacklogProbe]);

  const choosePath = (next: GuidedPath) => {
    if (path === next) {
      if (storedPath !== next) {
        setStoredPath(next);
      }
      return;
    }
    // Switching resets only the branch-specific steps' UI state — action
    // results and manual acknowledgements. Server truth (instance/sample
    // existence, the connected repo, job state) still governs the done marks.
    setStubResult(null);
    setInitResult(null);
    setConnectResult(null);
    setValidateResult(null);
    setStatusResult(null);
    setValidateOk(false);
    setPushDone(false);
    setPlaceholdersDone(false);
    setTokenExported(false);
    setForceRerun(false);
    setConnectReplace(false);
    setStoredPath(next);
  };

  const chooserDone = path !== null;
  const tokenEnvName =
    connectTokenEnv.trim() === "" ? defaultConnectTokenEnv : connectTokenEnv.trim();
  const tokenDone =
    tokenExported ||
    (tokenEnvName === defaultConnectTokenEnv &&
      (serverState?.env.goobersGithubToken ?? false));

  // Step done/failed flags per branch, in render order. The success step is
  // rendered explicitly from runOutcome, as before (it also needs to tell
  // "no PR opened" apart from "opened one", which a single boolean can't).
  const doneFlags =
    path === "own-repo"
      ? [welcomeDone, chooserDone, initDone, connectDone, tokenDone, validateDone, runFinishedOk, false]
      : path === "sample"
        ? [
            welcomeDone,
            chooserDone,
            sampleDone,
            pushDone,
            initDone,
            placeholdersDone,
            validateDone,
            runFinishedOk,
            false,
          ]
        : [welcomeDone, false];
  const failedFlags =
    path === "own-repo"
      ? [false, false, initFailed, connectFailed, false, validateFailed, runFailed, false]
      : path === "sample"
        ? [false, false, stubFailed, false, false, false, validateFailed, runFailed, false]
        : [false, false];
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

  const repoShapeValid = repoShapePattern.test(connectRepo.trim());

  const connectCommand = (() => {
    let command = `goobers connect ${connectRepo.trim() || "<owner>/<repo>"} --json`;
    if (tokenEnvName !== defaultConnectTokenEnv) {
      command += ` --token-env ${tokenEnvName}`;
    }
    if (connectSeed) {
      command += " --seed";
    }
    if (connectReplace) {
      command += " --replace";
    }
    return `${command} ${instancePath}`;
  })();

  // The validate, run, and success steps are shared between the two branches
  // (only their index, workflow name, and stage vocabulary differ).
  const validateStep = (index: number) => (
    <GuidedStep index={index} status={stepStatus(index)} title="Check everything">
      <p>
        Runs the full diagnostic pass over the instance: config validity, the harness
        toolchain, and reachability of your repository.
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
  );

  const startRun = (workflow: "quickstart" | "default-implement") =>
    void runAction(
      "run",
      () => client.startRun({ workflow }),
      ({ jobId }) => {
        setJobDetail(null);
        setStatusResult(null);
        // Reset so the next completion re-triggers the eligibility probe
        // effect above — eligible issues can change between runs (e.g. the
        // user just labeled one in response to a "0 eligible" warning).
        setProbeResult(null);
        setActiveJobId(jobId);
      },
    );

  const runStep = (
    index: number,
    workflow: "quickstart" | "default-implement",
    stages: readonly string[],
    description: string,
    probeSection?: React.ReactNode,
  ) => (
    <GuidedStep index={index} status={stepStatus(index)} title="Run your first autonomous workflow">
      <p>{description}</p>
      {probeSection}
      <RecoveryCommand command={`goobers run ${workflow} ${instancePath}`} />
      <button
        className="reconnect-button"
        disabled={busy !== null || (job !== null && !job.done)}
        onClick={() => startRun(workflow)}
        type="button"
      >
        {job !== null && !job.done
          ? "Running…"
          : runOutcome === "failed"
            ? "Retry the run"
            : runOutcome === "no-work"
              ? "Run again"
              : "Start the run"}
      </button>
      {job !== null && (
        <RunProgress detail={jobDetail} failed={runFailed} job={job} stages={stages} />
      )}
    </GuidedStep>
  );

  const successStep = (
    index: number,
    variant: GuidedPath,
    workflow: "quickstart" | "default-implement",
  ) => (
    <GuidedStep
      index={index}
      status={runOutcome === "success" || runOutcome === "no-work" ? "active" : "pending"}
      title="Success"
    >
      {runOutcome === "success" ? (
        <SuccessStep
          instancePath={instancePath}
          runId={job?.runId ?? null}
          status={statusResult}
          variant={variant}
        />
      ) : runOutcome === "no-work" ? (
        <NoWorkStep
          disabled={busy !== null}
          onRerun={() => startRun(workflow)}
          runId={job?.runId ?? null}
        />
      ) : (
        <p className="guided-note">
          Finishes the walkthrough once your first run completes: your local
          Time-to-First-PR readout and where to go next.
        </p>
      )}
    </GuidedStep>
  );

  return (
    <>
      <header className="page-heading">
        <p className="page-kicker">Guided onboarding</p>
        <h1>Getting Started</h1>
        <p>
          From an empty folder to your first autonomous pull request — against the repository
          you already work in, or a disposable sample. Every button below runs the exact CLI
          command shown beside it — this page is a wrapper over the CLI, nothing more.
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
            This walkthrough runs one autonomous workflow that ends in a real pull request.
            Everything this page does is the printed CLI command shown in each step's chip;
            the manual steps are called out explicitly and stay yours.
          </p>
          <ul className="guided-checklist">
            <li>A GitHub account and a repository to work against.</li>
            <li>Copilot CLI installed and signed in.</li>
            <li>
              <code>export GOOBERS_GITHUB_TOKEN=...</code> — and optionally{" "}
              <code>GOOBERS_GITHUB_ISSUES_TOKEN</code> for seeding starter issues.
            </li>
            <li>
              Connecting your own repository? Its own workflow determines any further
              tooling it needs — the checks step below (<code>goobers validate
              --check-harness --check-repos</code>) confirms what your connected instance
              actually requires.
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
            Token badges reflect the getting-started server's own environment as of when it
            launched — not this machine's environment generally, and not anything exported
            afterward. Export before running <code>goobers getting-started</code>, or restart
            it after exporting. Token values never reach this page — only whether each
            variable is set.
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

        <GuidedStep index={1} status={stepStatus(1)} title="Choose your path">
          <p>
            Two ways through. You can switch later — switching resets only this page's step
            state for the branch; nothing on disk is touched, and what the server already
            attests to stays done.
          </p>
          <div aria-label="Path chooser" className="guided-chooser" role="group">
            <button
              aria-pressed={path === "own-repo"}
              className="guided-chooser-card"
              data-selected={path === "own-repo"}
              onClick={() => choosePath("own-repo")}
              type="button"
            >
              <span className="guided-chooser-recommended">Recommended</span>
              <span className="guided-chooser-title">Connect your repository</span>
              <span className="guided-chooser-copy">
                Your repo, your issues, a real first PR.
              </span>
            </button>
            <button
              aria-pressed={path === "sample"}
              className="guided-chooser-card"
              data-selected={path === "sample"}
              onClick={() => choosePath("sample")}
              type="button"
            >
              <span className="guided-chooser-title">Try the disposable sample</span>
              <span className="guided-chooser-copy">
                A zero-stakes tutorial against a throwaway repo.
              </span>
            </button>
          </div>
        </GuidedStep>

        {path === "own-repo" && (
          <>
            <GuidedStep index={2} status={stepStatus(2)} title="Initialize a starter instance">
              <p>
                Creates a starter instance at <code>{instancePath}</code>: one coder goober
                driving the <code>default-implement</code> workflow — claim an issue,
                implement it, push a branch, open a PR.
              </p>
              <RecoveryCommand command={`goobers init ${instancePath}`} />
              <button
                className="reconnect-button"
                disabled={busy !== null}
                onClick={() =>
                  void runAction(
                    "init",
                    () => client.initInstance({ template: "starter" }),
                    setInitResult,
                  )
                }
                type="button"
              >
                {busy === "init" ? "Initializing…" : "Initialize the starter instance"}
              </button>
              {initResult && initResult.exitCode !== 0 && (
                <GuidedStderr
                  label="init failed"
                  text={initResult.stderr || initResult.stdout}
                />
              )}
              {initDone && (
                <p className="guided-note">
                  <strong>What you just created:</strong> <code>instance.yaml</code> and the{" "}
                  <code>config/</code> tree are the instance's declarative desired state —
                  which repositories it works on, which gaggles, workflows, and goobers
                  exist. The daemon reconciles running state against these files; they are
                  the single source of truth.
                </p>
              )}
            </GuidedStep>

            <GuidedStep index={3} status={stepStatus(3)} title="Connect your repository">
              <p>
                Rewrites the starter placeholders to name your repository — in{" "}
                <code>instance.yaml</code> and the gaggle's project/backlog — then checks it
                is reachable. Only the token's environment variable <em>name</em> is
                recorded; the value itself never leaves your shell.
              </p>
              <RecoveryCommand command={connectCommand} />
              <div className="guided-fields">
                <label className="guided-field">
                  <span>Repository (owner/repo)</span>
                  <input
                    onChange={(event) => setConnectRepo(event.target.value)}
                    placeholder="your-org/your-repo"
                    type="text"
                    value={connectRepo}
                  />
                </label>
                <label className="guided-field">
                  <span>Token environment variable (a name, never the token value)</span>
                  <input
                    onChange={(event) => setConnectTokenEnv(event.target.value)}
                    placeholder={defaultConnectTokenEnv}
                    type="text"
                    value={connectTokenEnv}
                  />
                </label>
                <label className="guided-check">
                  <input
                    checked={connectSeed}
                    onChange={(event) => setConnectSeed(event.target.checked)}
                    type="checkbox"
                  />
                  <span>
                    Seed the backlog: creates the labels the workforce's backlog selector
                    actually filters on, plus one safe starter issue — or skip and label one
                    of <strong>your</strong> real issues with <code>goobers</code> instead.
                  </span>
                </label>
                {connectResult !== null && connectResult.exitCode !== 0 && (
                  <label className="guided-check">
                    <input
                      checked={connectReplace}
                      onChange={(event) => setConnectReplace(event.target.checked)}
                      type="checkbox"
                    />
                    <span>
                      Re-run with <code>--replace</code> (rewrites a repository that is
                      already connected)
                    </span>
                  </label>
                )}
              </div>
              <button
                className="reconnect-button"
                disabled={busy !== null || !repoShapeValid}
                onClick={() =>
                  void runAction(
                    "connect",
                    () =>
                      client.connect({
                        repo: connectRepo.trim(),
                        ...(tokenEnvName !== defaultConnectTokenEnv
                          ? { tokenEnv: tokenEnvName }
                          : {}),
                        ...(connectSeed ? { seed: true } : {}),
                        ...(connectReplace ? { replace: true } : {}),
                      }),
                    setConnectResult,
                  )
                }
                type="button"
              >
                {busy === "connect" ? "Connecting…" : "Connect the repository"}
              </button>
              {connectRepo.trim() !== "" && !repoShapeValid && (
                <p className="guided-note">
                  Enter the repository as <code>owner/repo</code>.
                </p>
              )}
              {connectedRepo !== null && (
                <p className="guided-note">
                  Connected to <code>{connectedRepo}</code>.
                </p>
              )}
              {connectResult && <ConnectResult result={connectResult} />}
            </GuidedStep>

            <GuidedStep index={4} manual status={stepStatus(4)} title="Export the token">
              <p>
                <strong>Manual step.</strong> The instance reads your GitHub token from{" "}
                <code>{state.env.tokenEnv}</code>, the environment variable named at connect
                time — but only from the getting-started server's OWN process, fixed at
                launch. Export it in the shell that will LAUNCH{" "}
                <code>goobers getting-started</code>, then (re)start that command; exporting
                in the already-running shell, or any other shell, does not reach this
                process. Only presence is checked — the value never reaches this page.
              </p>
              <RecoveryCommand command={`export ${state.env.tokenEnv}=...`} />
              <p className="guided-note">
                Then relaunch: <code>goobers getting-started</code> resumes this walkthrough
                from the instance you already created.
              </p>
              <div aria-label="Connect token status" className="guided-badges">
                <EnvBadge
                  name={state.env.tokenEnv}
                  present={state.env.goobersGithubToken}
                />
              </div>
              <label className="guided-check">
                <input
                  checked={tokenExported}
                  onChange={(event) => setTokenExported(event.target.checked)}
                  type="checkbox"
                />
                <span>I exported the token</span>
              </label>
            </GuidedStep>

            {validateStep(5)}
            {runStep(
              6,
              "default-implement",
              defaultImplementRunStages,
              "Starts one default-implement workflow run: your coder goober claims a labeled issue from your backlog, implements it, pushes a branch, and opens a pull request on your repository.",
            )}
            {successStep(7, "own-repo", "default-implement")}
          </>
        )}

        {path === "sample" && (
          <>
            <GuidedStep index={2} status={stepStatus(2)} title="Materialize the sample">
              <p>
                Writes the embedded <code>getting-started-task-api</code> sample to{" "}
                <code>{samplePath}</code>. With a work-tracking repo named, it also seeds the
                starter labels and issues there.
              </p>
              <p className="guided-note">
                The sample is a small Node.js/TypeScript project — its own CI expects
                Node.js &gt;= 20 and npm on <code>PATH</code> to build and test it. Nothing
                here enforces that for you; it's the sample's own tooling, not this
                walkthrough's.
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
              index={3}
              manual
              status={stepStatus(3)}
              title="Create the disposable GitHub repo & push"
            >
              <p>
                <strong>Manual step.</strong> The sample needs a remote to open pull requests
                against. This repository is yours and disposable — Goobers never creates
                remotes, never pushes, and never touches a repository you did not explicitly
                name. Run these in a terminal (pick any owner/repo you own):
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
                Then either enter the owner/repo in step 3 above and re-run seeding to create
                the starter issues there, or continue — you can seed later.
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

            <GuidedStep index={4} status={stepStatus(4)} title="Initialize the tutorial instance">
              <p>
                Creates the tutorial instance from the quickstart template at{" "}
                <code>{instancePath}</code>.
              </p>
              <RecoveryCommand command={`goobers init --template=quickstart ${instancePath}`} />
              <button
                className="reconnect-button"
                disabled={busy !== null}
                onClick={() =>
                  void runAction(
                    "init",
                    () => client.initInstance({ template: "quickstart" }),
                    setInitResult,
                  )
                }
                type="button"
              >
                {busy === "init" ? "Initializing…" : "Initialize the instance"}
              </button>
              {initResult && initResult.exitCode !== 0 && (
                <GuidedStderr
                  label="init failed"
                  text={initResult.stderr || initResult.stdout}
                />
              )}
              {initDone && (
                <p className="guided-note">
                  <strong>What you just created:</strong> <code>instance.yaml</code> and the{" "}
                  <code>config/</code> tree are the instance's declarative desired state —
                  which repositories it works on, which gaggles, workflows, and goobers
                  exist. The daemon reconciles running state against these files; they are
                  the single source of truth. Edits are plain file edits, reviewable and
                  versionable like any other code.
                </p>
              )}
            </GuidedStep>

            <GuidedStep index={5} manual status={stepStatus(5)} title="Point it at your repo">
              <p>
                <strong>Manual step.</strong> The quickstart template ships with{" "}
                <code>your-org/your-repo</code> placeholders. Edit them to name the repository
                you created in step 4:
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
                  Make sure <code>{state.env.tokenEnv}</code> is exported in the shell that
                  will LAUNCH <code>goobers getting-started</code>, then (re)start that
                  command — the instance's token ref reads it from the getting-started
                  server's own process environment, fixed at launch, not from any shell an
                  export happens in afterward.
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

            {validateStep(6)}
            {runStep(
              7,
              "quickstart",
              quickstartRunStages,
              "Starts one quickstart workflow run: an agent picks up a starter issue, implements it, reviews it, runs CI, and opens a pull request on your repository.",
              <BacklogProbeWarning
                checking={probeChecking}
                onRecheck={() => void checkBacklogProbe()}
                result={probeResult}
              />,
            )}
            {successStep(8, "sample", "quickstart")}
          </>
        )}
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

/** The created/updated/skipped lists of an onboarding action envelope, with
 *  pending seed entries (skipped `issue:<x> (pending: <why>)`) rendered as the
 *  established non-error pending state. */
function EnvelopeLists({ envelope }: { envelope: OnboardingActionEnvelope | null }) {
  const created = envelope?.created ?? [];
  const updated = envelope?.updated ?? [];
  const skipped = envelope?.skipped ?? [];
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
      {updated.length > 0 && (
        <div>
          <p className="section-kicker">Updated</p>
          <ul className="guided-entry-list">
            {updated.map((entry) => (
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
            seeding token was available). Export the token and re-run this step to seed them.
          </p>
        </div>
      )}
    </div>
  );
}

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
  return <EnvelopeLists envelope={result.envelope} />;
}

function ConnectResult({
  result,
}: {
  result: GuidedEnvelopeResult<OnboardingActionEnvelope>;
}) {
  if (result.exitCode !== 0) {
    return (
      <GuidedStderr
        label="connect refused"
        text={result.stderr || "The repository was not connected."}
      />
    );
  }
  return <EnvelopeLists envelope={result.envelope} />;
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
  stages,
}: {
  detail: GuidedJobDetail | null;
  failed: boolean;
  job: { done: boolean; exitCode: number | null; runId: string | null };
  stages: readonly string[];
}) {
  const output = detail?.output ?? [];
  const stageStates = useMemo(() => runStageStates(output, stages), [output, stages]);
  return (
    <div className="guided-result">
      {job.runId && (
        <p className="guided-run-link">
          <a href={`#/run/${encodeURIComponent(job.runId)}`}>
            Watch run {job.runId} live →
          </a>
        </p>
      )}
      {stageStates && (
        <div aria-label="Run stage progress" className="guided-stage-row">
          {stages.map((stage) => (
            <span
              className={`guided-stage guided-stage-${stageStates[stage]}`}
              data-state={stageStates[stage]}
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
          points at (most often the repository connection or the token export), and retry.
        </p>
      )}
    </div>
  );
}

function runStageStates(
  output: string[],
  stages: readonly string[],
): Record<string, "pending" | "running" | "done"> | null {
  const states = Object.fromEntries(stages.map((stage) => [stage, "pending"])) as Record<
    string,
    "pending" | "running" | "done"
  >;
  let sawAny = false;
  for (const line of output) {
    const match = /^stage (\S+) (started|finished)\b/.exec(line);
    if (!match) {
      continue;
    }
    const stage = match[1];
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

/** #2638: the read-only pre-run probe's result, rendered inside the Run step
 *  BEFORE the user clicks Start — distinct from NoWorkStep below, which
 *  reports what a run that already happened found (or didn't). Renders
 *  nothing when there's genuinely nothing to say yet: no check has run, or
 *  the check couldn't run because no issues token is exported (that's the
 *  earlier "export the token" step's job to flag, not this one's). */
function BacklogProbeWarning({
  checking,
  onRecheck,
  result,
}: {
  checking: boolean;
  onRecheck: () => void;
  result: GuidedProbeResult | null;
}) {
  if (result === null) {
    return checking ? <p className="guided-note">Checking for eligible issues…</p> : null;
  }
  if (result.eligibleCount === null) {
    return null;
  }
  if (result.eligibleCount > 0) {
    return (
      <p className="guided-note">
        {result.eligibleCount} eligible {result.eligibleCount === 1 ? "issue" : "issues"} found —
        ready to run.
      </p>
    );
  }
  return (
    <div className="guided-note guided-failed-note" role="alert">
      <p>
        <strong>0 eligible issues found.</strong> This workflow only claims issues labeled{" "}
        <code>goobers:approved</code> and <code>goobers:ready</code> — starting the run now would
        likely finish without opening a pull request. Label an issue (the sample repository
        includes one to label), then check again.
      </p>
      <button className="reconnect-button" disabled={checking} onClick={onRecheck} type="button">
        {checking ? "Checking…" : "Check again"}
      </button>
    </div>
  );
}

/** #2638: the run completed (exit 0) but never reached the "open-pr" stage —
 *  no eligible backlog item was found, so nothing was implemented or opened.
 *  Rendered instead of SuccessStep, never alongside it — a clean exit with
 *  no PR is not the "first autonomous PR" the walkthrough promises. */
function NoWorkStep({
  disabled,
  onRerun,
  runId,
}: {
  disabled: boolean;
  onRerun: () => void;
  runId: string | null;
}) {
  return (
    <div className="guided-result" role="status">
      <p>
        <strong>No eligible issues found.</strong> The run finished without an error, but no
        issue in your backlog carried the labels this workflow claims from — so nothing was
        implemented and no pull request was opened.
      </p>
      <p className="guided-note">
        Label an issue (or seed a new one), then re-run. Nothing else in the walkthrough needs
        to change.
      </p>
      <button className="reconnect-button" disabled={disabled} onClick={onRerun} type="button">
        Re-run
      </button>
      {runId && (
        <p className="guided-run-link">
          <a href={`#/run/${encodeURIComponent(runId)}`}>Inspect run {runId} →</a>
        </p>
      )}
    </div>
  );
}

function SuccessStep({
  instancePath,
  runId,
  status,
  variant,
}: {
  instancePath: string;
  runId: string | null;
  status: GuidedEnvelopeResult<StatusEnvelope> | null;
  variant: GuidedPath;
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
      {variant === "own-repo" ? (
        <ul className="guided-checklist">
          <li>
            Label more of your issues with <code>goobers</code> — the workforce keeps picking
            them up.
          </li>
          <li>
            Graduate to the flagship chain (merge-review + pr-remediation) via{" "}
            <code>config-examples</code>.
          </li>
          <li>
            <code>goobers agent-kit install</code> — install the agent toolkit into your own
            config source.
          </li>
          <li>
            <code>goobers dashboard</code> — the operational portal over any instance.
          </li>
        </ul>
      ) : (
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
      )}
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

/** A sessionStorage-persisted string (the chosen path). */
function useSessionValue(key: string): [string | null, (value: string) => void] {
  const [value, setValue] = useState<string | null>(() => {
    try {
      return window.sessionStorage.getItem(key);
    } catch {
      return null;
    }
  });
  const update = useCallback(
    (next: string) => {
      setValue(next);
      try {
        window.sessionStorage.setItem(key, next);
      } catch {
        // Storage unavailable: the value just won't survive a reload.
      }
    },
    [key],
  );
  return [value, update];
}
