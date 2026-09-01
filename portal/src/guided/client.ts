// Typed fetch client for the /guided/* endpoints served by
// `goobers init --guided`.
//
// This deliberately lives OUTSIDE src/api/contract.generated.ts: the guided
// endpoints are dashboard-local wrappers over the product-owned onboarding
// actions (including structured guided initialization), not daemon API routes.
// They exist only while the getting-started command is serving this page,
// carry no daemon read-model semantics, and must not participate in the
// generated wire-contract drift checks.

export interface GuidedEnvState {
  /** The repository-token environment variable name the server is actually
   *  checking — the default, or whatever `connect --token-env` recorded.
   *  goobersGithubToken reports presence for exactly this name, read from
   *  the getting-started server's own process. */
  tokenEnv: string;
  goobersGithubToken: boolean;
  goobersGithubIssuesToken: boolean;
}

export interface GuidedJobSummary {
  id: string;
  kind: "run";
  done: boolean;
  exitCode: number | null;
  runId: string | null;
}

export interface GuidedJobDetail extends GuidedJobSummary {
  output: string[];
}

/** The repository the tutorial instance is connected to. Repo is null until
 *  instance.yaml names a real (non-placeholder) repository. */
export interface GuidedConnectedState {
  repo: string | null;
}

export interface GuidedState {
  version: number;
  platform: "windows" | "darwin" | "linux" | string;
  workdir: string;
  instancePath: string;
  configPath: string;
  suggestedStack?: string;
  suggestedCICommand?: string[];
  suggestedCapability?: string;
  instanceExists: boolean;
  env: GuidedEnvState;
  job: GuidedJobSummary | null;
  apiReady: boolean;
  connected: GuidedConnectedState;
}

export interface GuidedRepositoryInspection {
  provider: "github" | "ado";
  owner: string;
  project?: string;
  name: string;
  displayName: string;
  gaggleName: string;
  localPath?: string;
  defaultBranch: string;
  stack?: string;
  ciCommand?: string[];
  requiredCapabilities?: string[];
  pullRequestCI?: boolean;
  discovery: "deterministic" | "copilot" | "provider-metadata" | "unresolved";
  evidence?: string[];
  needsClone: boolean;
  peerConfigPath?: string;
  inRepoConfigPath?: string;
  ephemeral?: boolean;
  ephemeralReason?: string;
  safeInstancePath?: string;
  auth: {
    kind: "github-cli" | "azure-cli" | string;
    ready: boolean;
    identity?: string;
    remediationCommand?: string;
    message?: string;
    needsLogin?: boolean;
  };
}

export interface GuidedGitHubAuthorizationResult {
  auth: GuidedRepositoryInspection["auth"];
  message: string;
}

export interface GuidedChooseFolderResult {
  path?: string;
  canceled: boolean;
}

export interface GuidedRepositoryReadiness {
  provider: "github" | "ado";
  repository: string;
  selectorLabels: string[];
  lifecycleLabels: string[];
  missingLabels: string[];
  createdLabels?: string[];
  eligibleCount?: number;
  starterIssueCreated?: boolean;
  usesWorkItemTags?: boolean;
}

/** The shared shape of the envelope-returning actions: the CLI subprocess's
 *  exit code, its parsed stdout JSON (null when stdout was not JSON), and its
 *  raw stderr. */
export interface GuidedEnvelopeResult<E> {
  exitCode: number;
  envelope: E | null;
  stderr: string;
}

/** Fields rendered from structured onboarding action envelopes. */
export interface OnboardingActionEnvelope {
  action?: string;
  version?: number;
  created?: string[];
  updated?: string[];
  skipped?: string[];
  path?: string;
  nextCommand?: string;
}

export interface GuidedInitResult {
  exitCode: number;
  stdout: string;
  stderr: string;
}

/** `goobers validate --json` diagnostics envelope (fields we render). */
export interface DiagnosticsFinding {
  file: string;
  line?: number;
  col?: number;
  path?: string;
  code: string;
  severity: string;
  message: string;
}

export interface DiagnosticsEnvelope {
  ok?: boolean;
  counts?: { errors: number; warnings: number };
  findings?: DiagnosticsFinding[];
}

/** `goobers status --json` envelope (fields we render). */
export interface StatusEnvelope {
  timeToFirstPR?: {
    anchor?: string;
    initCompletedAt?: string;
    firstPROpenAt?: string;
    milliseconds?: number;
  };
}

/** `/guided/actions/probe-backlog` result (#2638): a read-only eligibility
 *  scan run BEFORE the sample quickstart's own run dispatches, so the wizard
 *  can warn "0 eligible issues" instead of letting a no-work run masquerade
 *  as success. `eligibleCount` is null when the probe could not run yet (no
 *  issues token exported) — distinct from a checked, genuine zero. */
export interface GuidedProbeResult {
  exitCode: number;
  eligibleCount: number | null;
  stderr: string;
}

export interface InitInstanceRequest {
  template?: "quickstart" | "starter" | "guided";
  guided?: GuidedInitOptions;
}

export type GuidedWorkflow =
  | "implementation"
  | "backlog-curation"
  | "work-nomination";

export interface GuidedInitOptions {
  repo?: string;
  provider?: "github" | "ado";
  owner?: string;
  project?: string;
  name?: string;
  localPath?: string;
  configPath?: string;
  branch: string;
  workflows: GuidedWorkflow[];
  issueScope: "all" | "assigned";
  assignedTo?: string;
  pullRequestCI?: boolean;
  ciCommand?: string[];
  requiredCapabilities?: string[];
  harness: "copilot" | "claude-code";
  repoTokenEnv: string;
  workTrackingTokenEnv: string;
  pullRequestTokenEnv?: string;
  repoPushTokenEnv?: string;
  optionalModelTokenEnv?: string;
  githubCLIUser?: string;
  authKind?: string;
}

export interface ConnectRequest {
  repo: string;
  tokenEnv?: string;
  seed?: boolean;
  replace?: boolean;
}

export interface RunRequest {
  workflow?:
    | "quickstart"
    | "default-implement"
    | "implementation"
    | "backlog-curation"
    | "work-nomination";
}

export interface ValidateRequest {
  checkHarness: boolean;
  checkRepos: boolean;
}

/** A non-2xx /guided/ response, carrying the server's {code, message} body when
 *  one was parseable. A missing route (running under the plain dashboard or
 *  daemon portal) surfaces as status 404. */
class GuidedRequestError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = "GuidedRequestError";
    this.status = status;
    this.code = code;
  }
}

type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export class GuidedClient {
  private readonly fetchFn: FetchLike;

  constructor(fetchFn?: FetchLike) {
    this.fetchFn = fetchFn ?? ((input, init) => fetch(input, init));
  }

  getState(): Promise<GuidedState> {
    return this.request<GuidedState>("/guided/state");
  }

  inspectRepository(location: string): Promise<GuidedRepositoryInspection> {
    return this.post("/guided/actions/inspect-repository", { location });
  }

  authorizeGitHub(repository: string): Promise<GuidedGitHubAuthorizationResult> {
    return this.post("/guided/actions/authorize-github", { repository });
  }

  chooseRepositoryFolder(): Promise<GuidedChooseFolderResult> {
    return this.post("/guided/actions/choose-repository-folder", {});
  }

  prepareRepository(body: {
    apply: boolean;
    createStarterIssue: boolean;
  }): Promise<GuidedRepositoryReadiness> {
    return this.post("/guided/actions/prepare-repository", body);
  }

  initInstance(body: InitInstanceRequest = {}): Promise<GuidedInitResult> {
    return this.post("/guided/actions/init-instance", body);
  }

  connect(body: ConnectRequest): Promise<GuidedEnvelopeResult<OnboardingActionEnvelope>> {
    return this.post("/guided/actions/connect", body);
  }

  validate(body: ValidateRequest): Promise<GuidedEnvelopeResult<DiagnosticsEnvelope>> {
    return this.post("/guided/actions/validate", body);
  }

  startRun(body: RunRequest = {}): Promise<{ jobId: string }> {
    return this.post("/guided/actions/run", body);
  }

  getJob(id: string): Promise<GuidedJobDetail> {
    return this.request<GuidedJobDetail>(`/guided/jobs/${encodeURIComponent(id)}`);
  }

  getStatus(): Promise<GuidedEnvelopeResult<StatusEnvelope>> {
    return this.request("/guided/status");
  }

  probeBacklog(): Promise<GuidedProbeResult> {
    return this.request("/guided/actions/probe-backlog");
  }

  private post<T>(path: string, body: unknown): Promise<T> {
    return this.request<T>(path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  }

  private async request<T>(path: string, init?: RequestInit): Promise<T> {
    const response = await this.fetchFn(path, init);
    if (!response.ok) {
      let code = "request_failed";
      let message = `${path} failed with status ${response.status}`;
      try {
        const body = (await response.json()) as { code?: string; message?: string };
        if (typeof body.code === "string") code = body.code;
        if (typeof body.message === "string") message = body.message;
      } catch {
        // Non-JSON error body (e.g. a dev server 404 page): keep the fallback.
      }
      throw new GuidedRequestError(response.status, code, message);
    }
    return (await response.json()) as T;
  }
}
