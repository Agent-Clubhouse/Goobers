import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactElement } from "react";
import { FixtureDaemonClient } from "../api/fixtureClient";
import { LiveDataProvider } from "../liveData";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { afterAll, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import type {
  AttemptList,
  ArtifactContent,
  DaemonClient,
  RequestOptions,
  RunEvent,
  StageAttempt,
  TranscriptContent,
  WorkflowGraphNode,
} from "../api/types";
import { goWireFixtures } from "../api/wire.generated";
import styles from "../styles.css?inline";
import tokens from "../tokens.css?inline";
import { RunStageInspector } from "./RunStageInspector";

/**
 * The inspector subscribes to live data now (#1714), so it needs a provider.
 *
 * A local wrapper rather than a shared helper: this is the only component test
 * that needs one, and a fixture client's event stream never emits, so the
 * subscription is inert here — these tests are about rendering, and the live
 * refresh has its own test in RunStageInspectorLive.test.tsx.
 */
function renderInspector(ui: ReactElement) {
  const client = new FixtureDaemonClient(populatedDaemonFixtures());
  const wrap = (node: ReactElement) => (
    <LiveDataProvider client={client}>{node}</LiveDataProvider>
  );
  const view = render(wrap(ui));
  // rerender must re-wrap. Without this it renders the bare component and the
  // provider disappears mid-test, which fails with the same "requires a
  // LiveDataProvider" error as having no wrapper at all.
  return { ...view, rerender: (node: ReactElement) => view.rerender(wrap(node)) };
}


function transcriptEvidence(seq: number): RunEvent {
  return {
    schema: "v1",
    seq,
    type: "span.recorded",
    branch: 0,
    time: "2026-07-18T12:36:57Z",
    knownSchema: true,
    stage: "review",
    name: "transcript",
  };
}

const reviewNode: WorkflowGraphNode = { id: "review", kind: "gate", evaluator: "agentic" };
const implementNode: WorkflowGraphNode = { id: "implement", kind: "agentic" };
const reviewerRepassEvents: RunEvent[] = [
  {
    schema: "v1",
    seq: 9,
    type: "gate.evaluated",
    branch: 0,
    time: "2026-07-18T12:36:57Z",
    knownSchema: true,
    gate: "review",
    verdict: "needs-changes",
    target: "implement",
  },
];

function attempt(overrides: Partial<StageAttempt>): StageAttempt {
  const visit = overrides.visit ?? 1;
  const number = overrides.number ?? 1;
  return {
    id: overrides.id ?? `sta-${visit}-${number}`,
    visit,
    number,
    class: "initial",
    status: "success",
    startedSeq: 1,
    finishedSeq: 2,
    durationMillis: 1500,
    artifacts: [],
    ...overrides,
  };
}

function stubClient(
  attempts: StageAttempt[],
  artifact?: ArtifactContent,
): DaemonClient {
  return {
    listStageAttempts: vi.fn(
      async (): Promise<AttemptList> => ({ runId: "run-1", stage: "review", attempts }),
    ),
    getArtifact: vi.fn(async (): Promise<ArtifactContent> => {
      if (!artifact) {
        throw new Error("no artifact");
      }
      return artifact;
    }),
  } as unknown as DaemonClient;
}

function resolveComputedColor(element: Element, property: "background" | "color"): string {
  const computed = window.getComputedStyle(element).getPropertyValue(property).trim();
  const customProperty = computed.match(/^var\((--[\w-]+)\)$/)?.[1];
  const resolved = customProperty
    ? window.getComputedStyle(document.documentElement).getPropertyValue(customProperty).trim()
    : computed;

  if (!/^#[\da-f]{6}$/i.test(resolved)) {
    throw new Error(`Expected ${property} to resolve to a six-digit hex color, received "${resolved}".`);
  }
  return resolved;
}

function contrastRatio(foreground: string, background: string): number {
  const luminance = (color: string) => {
    const channels = color
      .slice(1)
      .match(/.{2}/g)!
      .map((channel) => Number.parseInt(channel, 16) / 255)
      .map((channel) =>
        channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4,
      );
    return 0.2126 * channels[0] + 0.7152 * channels[1] + 0.0722 * channels[2];
  };
  const foregroundLuminance = luminance(foreground);
  const backgroundLuminance = luminance(background);
  const lighter = Math.max(foregroundLuminance, backgroundLuminance);
  const darker = Math.min(foregroundLuminance, backgroundLuminance);
  return (lighter + 0.05) / (darker + 0.05);
}

function expectPreviewColors(
  preview: HTMLElement,
  expected: { foreground: string; background: string },
) {
  const colors = {
    foreground: resolveComputedColor(preview, "color"),
    background: resolveComputedColor(preview, "background"),
  };
  expect(colors).toEqual(expected);
  expect(contrastRatio(colors.foreground, colors.background)).toBeGreaterThanOrEqual(4.5);
  return colors;
}

describe("run stage inspector", () => {
  const portalStyles = document.createElement("style");

  beforeAll(() => {
    portalStyles.textContent = `${tokens}\n${styles}`;
    document.head.append(portalStyles);
  });

  afterAll(() => {
    portalStyles.remove();
  });

  beforeEach(() => {
    delete document.documentElement.dataset.theme;
  });

  it("prompts to select a node when none is chosen", () => {
    renderInspector(<RunStageInspector client={stubClient([])} node={undefined} runId="run-1" selectedSeq={9} />,
    );
    expect(screen.getByText("Select a node")).toBeInTheDocument();
  });

  it("labels the inspector with the run's workflow and the stage's owning goober (#2538)", async () => {
    const ownedNode: WorkflowGraphNode = {
      id: "review",
      kind: "gate",
      evaluator: "agentic",
      owner: "reviewer-goober",
    };
    renderInspector(
      <RunStageInspector
        client={stubClient([attempt({ number: 1, status: "success" })])}
        node={ownedNode}
        runId="run-1"
        selectedSeq={9}
        workflow="implementation"
      />,
    );

    expect(
      screen.getByRole("complementary", { name: "implementation · review attempt inspector" }),
    ).toBeInTheDocument();
    expect(screen.getByText("implementation · gate")).toBeInTheDocument();
    expect(screen.getByText("Owned by reviewer-goober")).toBeInTheDocument();
  });

  it("omits workflow and owner chrome when neither is known", () => {
    renderInspector(
      <RunStageInspector client={stubClient([])} node={reviewNode} runId="run-1" selectedSeq={9} />,
    );

    expect(
      screen.getByRole("complementary", { name: "review attempt inspector" }),
    ).toBeInTheDocument();
    expect(screen.queryByText(/^Owned by/)).not.toBeInTheDocument();
  });

  it("loads and shows the current attempt's status, output, and artifact metadata", async () => {
    const client = stubClient([
      attempt({
        number: 1,
        status: "success",
        outputs: { verdict: "approve" },
        artifacts: [{ name: "rationale.md", digest: "sha256:abc", size: 42, mediaType: "text/markdown", recordedSeq: 2 }],
      }),
    ]);
    renderInspector(<RunStageInspector client={client} node={reviewNode} runId="run-1" selectedSeq={9} />);

    expect(await screen.findByText("success")).toBeInTheDocument();
    expect(screen.getByText("approve")).toBeInTheDocument();
    expect(screen.getByText("rationale.md")).toBeInTheDocument();
    expect(screen.getByText("sha256:abc")).toBeInTheDocument();
    expect(client.listStageAttempts).toHaveBeenCalledWith("run-1", "review", expect.anything());
  });

  it("shows the requested model when the telemetry rollup has indexed it (#1550)", async () => {
    const client = stubClient([attempt({ number: 1, status: "success", model: "auto" })]);
    renderInspector(<RunStageInspector client={client} node={reviewNode} runId="run-1" selectedSeq={9} />);

    expect(await screen.findByText("model: auto")).toBeInTheDocument();
  });

  it("omits the model line when telemetry has not indexed one", async () => {
    const client = stubClient([attempt({ number: 1, status: "success" })]);
    renderInspector(<RunStageInspector client={client} node={reviewNode} runId="run-1" selectedSeq={9} />);

    await screen.findByText("success");
    expect(screen.queryByText(/^model:/)).not.toBeInTheDocument();
  });

  it("shows placement provenance when the attempt's journal recorded it (#3515)", async () => {
    const client = stubClient([
      attempt({
        number: 1,
        status: "success",
        placement: {
          runner: "linux-large",
          node: "aks-linux-0001",
          host: "goobers-stage-review-4x2vq",
          os: "linux",
          pod: "goobers-stage-review-4x2vq",
          queuedAt: "2026-08-22T10:00:00Z",
          podStartedAt: "2026-08-22T10:00:09Z",
        },
      }),
    ]);
    renderInspector(<RunStageInspector client={client} node={reviewNode} runId="run-1" selectedSeq={9} />);

    expect(await screen.findByText("runner: linux-large")).toBeInTheDocument();
    expect(screen.getByText("node: aks-linux-0001")).toBeInTheDocument();
    expect(screen.getByText("pod: goobers-stage-review-4x2vq")).toBeInTheDocument();
    expect(screen.getByText("queue wait: 9s")).toBeInTheDocument();
    // The pod's hostname is redundant once a real node is known.
    expect(screen.queryByText(/^host:/)).not.toBeInTheDocument();
  });

  it("labels a local attempt's hostname as a host, never as a node (#3515)", async () => {
    const client = stubClient([
      attempt({
        number: 1,
        status: "success",
        placement: { runner: "self", host: "build-box-07", os: "darwin" },
      }),
    ]);
    renderInspector(<RunStageInspector client={client} node={reviewNode} runId="run-1" selectedSeq={9} />);

    expect(await screen.findByText("host: build-box-07")).toBeInTheDocument();
    expect(screen.queryByText(/^node:/)).not.toBeInTheDocument();
  });

  it("omits the placement row for journals recorded before provenance existed", async () => {
    const client = stubClient([attempt({ number: 1, status: "success" })]);
    renderInspector(<RunStageInspector client={client} node={reviewNode} runId="run-1" selectedSeq={9} />);

    await screen.findByText("success");
    expect(screen.queryByLabelText("Attempt placement")).not.toBeInTheDocument();
  });

  it("only shows attempts started by the selected sequence", async () => {
    const client = stubClient([
      attempt({ number: 1, startedSeq: 1, finishedSeq: 2 }),
      attempt({ number: 2, startedSeq: 8, finishedSeq: 9 }),
    ]);
    renderInspector(<RunStageInspector client={client} node={reviewNode} runId="run-1" selectedSeq={5} />);

    // Attempt 2 started at seq 8, after the playhead at 5 — it must not appear.
    await waitFor(() => expect(screen.queryByText("Attempt 2")).not.toBeInTheDocument());
    // With a single visible attempt the switcher is not rendered at all.
    expect(screen.queryByRole("group", { name: "Stage visits" })).not.toBeInTheDocument();
  });

  it("groups the generated reviewer-repass fixture into visits and nested retries", async () => {
    const consoleError = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const fixtureAttempts = goWireFixtures.stageAttempts.attempts;
      const repeatedInitials = fixtureAttempts.filter(
        (candidate) => candidate.number === 1 && candidate.visit <= 2,
      );
      expect(repeatedInitials).toHaveLength(2);
      expect(new Set(repeatedInitials.map((candidate) => candidate.id)).size).toBe(2);

      const client = stubClient(fixtureAttempts);
      renderInspector(<RunStageInspector
          client={client}
          events={reviewerRepassEvents}
          node={implementNode}
          runId="run-123"
          selectedSeq={13}
        />,
      );

      const visits = await screen.findByRole("group", { name: "Stage visits" });
      const visit1 = within(visits).getByRole("button", { name: "Visit 1" });
      const visit2 = within(visits).getByRole("button", { name: "Visit 2" });
      expect(visit2).toHaveAttribute("aria-pressed", "true");

      let retries = screen.getByRole("group", { name: "Visit 2 attempts" });
      const repassAttempt = within(retries).getByRole("button", {
        name: "Visit 2 · Attempt 1",
      });
      const infraRetry = within(retries).getByRole("button", {
        name: "Visit 2 · Attempt 2 (infra retry)",
      });
      expect(infraRetry).toHaveAttribute("aria-pressed", "true");
      expect(screen.getByText("2m 0s")).toBeInTheDocument();
      expect(screen.getByText("success")).toBeInTheDocument();
      expect(screen.getByText("Review returned needs-changes and selected implement.")).toBeInTheDocument();

      fireEvent.click(repassAttempt);
      expect(screen.getByText("repass_failed: first repass failed")).toBeInTheDocument();

      fireEvent.click(visit1);
      expect(screen.queryByText(/Repass decision/)).not.toBeInTheDocument();
      expect(screen.getByText("implemented")).toBeInTheDocument();
      expect(screen.getByText("result")).toBeInTheDocument();
      expect(screen.getByText("Visit 1 · Attempt 2 · Seq 8")).toBeInTheDocument();

      visit1.focus();
      fireEvent.keyDown(visit1, { key: "ArrowRight" });
      expect(visit2).toHaveFocus();
      expect(visit2).toHaveAttribute("aria-pressed", "true");

      retries = screen.getByRole("group", { name: "Visit 2 attempts" });
      const selectedInfraRetry = within(retries).getByRole("button", {
        name: "Visit 2 · Attempt 2 (infra retry)",
      });
      selectedInfraRetry.focus();
      fireEvent.keyDown(selectedInfraRetry, { key: "ArrowLeft" });
      expect(
        within(retries).getByRole("button", { name: "Visit 2 · Attempt 1" }),
      ).toHaveFocus();
      expect(screen.getByText("repass_failed: first repass failed")).toBeInTheDocument();

      expect(consoleError.mock.calls.flat().join(" ")).not.toMatch(
        /same key|unique ["']key["']/i,
      );
    } finally {
      consoleError.mockRestore();
    }
  });

  it("reveals a repass visit only after its traversal starts", async () => {
    const client = stubClient(goWireFixtures.stageAttempts.attempts);
    const view = renderInspector(<RunStageInspector
        client={client}
        events={reviewerRepassEvents}
        node={implementNode}
        runId="run-123"
        selectedSeq={9}
      />,
    );

    const visits = await screen.findByRole("group", { name: "Stage visits" });
    expect(within(visits).queryByRole("button", { name: "Visit 2" })).not.toBeInTheDocument();

    view.rerender(
      <RunStageInspector
        client={client}
        events={reviewerRepassEvents}
        node={implementNode}
        runId="run-123"
        selectedSeq={10}
      />,
    );
    expect(
      await within(screen.getByRole("group", { name: "Stage visits" })).findByRole("button", {
        name: "Visit 2",
      }),
    ).toBeInTheDocument();
  });

  it.each([
    ["plain text", "text/plain", "plain text preview"],
    ["JSON", "application/json", '{"preview":"json"}'],
    ["YAML", "application/yaml", "preview: yaml"],
    ["Markdown", "text/markdown", "# Markdown preview"],
  ])("fetches and previews %s artifact bodies on demand", async (_format, mediaType, body) => {
    const bytes = new TextEncoder().encode(body).buffer;
    const client = stubClient(
      [
        attempt({
          artifacts: [{ name: "preview", digest: "sha256:abc", size: body.length, mediaType }],
        }),
      ],
      { digest: "sha256:abc", mediaType, size: body.length, etag: null, bytes },
    );
    renderInspector(<RunStageInspector client={client} node={reviewNode} runId="run-1" selectedSeq={9} />);

    fireEvent.click(await screen.findByRole("button", { name: "View content" }));
    const preview = await screen.findByText(body);
    expect(preview).toBeInTheDocument();
    expect(preview.className).not.toContain("artifact-content-bounded");
    expect(client.getArtifact).toHaveBeenCalledWith("run-1", "sha256:abc", {
      signal: expect.any(AbortSignal),
    });
  });

  it("caps an oversized artifact preview with an internal scroll bound (#fix-artifact-windowing)", async () => {
    const body = "x".repeat(20_001);
    const bytes = new TextEncoder().encode(body).buffer;
    const client = stubClient(
      [
        attempt({
          artifacts: [{ name: "big.log", digest: "sha256:big", size: body.length, mediaType: "text/plain" }],
        }),
      ],
      { digest: "sha256:big", mediaType: "text/plain", size: body.length, etag: null, bytes },
    );
    renderInspector(<RunStageInspector client={client} node={reviewNode} runId="run-1" selectedSeq={9} />);

    fireEvent.click(await screen.findByRole("button", { name: "View content" }));
    await waitFor(() => {
      expect(document.querySelector(".artifact-content")).not.toBeNull();
    });
    const preview = document.querySelector(".artifact-content") as HTMLElement;

    expect(preview.className).toContain("artifact-content-bounded");
    expect(window.getComputedStyle(preview).maxHeight).toBe("320px");
    expect(window.getComputedStyle(preview).overflowY).toBe("auto");
  });

  it("keeps an open artifact preview readable across initial, light, and dark themes", async () => {
    const body = "artifact preview contrast";
    const bytes = new TextEncoder().encode(body).buffer;
    const client = stubClient(
      [
        attempt({
          artifacts: [{ name: "preview.txt", digest: "sha256:abc", size: body.length, mediaType: "text/plain" }],
        }),
      ],
      { digest: "sha256:abc", mediaType: "text/plain", size: body.length, etag: null, bytes },
    );
    renderInspector(<RunStageInspector client={client} node={reviewNode} runId="run-1" selectedSeq={9} />);

    fireEvent.click(await screen.findByRole("button", { name: "View content" }));
    const preview = await screen.findByText(body);
    const initialColors = expectPreviewColors(preview, {
      foreground: "#f2eff8",
      background: "#25242b",
    });
    expect(window.getComputedStyle(preview).whiteSpace).toBe("pre-wrap");
    // #1457 makes the preview overflow visible (no inner scroll/clip) so the
    // sticky stage inspector no longer traps expanded artifact content; the
    // earlier auto value is superseded.
    expect(window.getComputedStyle(preview).overflow).toBe("visible");
    expect(window.getComputedStyle(preview).wordBreak).toBe("break-word");

    document.documentElement.dataset.theme = "light";
    expectPreviewColors(preview, initialColors);

    document.documentElement.dataset.theme = "dark";
    const darkColors = expectPreviewColors(preview, {
      foreground: "#eeebf5",
      background: "#0d0d11",
    });
    expect(darkColors).not.toEqual(initialColors);
    expect(preview).toBeInTheDocument();
  });
  describe("cancels evidence loads when the view goes away (#3665)", () => {
    function pendingClient(): {
      client: DaemonClient;
      artifactSignals: AbortSignal[];
      transcriptSignals: AbortSignal[];
    } {
      const artifactSignals: AbortSignal[] = [];
      const transcriptSignals: AbortSignal[] = [];
      const client = {
        listStageAttempts: vi.fn(
          async (): Promise<AttemptList> => ({
            runId: "run-1",
            stage: "review",
            attempts: [
              attempt({
                artifacts: [
                  {
                    name: "preview",
                    digest: "sha256:abc",
                    size: 4,
                    mediaType: "text/plain",
                  },
                ],
              }),
            ],
          }),
        ),
        getArtifact: vi.fn(async (_runId: string, _digest: string, options?: RequestOptions) => {
          artifactSignals.push(options!.signal!);
          return new Promise<ArtifactContent>(() => {});
        }),
        getTranscript: vi.fn(async (_runId: string, _seq: number, options?: RequestOptions) => {
          transcriptSignals.push(options!.signal!);
          return new Promise<TranscriptContent>(() => {});
        }),
      } as unknown as DaemonClient;
      return { client, artifactSignals, transcriptSignals };
    }

    it("aborts an in-flight artifact download when the inspector unmounts", async () => {
      const { client, artifactSignals } = pendingClient();
      const view = renderInspector(
        <RunStageInspector client={client} node={reviewNode} runId="run-1" selectedSeq={9} />,
      );

      fireEvent.click(await screen.findByRole("button", { name: "View content" }));
      await waitFor(() => expect(artifactSignals).toHaveLength(1));
      expect(artifactSignals[0].aborted).toBe(false);

      view.unmount();

      expect(artifactSignals[0].aborted).toBe(true);
    });

    it("aborts an in-flight transcript download when the selected evidence changes", async () => {
      const { client, transcriptSignals } = pendingClient();
      const view = renderInspector(
        <RunStageInspector
          client={client}
          node={reviewNode}
          runId="run-1"
          selectedEvidence={transcriptEvidence(9)}
          selectedSeq={9}
        />,
      );

      fireEvent.click(await screen.findByRole("button", { name: "View transcript" }));
      await waitFor(() => expect(transcriptSignals).toHaveLength(1));

      view.rerender(
        <RunStageInspector
          client={client}
          node={reviewNode}
          runId="run-1"
          selectedEvidence={transcriptEvidence(11)}
          selectedSeq={11}
        />,
      );

      expect(transcriptSignals[0].aborted).toBe(true);
      expect(screen.queryByRole("alert")).not.toBeInTheDocument();
      expect(await screen.findByRole("button", { name: "View transcript" })).toBeInTheDocument();
    });

    it("reports no error when an aborted artifact download rejects", async () => {
      const artifactSignals: AbortSignal[] = [];
      let rejectLoad: ((reason: Error) => void) | undefined;
      const client = {
        listStageAttempts: vi.fn(
          async (): Promise<AttemptList> => ({
            runId: "run-1",
            stage: "review",
            attempts: [
              attempt({
                artifacts: [
                  { name: "preview", digest: "sha256:abc", size: 4, mediaType: "text/plain" },
                ],
              }),
            ],
          }),
        ),
        getArtifact: vi.fn(async (_runId: string, _digest: string, options?: RequestOptions) => {
          artifactSignals.push(options!.signal!);
          return new Promise<ArtifactContent>((_resolve, reject) => {
            rejectLoad = reject;
          });
        }),
      } as unknown as DaemonClient;
      const view = renderInspector(
        <RunStageInspector client={client} node={reviewNode} runId="run-1" selectedSeq={9} />,
      );

      fireEvent.click(await screen.findByRole("button", { name: "View content" }));
      await waitFor(() => expect(artifactSignals).toHaveLength(1));

      view.rerender(
        <RunStageInspector client={client} node={reviewNode} runId="run-2" selectedSeq={9} />,
      );
      expect(artifactSignals[0].aborted).toBe(true);
      rejectLoad!(new Error("The user aborted a request."));

      expect(await screen.findByRole("button", { name: "View content" })).toBeInTheDocument();
      expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    });
  });
});
