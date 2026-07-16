export type StageKind = "deterministic" | "agentic" | "gate";
export type RunStatus = "running" | "completed" | "failed" | "aborted" | "escalated";
export type NodeState = "pending" | "active" | "complete" | "failed" | "escalated";
export type EventTone = "neutral" | "active" | "success" | "warning" | "danger";

export interface WorkflowStage {
  id: string;
  name: string;
  description: string;
  kind: StageKind;
  goober?: string;
  evaluator?: string;
  retry: string;
  x: number;
  y: number;
  yaml: string;
}

export interface WorkflowEdge {
  from: string;
  to: string;
  label?: string;
  repass?: boolean;
}

export interface Workflow {
  id: string;
  name: string;
  description: string;
  gaggle: string;
  version: number;
  digest: string;
  trigger: string;
  maxConcurrency: number;
  status: "enabled" | "paused";
  activeRuns: number;
  lastOutcome: RunStatus;
  lastRunAt: string;
  stages: WorkflowStage[];
  edges: WorkflowEdge[];
}

export interface Artifact {
  name: string;
  mediaType: string;
  size: string;
  summary: string;
  digest: string;
  digestVerified: boolean;
  recordedSeq: number;
  content?: string;
  contentError?: string;
  downloadUrl?: string;
}

export interface StageAttempt {
  id: string;
  stageId: string;
  number: number;
  kind: "initial" | "policy" | "infra";
  status: "running" | "completed" | "failed";
  duration: string;
  startedSeq: number;
  endedSeq?: number;
  summary: string;
  outputs?: Record<string, string | number | boolean>;
  artifacts: Artifact[];
}

export interface RunEvent {
  id: string;
  seq: number;
  time: string;
  elapsed: string;
  type: string;
  title: string;
  detail: string;
  tone: EventTone;
  stageId?: string;
  attempt?: number;
}

export interface Escalation {
  title: string;
  cause: string;
  gate: string;
  branch: string;
  attemptsUsed: number;
  attemptsAllowed: number;
}

export interface Run {
  id: string;
  shortId: string;
  title: string;
  issue: string;
  workflowId: string;
  workflowVersion: number;
  workflowDigest: string;
  workflowStages: WorkflowStage[];
  workflowEdges: WorkflowEdge[];
  status: RunStatus;
  startedAt: string;
  duration: string;
  trigger: string;
  currentStage: string;
  repasses: number;
  events: RunEvent[];
  attempts: StageAttempt[];
  escalation?: Escalation;
}

export interface InstanceWarning {
  code: string;
  title: string;
  detail: string;
}

function fixtureArtifact(artifact: Omit<Artifact, "digestVerified">): Artifact {
  return { ...artifact, digestVerified: artifact.content !== undefined };
}

const implementationStages: WorkflowStage[] = [
  {
    id: "context",
    name: "Gather context",
    description: "Resolve the issue, repository state, and workflow inputs into a pinned context artifact.",
    kind: "deterministic",
    goober: "context-loader",
    retry: "2 attempts",
    x: 10,
    y: 43,
    yaml: `- name: context
  type: deterministic
  run:
    command: [goobers, gather-pr-context]
  next: implement`,
  },
  {
    id: "implement",
    name: "Implement",
    description: "Make the scoped code change in a contained worktree and report the resulting artifacts.",
    kind: "agentic",
    goober: "implementer",
    retry: "3 attempts",
    x: 31,
    y: 43,
    yaml: `- name: implement
  type: agentic
  goober: implementer
  retry:
    maxAttempts: 3
  next: review`,
  },
  {
    id: "review",
    name: "Review",
    description: "Inspect the implementation diff and produce a structured pass or needs-changes verdict.",
    kind: "agentic",
    goober: "reviewer",
    retry: "1 attempt",
    x: 52,
    y: 43,
    yaml: `- name: review
  type: agentic
  goober: reviewer
  next: review-gate`,
  },
  {
    id: "review-gate",
    name: "Review gate",
    description: "Route a passing verdict forward, a needs-changes verdict back to implementation, or exhausted repasses to escalation.",
    kind: "gate",
    evaluator: "agentic verdict",
    retry: "3 repasses",
    x: 73,
    y: 43,
    yaml: `- name: review-gate
  evaluator: agentic
  branches:
    pass: merge
    needs-changes: implement
    exhausted: "@escalate"`,
  },
  {
    id: "merge",
    name: "Merge",
    description: "Rebase, merge the approved pull request, and record the terminal provider mutation.",
    kind: "deterministic",
    goober: "merge-pr",
    retry: "2 attempts",
    x: 93,
    y: 43,
    yaml: `- name: merge
  type: deterministic
  run:
    command: [goobers, merge-pr]
  next: ""`,
  },
];

const implementationEdges: WorkflowEdge[] = [
  { from: "context", to: "implement" },
  { from: "implement", to: "review" },
  { from: "review", to: "review-gate" },
  { from: "review-gate", to: "merge", label: "pass" },
  { from: "review-gate", to: "implement", label: "needs changes", repass: true },
];

const curationStages: WorkflowStage[] = [
  {
    id: "query",
    name: "Query backlog",
    description: "Select eligible work from the configured provider.",
    kind: "deterministic",
    goober: "backlog-query",
    retry: "2 attempts",
    x: 12,
    y: 43,
    yaml: `- name: query
  type: deterministic
  next: nominate`,
  },
  {
    id: "nominate",
    name: "Nominate",
    description: "Assess candidate issues against the current product priorities.",
    kind: "agentic",
    goober: "nominator",
    retry: "2 attempts",
    x: 37,
    y: 43,
    yaml: `- name: nominate
  type: agentic
  goober: nominator
  next: curate`,
  },
  {
    id: "curate",
    name: "Curate",
    description: "Produce a bounded, implementation-ready issue description.",
    kind: "agentic",
    goober: "curator",
    retry: "2 attempts",
    x: 63,
    y: 43,
    yaml: `- name: curate
  type: agentic
  goober: curator
  next: approval`,
  },
  {
    id: "approval",
    name: "Approval gate",
    description: "Check that the issue is approved and ready for implementation.",
    kind: "gate",
    evaluator: "coded check",
    retry: "No repass",
    x: 88,
    y: 43,
    yaml: `- name: approval
  evaluator: automated
  branches:
    pass: ""
    fail: "@abort"`,
  },
];

export const workflows: Workflow[] = [
  {
    id: "implementation",
    name: "Implementation",
    description: "Turn an approved issue into a reviewed, merged pull request.",
    gaggle: "goobers",
    version: 8,
    digest: "sha256:1df8b863d11a9a3c",
    trigger: "Backlog item",
    maxConcurrency: 3,
    status: "enabled",
    activeRuns: 1,
    lastOutcome: "escalated",
    lastRunAt: "8 min ago",
    stages: implementationStages,
    edges: implementationEdges,
  },
  {
    id: "backlog-curation",
    name: "Backlog curation",
    description: "Find, assess, and prepare the next bounded piece of work.",
    gaggle: "goobers",
    version: 3,
    digest: "sha256:7a42a16b5d88f207",
    trigger: "Every 30 minutes",
    maxConcurrency: 1,
    status: "enabled",
    activeRuns: 0,
    lastOutcome: "completed",
    lastRunAt: "24 min ago",
    stages: curationStages,
    edges: [
      { from: "query", to: "nominate" },
      { from: "nominate", to: "curate" },
      { from: "curate", to: "approval" },
    ],
  },
  {
    id: "merge-review",
    name: "Merge review",
    description: "Inspect a merged change and dispatch any required remediation.",
    gaggle: "goobers",
    version: 2,
    digest: "sha256:9634e0d912cb6f51",
    trigger: "Pull request merged",
    maxConcurrency: 2,
    status: "enabled",
    activeRuns: 1,
    lastOutcome: "completed",
    lastRunAt: "3 min ago",
    stages: implementationStages.slice(1, 4).map((stage, index) => ({
      ...stage,
      x: 20 + index * 30,
    })),
    edges: [
      { from: "implement", to: "review" },
      { from: "review", to: "review-gate" },
    ],
  },
];

const activeRunEvents: RunEvent[] = [
  {
    id: "active-1",
    seq: 1,
    time: "21:12:04",
    elapsed: "0:00",
    type: "run.started",
    title: "Run started",
    detail: "Issue #441 claimed by the implementation workflow.",
    tone: "neutral",
  },
  {
    id: "active-2",
    seq: 2,
    time: "21:12:05",
    elapsed: "0:01",
    type: "stage.started",
    title: "Gathering context",
    detail: "Loading the issue, instance configuration, and repository state.",
    tone: "active",
    stageId: "context",
    attempt: 1,
  },
  {
    id: "active-3",
    seq: 3,
    time: "21:12:07",
    elapsed: "0:03",
    type: "stage.finished",
    title: "Context pinned",
    detail: "Issue, branch, and workflow definition recorded as immutable inputs.",
    tone: "success",
    stageId: "context",
    attempt: 1,
  },
  {
    id: "active-4",
    seq: 4,
    time: "21:12:08",
    elapsed: "0:04",
    type: "stage.started",
    title: "Implementation started",
    detail: "Implementer entered a contained worktree with the pinned context.",
    tone: "active",
    stageId: "implement",
    attempt: 1,
  },
  {
    id: "active-5",
    seq: 5,
    time: "21:18:31",
    elapsed: "6:27",
    type: "artifact.recorded",
    title: "Pull request opened",
    detail: "PR #472 and the implementation summary were recorded.",
    tone: "neutral",
    stageId: "implement",
    attempt: 1,
  },
  {
    id: "active-6",
    seq: 6,
    time: "21:18:42",
    elapsed: "6:38",
    type: "stage.finished",
    title: "Implementation complete",
    detail: "Targeted tests passed. Review can begin.",
    tone: "success",
    stageId: "implement",
    attempt: 1,
  },
  {
    id: "active-7",
    seq: 7,
    time: "21:18:44",
    elapsed: "6:40",
    type: "stage.started",
    title: "Review in progress",
    detail: "Reviewer is inspecting the API contract and tests.",
    tone: "active",
    stageId: "review",
    attempt: 1,
  },
];

const escalatedRunEvents: RunEvent[] = [
  {
    id: "escalated-1",
    seq: 1,
    time: "19:44:10",
    elapsed: "0:00",
    type: "run.started",
    title: "Run started",
    detail: "Issue #402 claimed by the implementation workflow.",
    tone: "neutral",
  },
  {
    id: "escalated-2",
    seq: 2,
    time: "19:44:11",
    elapsed: "0:01",
    type: "stage.started",
    title: "Gathering context",
    detail: "Repository and issue state are being pinned.",
    tone: "active",
    stageId: "context",
    attempt: 1,
  },
  {
    id: "escalated-3",
    seq: 3,
    time: "19:44:13",
    elapsed: "0:03",
    type: "stage.finished",
    title: "Context pinned",
    detail: "The run is ready for implementation.",
    tone: "success",
    stageId: "context",
    attempt: 1,
  },
  {
    id: "escalated-4",
    seq: 4,
    time: "19:44:15",
    elapsed: "0:05",
    type: "stage.started",
    title: "Implementation attempt 1",
    detail: "Implementer began the first bounded attempt.",
    tone: "active",
    stageId: "implement",
    attempt: 1,
  },
  {
    id: "escalated-5",
    seq: 5,
    time: "19:51:02",
    elapsed: "6:52",
    type: "stage.finished",
    title: "Implementation attempt 1 complete",
    detail: "A daemon endpoint and partial portal client were produced.",
    tone: "success",
    stageId: "implement",
    attempt: 1,
  },
  {
    id: "escalated-6",
    seq: 6,
    time: "19:51:03",
    elapsed: "6:53",
    type: "stage.started",
    title: "Review attempt 1",
    detail: "Reviewer began checking the implementation against the issue scope.",
    tone: "active",
    stageId: "review",
    attempt: 1,
  },
  {
    id: "escalated-7",
    seq: 7,
    time: "19:52:14",
    elapsed: "8:04",
    type: "gate.evaluated",
    title: "Changes requested",
    detail: "The run mixed API, UI, and DAG work without completing a coherent vertical slice.",
    tone: "warning",
    stageId: "review-gate",
    attempt: 1,
  },
  {
    id: "escalated-8",
    seq: 8,
    time: "19:52:16",
    elapsed: "8:06",
    type: "stage.started",
    title: "Implementation policy repass",
    detail: "The reviewer-requested policy repass began.",
    tone: "active",
    stageId: "implement",
    attempt: 2,
  },
  {
    id: "escalated-9",
    seq: 9,
    time: "19:53:04",
    elapsed: "8:54",
    type: "stage.finished",
    title: "Implementation repass interrupted",
    detail: "A transient runner transport failure interrupted the policy repass before it produced an outcome.",
    tone: "danger",
    stageId: "implement",
    attempt: 2,
  },
  {
    id: "escalated-10",
    seq: 10,
    time: "19:53:06",
    elapsed: "8:56",
    type: "stage.started",
    title: "Implementation infrastructure retry",
    detail: "The runner restarted the interrupted policy repass as an infrastructure retry.",
    tone: "active",
    stageId: "implement",
    attempt: 3,
  },
  {
    id: "escalated-11",
    seq: 11,
    time: "19:58:47",
    elapsed: "14:37",
    type: "stage.finished",
    title: "Infrastructure retry complete",
    detail: "The recovered repass improved API coverage, but the portal remained coupled to speculative response shapes.",
    tone: "success",
    stageId: "implement",
    attempt: 3,
  },
  {
    id: "escalated-12",
    seq: 12,
    time: "19:58:49",
    elapsed: "14:39",
    type: "stage.started",
    title: "Review attempt 2",
    detail: "Reviewer began the second pass.",
    tone: "active",
    stageId: "review",
    attempt: 2,
  },
  {
    id: "escalated-13",
    seq: 13,
    time: "20:00:20",
    elapsed: "16:10",
    type: "gate.evaluated",
    title: "Changes requested again",
    detail: "Historical workflow graphs were still reconstructed from mutable current config.",
    tone: "warning",
    stageId: "review-gate",
    attempt: 2,
  },
  {
    id: "escalated-14",
    seq: 14,
    time: "20:00:22",
    elapsed: "16:12",
    type: "stage.started",
    title: "Implementation attempt 4",
    detail: "The final policy repass began.",
    tone: "active",
    stageId: "implement",
    attempt: 4,
  },
  {
    id: "escalated-15",
    seq: 15,
    time: "20:05:55",
    elapsed: "21:45",
    type: "stage.finished",
    title: "Implementation attempt 4 complete",
    detail: "The run produced a final diff and contract notes.",
    tone: "success",
    stageId: "implement",
    attempt: 4,
  },
  {
    id: "escalated-16",
    seq: 16,
    time: "20:05:57",
    elapsed: "21:47",
    type: "stage.started",
    title: "Review attempt 3",
    detail: "Reviewer began the final allowed pass.",
    tone: "active",
    stageId: "review",
    attempt: 3,
  },
  {
    id: "escalated-17",
    seq: 17,
    time: "20:07:12",
    elapsed: "23:02",
    type: "gate.evaluated",
    title: "Repass budget exhausted",
    detail: "The issue remains too broad for one coherent implementation run.",
    tone: "danger",
    stageId: "review-gate",
    attempt: 3,
  },
  {
    id: "escalated-18",
    seq: 18,
    time: "20:07:13",
    elapsed: "23:03",
    type: "run.finished",
    title: "Run escalated",
    detail: "A human must split or re-scope the work before another run.",
    tone: "danger",
    stageId: "review-gate",
  },
];

const completedRunEvents: RunEvent[] = [
  {
    id: "completed-1",
    seq: 1,
    time: "18:10:00",
    elapsed: "0:00",
    type: "run.started",
    title: "Run started",
    detail: "Issue #455 claimed.",
    tone: "neutral",
  },
  {
    id: "completed-2",
    seq: 2,
    time: "18:10:01",
    elapsed: "0:01",
    type: "stage.started",
    title: "Gathering context",
    detail: "Repository and issue state are being pinned.",
    tone: "active",
    stageId: "context",
    attempt: 1,
  },
  {
    id: "completed-3",
    seq: 3,
    time: "18:10:02",
    elapsed: "0:02",
    type: "stage.finished",
    title: "Context pinned",
    detail: "Inputs recorded.",
    tone: "success",
    stageId: "context",
    attempt: 1,
  },
  {
    id: "completed-4",
    seq: 4,
    time: "18:10:04",
    elapsed: "0:04",
    type: "stage.started",
    title: "Implementation started",
    detail: "The implementer entered the pinned worktree.",
    tone: "active",
    stageId: "implement",
    attempt: 1,
  },
  {
    id: "completed-5",
    seq: 5,
    time: "18:17:40",
    elapsed: "7:40",
    type: "stage.finished",
    title: "Implementation complete",
    detail: "Escalation disposition support implemented.",
    tone: "success",
    stageId: "implement",
    attempt: 1,
  },
  {
    id: "completed-6",
    seq: 6,
    time: "18:17:42",
    elapsed: "7:42",
    type: "stage.started",
    title: "Review started",
    detail: "The reviewer began inspecting the implementation.",
    tone: "active",
    stageId: "review",
    attempt: 1,
  },
  {
    id: "completed-7",
    seq: 7,
    time: "18:19:12",
    elapsed: "9:12",
    type: "stage.finished",
    title: "Review completed",
    detail: "The reviewer produced a passing verdict.",
    tone: "success",
    stageId: "review",
    attempt: 1,
  },
  {
    id: "completed-8",
    seq: 8,
    time: "18:19:14",
    elapsed: "9:14",
    type: "gate.evaluated",
    title: "Review passed",
    detail: "No blocking findings.",
    tone: "success",
    stageId: "review-gate",
    attempt: 1,
  },
  {
    id: "completed-9",
    seq: 9,
    time: "18:19:16",
    elapsed: "9:16",
    type: "stage.started",
    title: "Merge started",
    detail: "The approved pull request entered the terminal merge step.",
    tone: "active",
    stageId: "merge",
    attempt: 1,
  },
  {
    id: "completed-10",
    seq: 10,
    time: "18:20:08",
    elapsed: "10:08",
    type: "run.finished",
    title: "Run completed",
    detail: "Pull request #455 merged.",
    tone: "success",
    stageId: "merge",
  },
];

export const runs: Run[] = [
  {
    id: "01JZ441DAEMONAPI",
    shortId: "01JZ...API",
    title: "Daemon read-only HTTP API",
    issue: "#441",
    workflowId: "implementation",
    workflowVersion: 8,
    workflowDigest: "sha256:1df8b863d11a9a3c",
    workflowStages: implementationStages.map((stage) => ({ ...stage })),
    workflowEdges: implementationEdges.map((edge) => ({ ...edge })),
    status: "running",
    startedAt: "Today at 9:12 PM",
    duration: "8m 44s",
    trigger: "Backlog item #441",
    currentStage: "Review",
    repasses: 0,
    events: activeRunEvents,
    attempts: [
      {
        id: "context-1-active",
        stageId: "context",
        number: 1,
        kind: "initial",
        status: "completed",
        duration: "2s",
        startedSeq: 2,
        endedSeq: 3,
        summary: "Pinned the issue, repository revision, and workflow digest.",
        outputs: { workflowDigest: "sha256:1df8...9a3c" },
        artifacts: [
          fixtureArtifact({
            name: "issue-context.json",
            mediaType: "application/json",
            size: "62 B",
            summary: "Normalized issue and repository context.",
            digest: "sha256:dc1dc6c7a33bb8a902588257bcb7eaae63ee1224c2a883e6a72fb0f090cfc63b",
            recordedSeq: 3,
            content: `{"issue":441,"workflow":"implementation","revision":"b74ca21"}`,
          }),
        ],
      },
      {
        id: "implement-1-active",
        stageId: "implement",
        number: 1,
        kind: "initial",
        status: "completed",
        duration: "6m 34s",
        startedSeq: 4,
        endedSeq: 6,
        summary: "Added the initial daemon read endpoints with fixture-backed coverage.",
        outputs: { tests: "18 passed", pullRequest: "#472" },
        artifacts: [
          fixtureArtifact({
            name: "implementation-summary.md",
            mediaType: "text/markdown",
            size: "75 B",
            summary: "Changed files, decisions, and targeted test results.",
            digest: "sha256:853303558ef5e644b0976151fde00115149b76e0f4a06d9879a5b39d01c3d9ea",
            recordedSeq: 5,
            content: "## Implementation\n\nAdded daemon read endpoints and fixture-backed coverage.",
          }),
          fixtureArtifact({
            name: "pull-request.json",
            mediaType: "application/json",
            size: "57 B",
            summary: "Provider reference for PR #472.",
            digest: "sha256:ed9f4b575f7fa5050baf45af58189f30f0dc69e3417e54b7a0d23915844c9c83",
            recordedSeq: 5,
            content: `{"number":472,"state":"open","head":"implementation/441"}`,
          }),
          fixtureArtifact({
            name: "artifact-manifest.json",
            mediaType: "application/json",
            size: "640 B",
            summary: "Digest manifest for the implementation artifacts.",
            digest: "sha256:77c5f9161c7ce06ab18f71b4e78c281f5ec4a1ab668e33514dceb4d37a453dc0",
            recordedSeq: 5,
            contentError: "Artifact content could not be loaded from the local journal.",
          }),
          fixtureArtifact({
            name: "implementation.patch",
            mediaType: "text/x-diff",
            size: "14.7 KB",
            summary: "Raw implementation patch; available as a download only.",
            digest: "sha256:b84792fd08e659b82375c4a71f7679a54e6f783395c542a142f53db6390ebdeb",
            recordedSeq: 5,
            downloadUrl: "/artifacts/01JZ441DAEMONAPI/implementation.patch",
          }),
        ],
      },
      {
        id: "review-1-active",
        stageId: "review",
        number: 1,
        kind: "initial",
        status: "running",
        duration: "2m 04s",
        startedSeq: 7,
        summary: "Review is checking contract coverage and journal containment.",
        artifacts: [],
      },
    ],
  },
  {
    id: "01JZ402DASHBOARD",
    shortId: "01JZ...ARD",
    title: "Live visual dashboard and workflow DAG",
    issue: "#402",
    workflowId: "implementation",
    workflowVersion: 7,
    workflowDigest: "sha256:589d28aa47d62b10",
    workflowStages: implementationStages.map((stage) => ({ ...stage })),
    workflowEdges: implementationEdges.map((edge) => ({ ...edge })),
    status: "escalated",
    startedAt: "Today at 7:44 PM",
    duration: "23m 03s",
    trigger: "Backlog item #402",
    currentStage: "Review gate",
    repasses: 3,
    events: escalatedRunEvents,
    escalation: {
      title: "Scope could not converge within the repass budget",
      cause: "The run repeatedly mixed daemon API, portal retargeting, and graph rendering without producing one coherent vertical slice.",
      gate: "review-gate",
      branch: "@escalate",
      attemptsUsed: 3,
      attemptsAllowed: 3,
    },
    attempts: [
      {
        id: "context-1-escalated",
        stageId: "context",
        number: 1,
        kind: "initial",
        status: "completed",
        duration: "2s",
        startedSeq: 2,
        endedSeq: 3,
        summary: "Pinned the deliberately broad evaluation issue and workflow definition.",
        artifacts: [
          fixtureArtifact({
            name: "issue-context.json",
            mediaType: "application/json",
            size: "50 B",
            summary: "Issue #402 context and acceptance criteria.",
            digest: "sha256:d83b83401c9b0ba19cea572ec0b653b4fb8d26f1e851af3966e7aa157f00544b",
            recordedSeq: 3,
            content: `{"issue":402,"scope":"dashboard and workflow DAG"}`,
          }),
        ],
      },
      {
        id: "implement-1-escalated",
        stageId: "implement",
        number: 1,
        kind: "initial",
        status: "completed",
        duration: "6m 47s",
        startedSeq: 4,
        endedSeq: 5,
        summary: "Produced a partial API and portal client without a complete slice.",
        outputs: { diff: "17 files, +812/-44" },
        artifacts: [
          fixtureArtifact({
            name: "attempt-1-summary.md",
            mediaType: "text/markdown",
            size: "55 B",
            summary: "Initial implementation decisions and test output.",
            digest: "sha256:1f4f30effdb53fa63bf661638c4789caf67ded73bbc72241635e490c4190d64c",
            recordedSeq: 5,
            content: "## Attempt 1\n\nProduced a partial API and portal client.",
          }),
        ],
      },
      {
        id: "implement-2-escalated",
        stageId: "implement",
        number: 2,
        kind: "policy",
        status: "failed",
        duration: "48s",
        startedSeq: 8,
        endedSeq: 9,
        summary: "Runner transport failed before the policy repass produced an outcome.",
        artifacts: [],
      },
      {
        id: "implement-3-escalated",
        stageId: "implement",
        number: 3,
        kind: "infra",
        status: "completed",
        duration: "5m 41s",
        startedSeq: 10,
        endedSeq: 11,
        summary: "Recovered the interrupted policy repass and improved endpoint coverage.",
        outputs: { diff: "21 files, +1044/-81" },
        artifacts: [
          fixtureArtifact({
            name: "attempt-3-summary.md",
            mediaType: "text/markdown",
            size: "86 B",
            summary: "Infrastructure retry changes and unresolved contract notes.",
            digest: "sha256:abb6e88e6dd874fb48c3248b079e896fd456f985b600e7841ee9ba53847a410b",
            recordedSeq: 11,
            content: "## Infrastructure retry\n\nRecovered the policy repass after a runner transport failure.",
          }),
        ],
      },
      {
        id: "implement-4-escalated",
        stageId: "implement",
        number: 4,
        kind: "policy",
        status: "completed",
        duration: "5m 33s",
        startedSeq: 14,
        endedSeq: 15,
        summary: "Recorded the final diff and explicit remaining scope boundaries.",
        outputs: { diff: "23 files, +1182/-96" },
        artifacts: [
          fixtureArtifact({
            name: "attempt-4-summary.md",
            mediaType: "text/markdown",
            size: "69 B",
            summary: "Final attempt summary and remaining blockers.",
            digest: "sha256:d199982c61f40cfcf85c5c816128cdca547fa7df57bbea725cf5c8e9ccce5a07",
            recordedSeq: 15,
            content: "## Attempt 4\n\nRecorded the final diff and remaining scope boundaries.",
          }),
        ],
      },
      {
        id: "review-1-escalated",
        stageId: "review",
        number: 1,
        kind: "initial",
        status: "failed",
        duration: "1m 11s",
        startedSeq: 6,
        endedSeq: 7,
        summary: "Requested a coherent vertical slice.",
        artifacts: [
          fixtureArtifact({
            name: "verdict-1.json",
            mediaType: "application/json",
            size: "44 B",
            summary: "Structured needs-changes verdict.",
            digest: "sha256:fd26626516922e660f47e5054558a09b48a6084e7654b704629f50105f8a8437",
            recordedSeq: 7,
            content: `{"verdict":"needs-changes","reason":"scope"}`,
          }),
        ],
      },
      {
        id: "review-2-escalated",
        stageId: "review",
        number: 2,
        kind: "policy",
        status: "failed",
        duration: "1m 31s",
        startedSeq: 12,
        endedSeq: 13,
        summary: "Flagged mutable reconstruction of historical workflow graphs.",
        artifacts: [
          fixtureArtifact({
            name: "verdict-2.json",
            mediaType: "application/json",
            size: "61 B",
            summary: "Second structured needs-changes verdict.",
            digest: "sha256:1897e345d527a9af4d59985b65884c830d76050165cc10c52a17d7535d7994ad",
            recordedSeq: 13,
            content: `{"verdict":"needs-changes","reason":"mutable workflow graph"}`,
          }),
        ],
      },
      {
        id: "review-3-escalated",
        stageId: "review",
        number: 3,
        kind: "policy",
        status: "failed",
        duration: "1m 15s",
        startedSeq: 16,
        endedSeq: 17,
        summary: "Escalated after the final allowed repass remained over-scoped.",
        artifacts: [
          fixtureArtifact({
            name: "verdict-3.json",
            mediaType: "application/json",
            size: "64 B",
            summary: "Terminal verdict and escalation rationale.",
            digest: "sha256:466ded314fdca25b66706f86b7656170220ac0feb5309609be133e3e281b2dca",
            recordedSeq: 17,
            content: `{"verdict":"needs-changes","terminal":true,"branch":"@escalate"}`,
          }),
        ],
      },
      {
        id: "review-gate-1-escalated",
        stageId: "review-gate",
        number: 1,
        kind: "initial",
        status: "failed",
        duration: "<1s",
        startedSeq: 7,
        endedSeq: 7,
        summary: "Routed the first needs-changes verdict back to implementation.",
        outputs: { target: "implement" },
        artifacts: [
          fixtureArtifact({
            name: "verdict-1.json",
            mediaType: "application/json",
            size: "44 B",
            summary: "Structured needs-changes verdict.",
            digest: "sha256:fd26626516922e660f47e5054558a09b48a6084e7654b704629f50105f8a8437",
            recordedSeq: 7,
            content: `{"verdict":"needs-changes","reason":"scope"}`,
          }),
        ],
      },
      {
        id: "review-gate-2-escalated",
        stageId: "review-gate",
        number: 2,
        kind: "policy",
        status: "failed",
        duration: "<1s",
        startedSeq: 13,
        endedSeq: 13,
        summary: "Routed the second needs-changes verdict back to implementation.",
        outputs: { target: "implement" },
        artifacts: [
          fixtureArtifact({
            name: "verdict-2.json",
            mediaType: "application/json",
            size: "61 B",
            summary: "Second structured needs-changes verdict.",
            digest: "sha256:1897e345d527a9af4d59985b65884c830d76050165cc10c52a17d7535d7994ad",
            recordedSeq: 13,
            content: `{"verdict":"needs-changes","reason":"mutable workflow graph"}`,
          }),
        ],
      },
      {
        id: "review-gate-3-escalated",
        stageId: "review-gate",
        number: 3,
        kind: "policy",
        status: "failed",
        duration: "<1s",
        startedSeq: 17,
        endedSeq: 17,
        summary: "The final needs-changes verdict exhausted the repass budget and selected escalation.",
        outputs: { target: "@escalate" },
        artifacts: [
          fixtureArtifact({
            name: "verdict-3.json",
            mediaType: "application/json",
            size: "64 B",
            summary: "Terminal verdict and escalation rationale.",
            digest: "sha256:466ded314fdca25b66706f86b7656170220ac0feb5309609be133e3e281b2dca",
            recordedSeq: 17,
            content: `{"verdict":"needs-changes","terminal":true,"branch":"@escalate"}`,
          }),
        ],
      },
    ],
  },
  {
    id: "01JZ455ESCALATE",
    shortId: "01JZ...ATE",
    title: "First-class non-retryable escalation disposition",
    issue: "#455",
    workflowId: "implementation",
    workflowVersion: 6,
    workflowDigest: "sha256:ce77c0c1c12930a4",
    workflowStages: implementationStages.map((stage) => ({ ...stage })),
    workflowEdges: implementationEdges.map((edge) => ({ ...edge })),
    status: "completed",
    startedAt: "Today at 6:10 PM",
    duration: "10m 08s",
    trigger: "Backlog item #455",
    currentStage: "Complete",
    repasses: 0,
    events: completedRunEvents,
    attempts: [
      {
        id: "implement-1-complete",
        stageId: "implement",
        number: 1,
        kind: "initial",
        status: "completed",
        duration: "7m 38s",
        startedSeq: 4,
        endedSeq: 5,
        summary: "Implemented and tested the escalation disposition.",
        outputs: { tests: "42 passed" },
        artifacts: [
          fixtureArtifact({
            name: "implementation-summary.md",
            mediaType: "text/markdown",
            size: "68 B",
            summary: "Implementation and verification summary.",
            digest: "sha256:57aed31c2df5acbf843d50c8deb5eb0938f812a643c371b4ccd426eb571e5a83",
            recordedSeq: 5,
            content: "## Implementation\n\nAdded first-class escalation disposition support.",
          }),
        ],
      },
    ],
  },
];

export const instanceWarnings: InstanceWarning[] = [
  {
    code: "VER001",
    title: "One workflow uses an unversioned preview field",
    detail: "backlog-curation uses readiness.maxOpenPRs without an explicit DSL compatibility target.",
  },
];

export function workflowForRun(run: Run): Workflow {
  const workflow = workflows.find((candidate) => candidate.id === run.workflowId);
  if (!workflow) {
    throw new Error(`Missing workflow ${run.workflowId}`);
  }
  return {
    ...workflow,
    version: run.workflowVersion,
    digest: run.workflowDigest,
    stages: run.workflowStages,
    edges: run.workflowEdges,
  };
}

export function runStatusLabel(status: RunStatus): string {
  switch (status) {
    case "running":
      return "Running";
    case "completed":
      return "Completed";
    case "failed":
      return "Failed";
    case "aborted":
      return "Aborted";
    case "escalated":
      return "Needs attention";
  }
}
