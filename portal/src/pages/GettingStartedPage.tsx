import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { AnimatedGoober } from "../components/AnimatedGoober";
import { RecoveryCommand } from "../components/RecoveryAction";
import {
  GuidedClient,
  type DiagnosticsEnvelope,
  type GuidedInitOptions,
  type GuidedInitResult,
  type GuidedGitHubAuthorizationResult,
  type GuidedRepositoryInspection,
  type GuidedRepositoryReadiness,
  type GuidedState,
  type GuidedWorkflow,
} from "../guided/client";
import { Icon } from "../ui/Icon";

const defaultClient = new GuidedClient();
const statePollIntervalMs = 5_000;
const githubPATSettingsURL = "https://github.com/settings/personal-access-tokens/new";
/** Deadline for a single `/guided/state` read, so a getting-started server that
 *  stops answering cannot stall the poll loop indefinitely. */
const stateRequestTimeoutMs = 15_000;
type RepositorySource = "local" | "remote";
type QueryState =
  | { status: "loading" }
  | { status: "unavailable" }
  | { status: "ready"; state: GuidedState };
type BusyAction =
  | "browse"
  | "inspect"
  | "authorize"
  | "init"
  | "prepare"
  | "validate"
  | null;
type WizardPageId =
  | "welcome"
  | "repository"
  | "placement"
  | "workflows"
  | "issue-scope"
  | "runtime"
  | "review"
  | "repository-setup"
  | "validate"
  | "complete";

interface WizardPage {
  id: WizardPageId;
  label: string;
  step: number;
}

const wizardStepCount = 10;

const workflowChoices: Array<{
  id: GuidedWorkflow;
  title: string;
  description: string;
  outcome: string;
  image: string;
}> = [
  {
    id: "work-nomination",
    title: "Work nomination",
    description: "Uses code and operational signals to propose new issues for human approval.",
    outcome: "Adds a nominator goober.",
    image: "/workflow-nomination.png",
  },
  {
    id: "backlog-curation",
    title: "Backlog curation",
    description: "Turns approved issues into scoped work that is ready for implementation.",
    outcome: "Adds a curator goober.",
    image: "/workflow-curation.png",
  },
  {
    id: "implementation",
    title: "Implementation",
    description: "Takes ready issues through implementation, review, local CI, and a pull request.",
    outcome: "Adds implementer and reviewer goobers.",
    image: "/workflow-implementation.png",
  },
];

export function GettingStartedPage({ client = defaultClient }: { client?: GuidedClient } = {}) {
  const [query, setQuery] = useState<QueryState>({ status: "loading" });
  const [pageIndex, setPageIndex] = useSessionState("goobers-wizard-page", 0);
  const [repositorySource, setRepositorySource] = useSessionState<RepositorySource>(
    "goobers-wizard-repository-source",
    "local",
  );
  const [repo, setRepo] = useSessionState("goobers-wizard-repo", "");
  const [branch, setBranch] = useSessionState("goobers-wizard-branch", "main");
  const [instancePlacement, setInstancePlacement] = useSessionState<
    "peer" | "custom"
  >("goobers-wizard-instance-placement", "peer");
  const [instancePath, setInstancePath] = useSessionState(
    "goobers-wizard-instance-path",
    "",
  );
  const [workflows, setWorkflows] = useSessionState<GuidedWorkflow[]>(
    "goobers-wizard-workflows",
    ["work-nomination", "backlog-curation", "implementation"],
  );
  const [issueScope, setIssueScope] = useSessionState<"all" | "assigned">(
    "goobers-wizard-issue-scope",
    "all",
  );
  const [ciCommandText, setCICommandText] = useSessionState("goobers-wizard-ci", "");
  const [capability, setCapability] = useSessionState("goobers-wizard-capability", "");
  const [pullRequestCI, setPullRequestCI] = useSessionState(
    "goobers-wizard-pull-request-ci",
    false,
  );
  const [harness, setHarness] = useSessionState<"copilot" | "claude-code">(
    "goobers-wizard-harness",
    "copilot",
  );
  const [repoTokenEnv, setRepoTokenEnv] = useSessionState(
    "goobers-wizard-repo-token",
    "GOOBERS_GITHUB_REPO_TOKEN",
  );
  const [issuesTokenEnv, setIssuesTokenEnv] = useSessionState(
    "goobers-wizard-issues-token",
    "GOOBERS_GITHUB_ISSUES_TOKEN",
  );
  const [prTokenEnv, setPRTokenEnv] = useSessionState(
    "goobers-wizard-pr-token",
    "GOOBERS_GITHUB_PR_TOKEN",
  );
  const [pushTokenEnv, setPushTokenEnv] = useSessionState(
    "goobers-wizard-push-token",
    "GOOBERS_GITHUB_PUSH_TOKEN",
  );
  const [modelTokenEnv, setModelTokenEnv] = useSessionState(
    "goobers-wizard-model-token",
    "",
  );
  const [createStarterIssue, setCreateStarterIssue] = useSessionState(
    "goobers-wizard-create-starter-issue",
    true,
  );
  const [inspection, setInspection] = useState<GuidedRepositoryInspection | null>(null);
  const [repositoryReadiness, setRepositoryReadiness] =
    useState<GuidedRepositoryReadiness | null>(null);

  const [busy, setBusy] = useState<BusyAction>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [initResult, setInitResult] = useState<GuidedInitResult | null>(null);
  const [validateResult, setValidateResult] = useState<{
    exitCode: number;
    envelope: DiagnosticsEnvelope | null;
    stderr: string;
  } | null>(null);
  const [promptCopied, setPromptCopied] = useState(false);

  const statePass = useRef<{ generation: number; controller: AbortController | null }>({
    generation: 0,
    controller: null,
  });

  /**
   * Read `/guided/state` under a pass that supersedes any read still in flight
   * (#3660). Without both guards a slow earlier read lands after a newer one
   * and repaints settled state — a finished job shown as running for as long as
   * the page stays open — and keeps writing after unmount: the abort stops the
   * request, the generation stamp drops a response that had already settled
   * when the abort was issued.
   */
  const refreshState = useCallback(async () => {
    const pass = statePass.current;
    pass.controller?.abort();
    const controller = new AbortController();
    const generation = (pass.generation += 1);
    pass.controller = controller;
    const current = () => generation === pass.generation && !controller.signal.aborted;
    try {
      const state = await client.getState({
        signal: controller.signal,
        timeoutMs: stateRequestTimeoutMs,
      });
      if (!current()) {
        return null;
      }
      setQuery({ status: "ready", state });
      return state;
    } catch {
      if (!current()) {
        return null;
      }
      setQuery((previous) =>
        previous.status === "ready" ? previous : { status: "unavailable" },
      );
      return null;
    } finally {
      if (pass.controller === controller) {
        pass.controller = null;
      }
    }
  }, [client]);

  useEffect(() => {
    const pass = statePass.current;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    // Each poll is scheduled only once the previous one has settled, so reads
    // never overlap and cannot arrive out of order.
    const poll = async () => {
      await refreshState();
      if (stopped) {
        return;
      }
      timer = setTimeout(() => void poll(), statePollIntervalMs);
    };
    void poll();
    return () => {
      stopped = true;
      if (timer !== undefined) {
        clearTimeout(timer);
      }
      pass.generation += 1;
      pass.controller?.abort();
      pass.controller = null;
    };
  }, [refreshState]);

  const state = query.status === "ready" ? query.state : null;
  const implementationSelected = workflows.includes("implementation");
  const pullRequestsNeeded =
    implementationSelected || workflows.includes("backlog-curation");
  const customizationPrompt = `Use the goobers-dsl-author skill to inspect ${repo.trim() || "my application repository"} and customize the generated gaggle in the Goobers Instance at ${instancePath.trim() || state?.instancePath || "my instance folder"} for the repository's actual contribution, CI, and review conventions. Explain the proposed state graph and least-privilege capabilities before changing files, then update the configuration and validate it.`;

  useEffect(() => {
    if (!implementationSelected) {
      setWorkflows([...workflows, "implementation"]);
    }
  }, [implementationSelected, setWorkflows, workflows]);

  useEffect(() => {
    if (!inspection) {
      return;
    }
    setBranch(inspection.defaultBranch);
    if (implementationSelected && inspection.ciCommand?.length) {
      setCICommandText(inspection.ciCommand.join(" "));
      setPullRequestCI(false);
    } else if (inspection.pullRequestCI) {
      setCICommandText("");
      setCapability("");
      setPullRequestCI(true);
    }
    if (implementationSelected && inspection.requiredCapabilities?.length) {
      setCapability(inspection.requiredCapabilities[0]);
    }
  }, [
    inspection,
    implementationSelected,
    setCICommandText,
    setCapability,
    setPullRequestCI,
    setBranch,
  ]);

  useEffect(() => {
    if (!inspection || state?.instancePathPinned) {
      return;
    }
    if (instancePlacement === "peer" && inspection.peerInstancePath) {
      setInstancePath(inspection.peerInstancePath);
    }
  }, [
    inspection,
    instancePlacement,
    setInstancePath,
    state?.instancePathPinned,
  ]);

  useEffect(() => {
    if (state?.instancePathPinned) {
      setInstancePath(state.instancePath);
    }
  }, [setInstancePath, state?.instancePath, state?.instancePathPinned]);

  const pages = useMemo<WizardPage[]>(
    () => [
      { id: "welcome", label: "Welcome", step: 1 },
      { id: "repository", label: "Repository", step: 2 },
      { id: "placement", label: "Configuration", step: 3 },
      { id: "workflows", label: "Workflows", step: 4 },
      { id: "issue-scope", label: "Issue scope", step: 5 },
      { id: "runtime", label: "Runtime", step: 6 },
      { id: "review", label: "Review", step: 7 },
      { id: "repository-setup", label: "Repository setup", step: 8 },
      { id: "validate", label: "Checks", step: 9 },
      { id: "complete", label: "Complete", step: 10 },
    ],
    [],
  );

  useEffect(() => {
    if (pageIndex >= pages.length) {
      setPageIndex(pages.length - 1);
    }
  }, [pageIndex, pages.length, setPageIndex]);

  useEffect(() => {
    if (pages[Math.min(pageIndex, pages.length - 1)]?.id !== "complete") {
      return;
    }
    void client.complete().catch((error) => {
      setActionError(error instanceof Error ? error.message : String(error));
    });
  }, [client, pageIndex, pages]);

  if (query.status === "loading") {
    return (
      <section aria-live="polite" className="daemon-state" role="status">
        <span aria-hidden="true" className="loading-mark" />
        <div>
          <h1>Opening setup</h1>
          <p>Reading the setup already completed on this machine.</p>
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
            <h2>Setup is not available from this dashboard</h2>
            <p>
              Open the setup wizard from a terminal. The command starts Goobers locally and
              opens this wizard in your browser.
            </p>
            <RecoveryCommand command="goobers init --guided" />
          </div>
        </section>
      </>
    );
  }

  if (state === null) {
    return null;
  }

  const currentPage = pages[Math.min(pageIndex, pages.length - 1)];
  const instanceReady = state.instanceExists || initResult?.exitCode === 0;
  const validationPassed = validateResult?.exitCode === 0;
  const repositoryReady =
    inspection !== null && !inspection.needsClone && inspection.auth.ready;
  const ciCommand = splitCommand(ciCommandText);
  const runtimeValid =
    pullRequestCI || (ciCommand.length > 0 && capability.trim() !== "");
  const placementValid = instancePath.trim() !== "";
  const repositoryPrepared =
    repositoryReadiness?.usesWorkItemTags === true ||
    repositoryReadiness?.missingLabels.length === 0;
  const cloneCommand =
    inspection?.provider === "github"
      ? `gh repo clone ${inspection.owner}/${inspection.name}`
      : inspection?.provider === "ado"
        ? `az repos clone --organization https://dev.azure.com/${inspection.owner} --project "${inspection.project}" --repository "${inspection.name}"`
        : "";

  const runAction = async <T,>(
    kind: Exclude<BusyAction, null>,
    action: () => Promise<T>,
    apply: (result: T) => void,
  ) => {
    setBusy(kind);
    setActionError(null);
    try {
      apply(await action());
    } catch (error) {
      setActionError(error instanceof Error ? error.message : String(error));
    } finally {
      setBusy(null);
      void refreshState();
    }
  };

  const inspectRepository = () =>
    void runAction(
      "inspect",
      () => client.inspectRepository(repo.trim()),
      (result) => {
        setInspection(result);
        setValidateResult(null);
      },
    );

  const authorizeGitHub = () => {
    if (!inspection || inspection.provider !== "github") {
      setActionError("Inspect a GitHub repository before starting authorization.");
      return;
    }
    void runAction(
      "authorize",
      async () => {
        const authorization: GuidedGitHubAuthorizationResult =
          await client.authorizeGitHub(`${inspection.owner}/${inspection.name}`);
        const refreshed = await client.inspectRepository(repo.trim());
        return { ...refreshed, auth: authorization.auth };
      },
      (result) => {
        setInspection(result);
      },
    );
  };

  const browseRepository = () =>
    void runAction(
      "browse",
      async () => {
        const selected = await client.chooseRepositoryFolder();
        if (selected.canceled || !selected.path) {
          return null;
        }
        return {
          path: selected.path,
          inspection: await client.inspectRepository(selected.path),
        };
      },
      (result) => {
        if (!result) {
          return;
        }
        setRepositorySource("local");
        setRepo(result.path);
        setInspection(result.inspection);
        setValidateResult(null);
      },
    );

  const createGuidedInstance = () => {
    if (!inspection) {
      setActionError("Inspect the repository before creating the instance.");
      return;
    }
    const guided: GuidedInitOptions = {
      provider: inspection.provider,
      owner: inspection.owner,
      project: inspection.project,
      name: inspection.name,
      localPath: inspection.localPath,
      instancePath: instancePath.trim(),
      branch: branch.trim(),
      workflows,
      issueScope,
      ...(issueScope === "assigned" && inspection.auth.identity
        ? { assignedTo: inspection.auth.identity }
        : {}),
      ...(pullRequestCI
        ? { pullRequestCI: true }
        : implementationSelected
        ? {
            ciCommand,
            requiredCapabilities: [capability.trim()],
          }
        : {}),
      harness,
      repoTokenEnv: repoTokenEnv.trim(),
      workTrackingTokenEnv: issuesTokenEnv.trim(),
      ...(inspection.auth.kind === "github-cli" && inspection.auth.identity
        ? { githubCLIUser: inspection.auth.identity }
        : {}),
      ...(inspection.provider === "ado" ? { authKind: inspection.auth.kind } : {}),
      ...(pullRequestsNeeded ? { pullRequestTokenEnv: prTokenEnv.trim() } : {}),
      ...(implementationSelected ? { repoPushTokenEnv: pushTokenEnv.trim() } : {}),
      ...(modelTokenEnv.trim()
        ? { optionalModelTokenEnv: modelTokenEnv.trim() }
        : {}),
    };
    void runAction(
      "init",
      () => client.initInstance({ template: "guided", guided }),
      setInitResult,
    );
  };

  const validate = () =>
    void runAction(
      "validate",
      () => client.validate({ checkHarness: true, checkRepos: true }),
      setValidateResult,
    );

  const prepareRepository = (apply: boolean) =>
    void runAction(
      "prepare",
      () =>
        client.prepareRepository({
          apply,
          createStarterIssue: apply && createStarterIssue,
        }),
      setRepositoryReadiness,
    );

  const goBack = () => {
    if (pageIndex >= pages.findIndex((page) => page.id === "review")) {
      setInitResult(null);
    }
    if (pageIndex >= pages.findIndex((page) => page.id === "repository-setup")) {
      setRepositoryReadiness(null);
    }
    if (pageIndex >= pages.findIndex((page) => page.id === "validate")) {
      setValidateResult(null);
    }
    setActionError(null);
    setPageIndex(Math.max(0, pageIndex - 1));
  };

  const canContinue = (() => {
    switch (currentPage.id) {
      case "welcome":
        return true;
      case "repository":
        return repositoryReady && branch.trim() !== "";
      case "placement":
        return placementValid;
      case "workflows":
        return workflows.length > 0;
      case "issue-scope":
        return issueScope === "all" || issueScope === "assigned";
      case "runtime":
        return runtimeValid;
      case "review":
        return instanceReady;
      case "repository-setup":
        return repositoryPrepared;
      case "validate":
        return validationPassed;
      case "complete":
        return true;
    }
  })();

  const pageContent = (() => {
    switch (currentPage.id) {
      case "welcome":
        return (
          <WizardPage className="guided-welcome-page" title="Welcome to Goobers">
            <div aria-hidden="true" className="guided-mascot-orbit">
              <AnimatedGoober />
            </div>
            <p className="guided-welcome-lead">
              Let&apos;s build an AI workforce for your repository.
            </p>
            <p>
              Guided init inspects how your repository works, creates a reviewable Goobers
              configuration, and checks that it can run safely. It will not start a workflow
              or change application code.
            </p>
            <DocumentationLink
              href="https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/concepts/README.md"
              label="Learn how gaggles, goobers, workflows, and desired state fit together"
            />
            <a
              className="guided-intro-link"
              href="https://goobers.dev/"
              rel="noreferrer"
              target="_blank"
            >
              Watch the Goobers introduction
            </a>
          </WizardPage>
        );
      case "repository":
        return (
          <WizardPage title="Choose the repository">
            <p>
              Choose an existing local clone for full inspection, or start with a GitHub
              or Azure DevOps URL and clone it before continuing.
            </p>
            <div aria-label="Repository source" className="guided-source-choice" role="group">
              <button
                aria-label="Local clone"
                aria-pressed={repositorySource === "local"}
                data-selected={repositorySource === "local"}
                onClick={() => {
                  setRepositorySource("local");
                  setRepo("");
                  setInspection(null);
                }}
                type="button"
              >
                <strong>Local clone</strong>
                <span>Recommended for repository-aware setup</span>
              </button>
              <button
                aria-label="Repository URL"
                aria-pressed={repositorySource === "remote"}
                data-selected={repositorySource === "remote"}
                onClick={() => {
                  setRepositorySource("remote");
                  setRepo("");
                  setInspection(null);
                }}
                type="button"
              >
                <strong>Repository URL</strong>
                <span>Identify it first, then clone locally</span>
              </button>
            </div>
            <form
              className="guided-repository-form"
              onSubmit={(event) => {
                event.preventDefault();
                if (repositoryReady) {
                  setPageIndex(Math.min(pages.length - 1, pageIndex + 1));
                  return;
                }
                if (repo.trim()) {
                  inspectRepository();
                }
              }}
            >
              <div className="guided-repository-entry">
                <TextField
                  label={repositorySource === "local" ? "Local clone" : "Repository URL"}
                  onChange={(value) => {
                    setRepo(value);
                    setInspection(null);
                  }}
                  placeholder={
                    repositorySource === "local"
                      ? "C:\\src\\my-app"
                      : "https://github.com/owner/repository"
                  }
                  value={repo}
                />
                {repositorySource === "local" && (
                  <button
                    className="secondary-button guided-browse-button"
                    disabled={busy !== null}
                    onClick={browseRepository}
                    type="button"
                  >
                    {busy === "browse" ? "Choosing…" : "Browse…"}
                  </button>
                )}
              </div>
              <button
                className="reconnect-button"
                disabled={busy !== null || repo.trim() === ""}
                type="submit"
              >
                {busy === "inspect"
                  ? "Inspecting…"
                  : inspection
                    ? "Inspect again"
                    : repositorySource === "local"
                      ? "Inspect clone"
                      : "Inspect URL"}
              </button>
              {busy === "inspect" && (
                <p aria-live="polite" className="guided-wait-message" role="status">
                  Inspecting repository files and authentication…
                </p>
              )}
              {busy === "browse" && (
                <p aria-live="polite" className="guided-wait-message" role="status">
                  Waiting for you to choose a local clone…
                </p>
              )}
            </form>
            {inspection && (
              <>
                <ReviewTable
                  rows={[
                    ["Provider", inspection.provider === "ado" ? "Azure DevOps" : "GitHub"],
                    ["Repository", inspection.displayName],
                    ["Local clone", inspection.localPath || "Not found"],
                    ["Default branch", inspection.defaultBranch],
                  ]}
                />
                {inspection.ephemeral && (
                  <div className="guided-callout">
                    <strong>This checkout may be ephemeral</strong>
                    <span>
                      {inspection.ephemeralReason ||
                        "The local checkout may be removed when its session ends."}{" "}
                      Keep the runtime instance outside it, for example{" "}
                      <code>
                        {inspection.safeInstancePath || "~/goobers/instances/<repository>"}
                      </code>
                      .
                    </span>
                  </div>
                )}
                {!inspection.auth.ready && (
                  <div className="guided-callout">
                    <strong>
                      {inspection.auth.message ||
                        (inspection.provider === "ado"
                          ? "Azure CLI authentication is required before setup can continue."
                          : "GitHub authentication is required before setup can continue.")}
                    </strong>
                    <span>
                      {inspection.provider === "ado"
                        ? inspection.auth.needsLogin
                          ? "Run the Azure CLI login command, then inspect the repository again. Azure DevOps authentication remains a manual step."
                          : "Confirm that the authenticated Azure account can access this repository before continuing."
                        : inspection.auth.needsLogin
                          ? "Goobers will open GitHub's device/web authorization now. It only runs when this repository needs it, and no token is written to Goobers configuration."
                          : "Confirm that the authenticated GitHub account can access this repository before continuing."}
                    </span>
                    {inspection.provider === "github" && (
                      <>
                        <a
                          className="guided-intro-link"
                          href={githubPATSettingsURL}
                          rel="noreferrer"
                          target="_blank"
                        >
                          Open GitHub fine-grained PAT settings
                        </a>
                        <ol className="guided-pat-steps">
                          <li>
                            Set <strong>Resource owner</strong> to{" "}
                            <strong>{inspection.owner}</strong>, the account or organization
                            that owns this repository. Keep the default personal account when
                            it is the owner.
                          </li>
                          <li>
                            Choose <strong>Only select repositories</strong>, then select{" "}
                            <strong>{inspection.displayName}</strong>.
                          </li>
                          <li>
                            Grant the least-privilege permissions in{" "}
                            <code>docs/guides/github-token-scopes.md</code>, generate the
                            token, then restart this wizard with{" "}
                            <code>GH_TOKEN=&lt;your token&gt; goobers init --guided</code>.
                          </li>
                        </ol>
                      </>
                    )}
                    {inspection.auth.remediationCommand && (
                      <code>{inspection.auth.remediationCommand}</code>
                    )}
                    {inspection.provider === "github" && inspection.auth.needsLogin && (
                      <button
                        className="reconnect-button"
                        disabled={busy !== null}
                        onClick={authorizeGitHub}
                        type="button"
                      >
                        {busy === "authorize"
                          ? "Waiting for GitHub authorization…"
                          : "Sign in with GitHub"}
                      </button>
                    )}
                  </div>
                )}
                {inspection.needsClone ? (
                  <div className="guided-callout">
                    <strong>
                      {inspection.auth.ready
                        ? "Clone the repository, then inspect it again"
                        : inspection.auth.needsLogin
                          ? "Authenticate, then clone the repository"
                          : "Resolve repository access, then clone the repository"}
                    </strong>
                    <span>
                      {inspection.auth.ready
                        ? "Run this command from the folder where you keep source code."
                        : inspection.auth.needsLogin
                          ? "Complete authentication before cloning this repository, then run this command from the folder where you keep source code."
                          : "After the authenticated account can access this repository, run this command from the folder where you keep source code."}
                    </span>
                    {cloneCommand && <code>{cloneCommand}</code>}
                  </div>
                ) : inspection.auth.ready ? (
                  <p className="guided-success">
                    {inspection.provider === "ado"
                      ? "Azure CLI authentication is ready"
                      : "GitHub CLI authentication is ready"}
                    {inspection.auth.identity ? ` as ${inspection.auth.identity}` : ""}.
                  </p>
                ) : null}
                {implementationSelected && !inspection.needsClone && (
                  <div className="guided-repository-runtime">
                    {inspection.stack && (
                      <p className="guided-note">
                        Inferred {inspection.stack} from{" "}
                        {inspection.discovery === "copilot"
                          ? "a read-only Copilot inspection"
                          : "repository files"}
                        {(inspection.evidence?.length ?? 0) > 0
                          ? `: ${inspection.evidence?.join(", ")}`
                          : "."}
                      </p>
                    )}
                    <ReviewTable
                      rows={[
                        [
                          "CI validation",
                          pullRequestCI
                            ? "Provider CI after the pull request opens"
                            : ciCommand.join(" ") || "Not inferred",
                        ],
                        ...(!pullRequestCI
                          ? [["Required capability", capability.trim() || "Not inferred"]]
                          : []),
                      ]}
                    />
                    <details>
                      <summary>Change CI validation</summary>
                      <div className="guided-fields">
                        <label className="guided-check">
                          <input
                            checked={pullRequestCI}
                            onChange={(event) => setPullRequestCI(event.target.checked)}
                            type="checkbox"
                          />
                          <span>Rely on provider CI after the pull request opens.</span>
                        </label>
                        <TextField
                          label="Local CI command"
                          onChange={(value) => {
                            setCICommandText(value);
                            if (value.trim()) {
                              setPullRequestCI(false);
                            }
                          }}
                          placeholder="npm run ci"
                          value={ciCommandText}
                        />
                        <TextField
                          label="Required toolchain capability"
                          onChange={setCapability}
                          placeholder="node@20"
                          value={capability}
                        />
                      </div>
                    </details>
                  </div>
                )}
              </>
            )}
          </WizardPage>
        );
      case "placement":
        return (
          <WizardPage title="Choose where the Goobers Instance lives">
            <p>
              The Instance contains this workforce&apos;s configuration and local operating
              data, including journals, claims, and managed workcopies. Keep it outside
              the application repository.
            </p>
            <DocumentationLink
              href="https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/instance-placement.md"
              label="Learn what the Instance directory contains"
            />
            <div aria-label="Instance location" className="guided-choice-grid" role="group">
              <ChoiceCard
                checked={instancePlacement === "peer"}
                description={`Create one neighboring Instance folder${inspection?.peerInstancePath ? ` at ${inspection.peerInstancePath}` : ""}.`}
                onClick={() => setInstancePlacement("peer")}
                recommended
                title="Neighboring Goobers Instance"
              />
              <ChoiceCard
                checked={instancePlacement === "custom"}
                description="Choose another durable local folder outside the application repository."
                onClick={() => setInstancePlacement("custom")}
                title="Custom location"
              />
            </div>
            {instancePlacement === "custom" && (
              <TextField
                label="Instance folder"
                onChange={setInstancePath}
                placeholder="C:\src\my-app-goobers"
                value={instancePath}
              />
            )}
            <p className="guided-note">
              Do not commit the Instance directory. It contains mutable runtime data in
              addition to the active gaggle and workflow definitions.
            </p>
          </WizardPage>
        );
      case "workflows":
        return (
          <WizardPage title="Set up your first gaggle">
            <p>
              A gaggle is a team of goobers and workflows for one area of work. This
              gaggle is stored in the Instance under{" "}
              <code>config/gaggles/{inspection?.gaggleName || "<repository>"}/</code>.
              Choose the workflows it should start with; you can customize them later
              with the Goobers authoring skills.
            </p>
            <p className="guided-note">
              These are production-oriented canonical modules adapted from{" "}
              <code>config-examples/gaggles/acme-web</code>. They are intentionally more
              complete than the disposable <code>quickstart@v1</code> tutorial workflow.
            </p>
            <div className="guided-module-grid">
              {workflowChoices.map((choice) => (
                <label className="guided-module-card" data-selected={workflows.includes(choice.id)} key={choice.id}>
                  <input
                    checked={workflows.includes(choice.id)}
                    disabled={choice.id === "implementation"}
                    onChange={() =>
                      choice.id !== "implementation" &&
                      setWorkflows(
                        workflows.includes(choice.id)
                          ? workflows.filter((workflow) => workflow !== choice.id)
                          : [...workflows, choice.id],
                      )
                    }
                    type="checkbox"
                  />
                  <img alt="" src={choice.image} />
                  <span>
                    <strong>{choice.title}</strong>
                    <span>{choice.description}</span>
                    <small>{choice.outcome}</small>
                    {choice.id === "implementation" && <small>Required for your first gaggle.</small>}
                  </span>
                </label>
              ))}
            </div>
          </WizardPage>
        );
      case "issue-scope":
        return (
          <WizardPage title="Choose which ready issues Goobers may implement">
            <p>
              Limit implementation to work assigned to your authenticated repository
              identity, or allow it to pick up any issue carrying the required ready and
              approval labels.
            </p>
            <fieldset className="guided-radio-group">
              <legend>Which ready issues may implementation pick up?</legend>
              <label data-selected={issueScope === "all"}>
                <input
                  checked={issueScope === "all"}
                  name="issue-scope"
                  onChange={() => setIssueScope("all")}
                  type="radio"
                />
                <span>
                  <strong>All ready issues</strong>
                  <small>Any issue carrying the required ready and approval labels.</small>
                </span>
              </label>
              <label data-selected={issueScope === "assigned"}>
                <input
                  checked={issueScope === "assigned"}
                  name="issue-scope"
                  onChange={() => setIssueScope("assigned")}
                  type="radio"
                />
                <span>
                  <strong>Only issues assigned to me</strong>
                  <small>
                    {inspection?.auth.identity
                      ? `Limits work to issues assigned to ${inspection.auth.identity}.`
                      : "Goobers will resolve your authenticated repository identity before creating the instance."}
                  </small>
                </span>
              </label>
            </fieldset>
          </WizardPage>
        );
      case "runtime":
        return (
          <WizardPage title="Configure the agent runtime">
            <p>
              A harness runs agentic stages, while deterministic stages and gates keep
              control of repository effects, retries, and escalation.
            </p>
            <DocumentationLink
              href="https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/requirements/goober.md"
              label="Learn how goobers, harnesses, capabilities, and instructions work"
            />
            <fieldset className="guided-radio-group">
              <legend>Agent harness</legend>
              <label data-selected={harness === "copilot"}>
                <input
                  checked={harness === "copilot"}
                  name="harness"
                  onChange={() => setHarness("copilot")}
                  type="radio"
                />
                <span>
                  <strong>GitHub Copilot CLI</strong>
                  <small>
                    Uses your signed-in Copilot CLI to implement, review, and customize
                    repository work.
                  </small>
                </span>
              </label>
              <label data-selected={harness === "claude-code"}>
                <input
                  checked={harness === "claude-code"}
                  name="harness"
                  onChange={() => setHarness("claude-code")}
                  type="radio"
                />
                <span>
                  <strong>Claude Code CLI</strong>
                  <small>
                    Uses your signed-in Claude Code CLI for the same generated workforce
                    roles.
                  </small>
                </span>
              </label>
            </fieldset>
          </WizardPage>
        );
      case "review":
        return (
          <WizardPage title="Review and create the instance">
            <ReviewTable
              rows={[
                ["Application repository", `${repo.trim()} (${branch.trim()})`],
                ["Goobers Instance", instancePath.trim()],
                ["Workflow modules", workflows.join(", ")],
                [
                  "Ready issue scope",
                  issueScope === "assigned"
                    ? `Assigned to ${inspection?.auth.identity || "authenticated user"}`
                    : "All ready issues",
                ],
                ["Agent harness", harness],
                [
                  "CI validation",
                  pullRequestCI ? "Provider CI after pull request" : ciCommand.join(" "),
                ],
                ...(!pullRequestCI
                  ? [["Required capability", capability.trim()]]
                  : []),
              ]}
            />
            <p className="guided-note">
              Setup creates one Instance directory containing the active configuration,
              journals, claims, managed workcopies, and other machine-local operating
              data. Existing Instance contents are never replaced.
            </p>
            <button
              className="reconnect-button guided-create-instance-button"
              disabled={busy !== null || instanceReady}
              onClick={createGuidedInstance}
              type="button"
            >
              {busy === "init"
                ? "Creating…"
                : initResult?.exitCode === 0
                  ? "Instance created"
                  : state.instanceExists
                    ? "Instance already exists"
                  : "Create Goobers instance"}
            </button>
            {state.instanceExists && initResult?.exitCode !== 0 && (
              <p className="guided-note">
                Restart clears the tutorial answers, but it does not delete Instance
                Configuration or runtime files that were already created.
              </p>
            )}
            {initResult?.stdout && <p className="guided-success">{initResult.stdout}</p>}
          </WizardPage>
        );
      case "repository-setup":
        return (
          <WizardPage title="Prepare the repository">
            <p>
              Goobers uses repository labels to identify eligible work and record workflow
              state. Check the repository first; nothing is changed until you approve it.
            </p>
            {!repositoryReadiness && (
              <button
                className="reconnect-button"
                disabled={busy !== null}
                onClick={() => prepareRepository(false)}
                type="button"
              >
                {busy === "prepare" ? "Checking…" : "Check labels and ready issues"}
              </button>
            )}
            {repositoryReadiness?.usesWorkItemTags && (
              <div className="guided-callout">
                <strong>Azure DevOps uses work-item tags</strong>
                <span>
                  These tags are created when Goobers first applies them; no repository
                  label catalog needs to be changed.
                </span>
              </div>
            )}
            {repositoryReadiness && !repositoryReadiness.usesWorkItemTags && (
              <>
                <div className="guided-callout">
                  <strong>Repository changes</strong>
                  <span>
                    {repositoryReadiness.missingLabels.length > 0
                      ? `Goobers will create ${repositoryReadiness.missingLabels.length} missing label(s). Existing labels will not be modified.`
                      : "All required labels already exist. Existing labels were not modified."}
                  </span>
                </div>
                <div className="guided-badges">
                  {repositoryReadiness.missingLabels.map((label) => (
                    <span className="guided-badge guided-badge-missing" key={label}>
                      {label}
                    </span>
                  ))}
                </div>
                <p>
                  Eligible ready issues:{" "}
                  <strong>{repositoryReadiness.eligibleCount ?? "unknown"}</strong>
                </p>
                {(repositoryReadiness.eligibleCount ?? 0) === 0 && (
                  <label className="guided-check">
                    <input
                      checked={createStarterIssue}
                      onChange={(event) => setCreateStarterIssue(event.target.checked)}
                      type="checkbox"
                    />
                    <span>
                      Create one safe starter issue to add a <code>HELLO-GOOBERS.md</code>{" "}
                      file, with the required ready and approval labels.
                    </span>
                  </label>
                )}
                {(!repositoryPrepared ||
                  (repositoryReadiness.eligibleCount ?? 0) === 0) && (
                  <div className="guided-inline-actions">
                    <button
                      className="reconnect-button"
                      disabled={
                        busy !== null ||
                        ((repositoryReadiness.eligibleCount ?? 0) === 0 &&
                          !createStarterIssue)
                      }
                      onClick={() => prepareRepository(true)}
                      type="button"
                    >
                      {busy === "prepare"
                        ? "Preparing…"
                        : (repositoryReadiness.eligibleCount ?? 0) === 0
                          ? repositoryReadiness.missingLabels.length > 0
                            ? "Create missing labels and starter issue"
                            : "Create starter issue"
                          : "Create missing labels"}
                    </button>
                    <button
                      className="secondary-button"
                      disabled={busy !== null}
                      onClick={() => prepareRepository(false)}
                      type="button"
                    >
                      Check again
                    </button>
                  </div>
                )}
                {repositoryPrepared && (
                  <p className="guided-success">
                    {(repositoryReadiness.eligibleCount ?? 0) > 0
                      ? "The repository is ready and has eligible work."
                      : "The required repository labels are ready. No eligible work was found; creating a starter issue is optional."}
                  </p>
                )}
              </>
            )}
          </WizardPage>
        );
      case "validate":
        return (
          <WizardPage title="Check the setup">
            <p>
              Goobers will validate the configuration, confirm the selected harness is
              usable, and verify access to the repository.
            </p>
            <RecoveryCommand
              command={`goobers validate --check-harness --check-repos "${state.instancePath}"`}
            />
            <button
              className="reconnect-button guided-validate-button"
              disabled={busy !== null}
              onClick={validate}
              type="button"
            >
              {busy === "validate" ? "Checking…" : "Run checks"}
            </button>
            {busy === "validate" && (
              <p aria-live="polite" className="guided-wait-message" role="status">
                This may take a minute or two while Goobers checks the harness and
                repository.
              </p>
            )}
            {validateResult && <ValidationResult result={validateResult} />}
          </WizardPage>
        );
      case "complete":
        return (
          <WizardPage className="guided-complete-page" title="Goobers is ready">
            <div aria-hidden="true" className="guided-complete-mascot">
              <AnimatedGoober />
            </div>
            <p className="guided-welcome-lead">
              Your configuration is ready, and the setup server has stopped.
            </p>
            <p>
              Next, ask your coding agent to tailor the generated gaggle to this
              repository.
            </p>
            <div className="guided-prompt-copy">
              <code>{customizationPrompt}</code>
              <button
                aria-label={promptCopied ? "Prompt copied" : "Copy prompt"}
                className="guided-copy-button"
                onClick={() => {
                  void navigator.clipboard.writeText(customizationPrompt).then(
                    () => setPromptCopied(true),
                    (error) =>
                      setActionError(
                        error instanceof Error ? error.message : String(error),
                      ),
                  );
                }}
                title={promptCopied ? "Copied" : "Copy prompt"}
                type="button"
              >
                <Icon name={promptCopied ? "check" : "copy"} size={17} />
              </button>
            </div>
            <DocumentationLink
              href="https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/dsl-authoring-skill.md"
              label="Learn how to customize gaggles and workflows with an agent"
            />
            <p>You can now close this browser window.</p>
          </WizardPage>
        );
    }
  })();

  return (
    <div className="guided-wizard">
      {actionError && (
        <section className="daemon-state daemon-state-error guided-action-error" role="alert">
          <div>
            <h1>That step could not be completed</h1>
            <p>{actionError}</p>
          </div>
        </section>
      )}

      <main className="guided-wizard-content">{pageContent}</main>

      {currentPage.id !== "complete" && (
      <nav aria-label="Setup progress" className="guided-wizard-footer">
        <div className="guided-footer-actions">
          <button
            className="secondary-button"
            disabled={pageIndex === 0 || busy !== null}
            onClick={goBack}
            type="button"
          >
            Back
          </button>
          <button
            className="guided-restart-button"
            disabled={busy !== null}
            onClick={() => {
              if (!window.confirm("Restart setup? Your selections will be cleared. Files and repository changes already created will remain.")) {
                return;
              }
              for (let index = window.sessionStorage.length - 1; index >= 0; index -= 1) {
                const key = window.sessionStorage.key(index);
                if (key?.startsWith("goobers-wizard-")) {
                  window.sessionStorage.removeItem(key);
                }
              }
              window.location.reload();
            }}
            type="button"
          >
            Restart
          </button>
        </div>
        <div className="guided-wizard-progress">
          <div>
            <span>Step {currentPage.step} of {wizardStepCount}</span>
          </div>
          <div
            aria-valuemax={wizardStepCount}
            aria-valuemin={1}
            aria-valuenow={currentPage.step}
            className="guided-progress-track"
            role="progressbar"
          >
            <span style={{ width: `${(currentPage.step / wizardStepCount) * 100}%` }} />
          </div>
        </div>
        <button
          className="reconnect-button"
          disabled={!canContinue || busy !== null}
          onClick={() => setPageIndex(Math.min(pages.length - 1, pageIndex + 1))}
          type="button"
        >
          Continue
        </button>
      </nav>
      )}
    </div>
  );
}

function WizardPage({
  children,
  className = "",
  title,
}: {
  children: React.ReactNode;
  className?: string;
  title: string;
}) {
  return (
    <section className={`content-section guided-wizard-page ${className}`.trim()}>
      <h2>{title}</h2>
      {children}
    </section>
  );
}

function DocumentationLink({ href, label }: { href: string; label: string }) {
  return (
    <a className="guided-intro-link" href={href} rel="noreferrer" target="_blank">
      {label}
    </a>
  );
}

function ChoiceCard({
  checked,
  description,
  onClick,
  recommended = false,
  title,
}: {
  checked: boolean;
  description: string;
  onClick: () => void;
  recommended?: boolean;
  title: string;
}) {
  return (
    <button
      aria-pressed={checked}
      className="guided-choice-card"
      data-selected={checked}
      onClick={onClick}
      type="button"
    >
      {recommended && <span className="guided-choice-recommendation">Recommended</span>}
      <strong>{title}</strong>
      <span>{description}</span>
    </button>
  );
}

function TextField({
  label,
  onChange,
  placeholder,
  value,
}: {
  label: string;
  onChange: (value: string) => void;
  placeholder: string;
  value: string;
}) {
  return (
    <label className="guided-field">
      <span>{label}</span>
      <input
        onChange={(event) => onChange(event.target.value)}
        placeholder={placeholder}
        type="text"
        value={value}
      />
    </label>
  );
}

function ReviewTable({ rows }: { rows: string[][] }) {
  return (
    <dl className="guided-review">
      {rows.map(([label, value]) => (
        <div key={label}>
          <dt>{label}</dt>
          <dd>{value}</dd>
        </div>
      ))}
    </dl>
  );
}

function ValidationResult({
  result,
}: {
  result: { exitCode: number; envelope: DiagnosticsEnvelope | null; stderr: string };
}) {
  if (result.exitCode === 0) {
    return <p className="guided-success">All configuration, harness, and repository checks passed.</p>;
  }
  const findings = result.envelope?.findings ?? [];
  return (
    <div className="guided-result" role="alert">
      <strong>Some checks need attention.</strong>
      {findings.length > 0 ? (
        <ul className="guided-checklist">
          {findings.map((finding) => (
            <li key={`${finding.code}-${finding.file}-${finding.line ?? 0}`}>
              <code>{finding.code}</code> {finding.file}: {finding.message}
            </li>
          ))}
        </ul>
      ) : (
        <pre className="code-block guided-output">{result.stderr}</pre>
      )}
    </div>
  );
}

function splitCommand(value: string): string[] {
  const trimmed = value.trim();
  if (trimmed === "") {
    return [];
  }
  if (trimmed.startsWith("[")) {
    try {
      const parsed = JSON.parse(trimmed) as unknown;
      if (Array.isArray(parsed) && parsed.every((part) => typeof part === "string")) {
        return parsed;
      }
    } catch {
      return [];
    }
  }
  return trimmed.split(/\s+/);
}

function useSessionState<T>(
  key: string,
  initialValue: T,
): [T, (value: T) => void] {
  const [value, setValue] = useState<T>(() => {
    const stored = window.sessionStorage.getItem(key);
    if (stored === null) {
      return initialValue;
    }
    try {
      return JSON.parse(stored) as T;
    } catch {
      return initialValue;
    }
  });
  const update = useCallback(
    (next: T) => {
      setValue(next);
      window.sessionStorage.setItem(key, JSON.stringify(next));
    },
    [key],
  );
  return [value, update];
}
