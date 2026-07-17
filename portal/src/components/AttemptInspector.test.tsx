import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { runs } from "../prototypeData";
import { AttemptInspector } from "./AttemptInspector";

const activeRun = runs.find((run) => run.id === "01JZ441DAEMONAPI")!;
const activeImplementStage = activeRun.workflowStages.find((stage) => stage.id === "implement")!;
const activeReviewStage = activeRun.workflowStages.find((stage) => stage.id === "review")!;

const escalatedRun = runs.find((run) => run.id === "01JZ455ESCALATE" || run.title.includes("dashboard"))!;
const escalatedImplementStage = escalatedRun.workflowStages.find((stage) => stage.id === "implement")!;

describe("AttemptInspector", () => {
  it("shows a not-reached state before the stage has any visible attempt", () => {
    render(<AttemptInspector eventSeq={0} run={activeRun} stage={activeImplementStage} />);

    expect(screen.getByText("Not reached at this point")).toBeInTheDocument();
    expect(screen.queryByRole("group", { name: "Stage attempts" })).not.toBeInTheDocument();
  });

  it("filters out attempts that have not started by the given event sequence", () => {
    render(<AttemptInspector eventSeq={6} run={escalatedRun} stage={escalatedImplementStage} />);

    expect(screen.getByRole("button", { name: "Attempt 1" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Attempt 2" })).not.toBeInTheDocument();
  });

  it("distinguishes and orders initial, policy, and infra attempts", () => {
    render(<AttemptInspector eventSeq={13} run={escalatedRun} stage={escalatedImplementStage} />);

    const buttons = screen.getAllByRole("button", { name: /^Attempt \d$/ });
    expect(buttons.map((button) => button.textContent)).toEqual(["Attempt 1", "Attempt 2", "Attempt 3"]);
  });

  it("selects the attempt matching the given event's attempt number", () => {
    render(
      <AttemptInspector eventAttemptNumber={2} eventSeq={9} run={escalatedRun} stage={escalatedImplementStage} />,
    );

    expect(screen.getByRole("button", { name: "Attempt 2" })).toHaveAttribute("aria-pressed", "true");
  });

  it("supports explicit attempt switching by click", async () => {
    const user = userEvent.setup();
    render(<AttemptInspector eventSeq={13} run={escalatedRun} stage={escalatedImplementStage} />);

    await user.click(screen.getByRole("button", { name: "Attempt 1" }));
    expect(screen.getByRole("button", { name: "Attempt 1" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: "Attempt 3" })).toHaveAttribute("aria-pressed", "false");
  });

  it("supports keyboard operation across the attempt switcher", async () => {
    const user = userEvent.setup();
    render(<AttemptInspector eventSeq={13} run={escalatedRun} stage={escalatedImplementStage} />);

    screen.getByRole("button", { name: "Attempt 1" }).focus();
    await user.keyboard("{ArrowRight}");
    expect(screen.getByRole("button", { name: "Attempt 2" })).toHaveFocus();
    expect(screen.getByRole("button", { name: "Attempt 2" })).toHaveAttribute("aria-pressed", "true");

    await user.keyboard("{End}");
    expect(screen.getByRole("button", { name: "Attempt 3" })).toHaveFocus();

    await user.keyboard("{Home}");
    expect(screen.getByRole("button", { name: "Attempt 1" })).toHaveFocus();
  });

  it("shows a no-artifact state for an attempt that recorded none", () => {
    render(<AttemptInspector eventSeq={7} run={activeRun} stage={activeReviewStage} />);

    expect(screen.getByText("No artifacts recorded yet.")).toBeInTheDocument();
  });

  it("shows provenance, media type, size, and digest metadata for each artifact", () => {
    render(<AttemptInspector eventSeq={6} run={activeRun} stage={activeImplementStage} />);

    const row = screen.getByText("pull-request.json").closest("article")!;
    expect(within(row).getByText("application/json")).toBeInTheDocument();
    expect(within(row).getByText("1.2 KB")).toBeInTheDocument();
    expect(within(row).getByText(/Attempt 1 · Seq 6/)).toBeInTheDocument();
    expect(within(row).getByText("Verified")).toBeInTheDocument();
  });

  it("renders safe JSON artifact content re-indented, in a dialog with focus trap", async () => {
    const user = userEvent.setup();
    render(<AttemptInspector eventSeq={6} run={activeRun} stage={activeImplementStage} />);

    const row = screen.getByText("pull-request.json").closest("article")!;
    await user.click(within(row).getByRole("button", { name: "View content" }));

    const dialog = await screen.findByRole("dialog", { name: undefined });
    expect(within(dialog).getByRole("heading", { name: "pull-request.json" })).toBeInTheDocument();
    expect(await within(dialog).findByLabelText("pull-request.json content")).toHaveTextContent(
      /"number": 472/,
    );
  });

  it("renders safe markdown/plain artifact content verbatim", async () => {
    const user = userEvent.setup();
    render(<AttemptInspector eventSeq={6} run={activeRun} stage={activeImplementStage} />);

    const row = screen.getByText("implementation-summary.md").closest("article")!;
    await user.click(within(row).getByRole("button", { name: "View content" }));

    expect(await screen.findByLabelText("implementation-summary.md content")).toHaveTextContent(
      "Added daemon read endpoints and fixture-backed coverage.",
    );
  });

  it("shows an artifact error state with a retry action", async () => {
    const user = userEvent.setup();
    render(<AttemptInspector eventSeq={6} run={activeRun} stage={activeImplementStage} />);

    const row = screen.getByText("artifact-manifest.json").closest("article")!;
    await user.click(within(row).getByRole("button", { name: "View content" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Artifact content could not be loaded from the local journal.",
    );
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
  });

  it("offers a download action, not a preview, for unsupported media with a download URL", () => {
    render(<AttemptInspector eventSeq={6} run={activeRun} stage={activeImplementStage} />);

    const row = screen.getByText("implementation.patch").closest("article")!;
    expect(within(row).getByRole("link", { name: "Download" })).toBeInTheDocument();
    expect(within(row).queryByRole("button", { name: "View content" })).not.toBeInTheDocument();
  });

  it("shows metadata-only access for unsupported media with no download URL", () => {
    render(<AttemptInspector eventSeq={6} run={activeRun} stage={activeImplementStage} />);

    const row = screen.getByText("coverage.bin").closest("article")!;
    expect(within(row).getByText("Metadata only")).toBeInTheDocument();
    expect(within(row).queryByRole("button", { name: "View content" })).not.toBeInTheDocument();
    expect(within(row).queryByRole("link", { name: "Download" })).not.toBeInTheDocument();
  });

  it("keeps definition context separate from attempt context", () => {
    render(<AttemptInspector eventSeq={6} run={activeRun} stage={activeImplementStage} />);

    const disclosure = screen.getByText("Stage definition").closest("details")!;
    expect(within(disclosure).getByText(activeImplementStage.description)).toBeInTheDocument();
    expect(disclosure.querySelector(".code-block")?.textContent).toBe(activeImplementStage.yaml);
  });

  it("closes the artifact viewer on Escape and returns focus to the trigger", async () => {
    const user = userEvent.setup();
    render(<AttemptInspector eventSeq={6} run={activeRun} stage={activeImplementStage} />);

    const row = screen.getByText("pull-request.json").closest("article")!;
    const trigger = within(row).getByRole("button", { name: "View content" });
    await user.click(trigger);
    await screen.findByRole("dialog");

    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  });
});
