import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterAll, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import type {
  AttemptList,
  ArtifactContent,
  DaemonClient,
  StageAttempt,
  WorkflowGraphNode,
} from "../api/types";
import styles from "../styles.css?inline";
import tokens from "../tokens.css?inline";
import { RunStageInspector } from "./RunStageInspector";

const reviewNode: WorkflowGraphNode = { id: "review", kind: "gate", evaluator: "agentic" };

function attempt(overrides: Partial<StageAttempt>): StageAttempt {
  return {
    number: 1,
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
    render(
      <RunStageInspector client={stubClient([])} node={undefined} runId="run-1" selectedSeq={9} />,
    );
    expect(screen.getByText("Select a node")).toBeInTheDocument();
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
    render(<RunStageInspector client={client} node={reviewNode} runId="run-1" selectedSeq={9} />);

    expect(await screen.findByText("success")).toBeInTheDocument();
    expect(screen.getByText("approve")).toBeInTheDocument();
    expect(screen.getByText("rationale.md")).toBeInTheDocument();
    expect(screen.getByText("sha256:abc")).toBeInTheDocument();
    expect(client.listStageAttempts).toHaveBeenCalledWith("run-1", "review", expect.anything());
  });

  it("only shows attempts started by the selected sequence", async () => {
    const client = stubClient([
      attempt({ number: 1, startedSeq: 1, finishedSeq: 2 }),
      attempt({ number: 2, startedSeq: 8, finishedSeq: 9 }),
    ]);
    render(<RunStageInspector client={client} node={reviewNode} runId="run-1" selectedSeq={5} />);

    // Attempt 2 started at seq 8, after the playhead at 5 — it must not appear.
    await waitFor(() => expect(screen.queryByText("Attempt 2")).not.toBeInTheDocument());
    // With a single visible attempt the switcher is not rendered at all.
    expect(screen.queryByText("Attempt 1")).not.toBeInTheDocument();
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
    render(<RunStageInspector client={client} node={reviewNode} runId="run-1" selectedSeq={9} />);

    fireEvent.click(await screen.findByRole("button", { name: "View content" }));
    expect(await screen.findByText(body)).toBeInTheDocument();
    expect(client.getArtifact).toHaveBeenCalledWith("run-1", "sha256:abc");
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
    render(<RunStageInspector client={client} node={reviewNode} runId="run-1" selectedSeq={9} />);

    fireEvent.click(await screen.findByRole("button", { name: "View content" }));
    const preview = await screen.findByText(body);
    const initialColors = expectPreviewColors(preview, {
      foreground: "#f2eff8",
      background: "#25242b",
    });
    expect(window.getComputedStyle(preview).whiteSpace).toBe("pre-wrap");
    expect(window.getComputedStyle(preview).overflow).toBe("auto");
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
});
