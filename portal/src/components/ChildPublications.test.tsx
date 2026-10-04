import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { ChildPublicationSummary } from "../api/types";
import { FixtureDaemonClient } from "../api/fixtureClient";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { ChildPublications } from "./ChildPublications";

const publication: ChildPublicationSummary = { action:"pr", intentDigest:"sha256:abc", state:"effect_pending", head:"goobers/children/child", base:"main", commit:"abc", needsHuman:true, createdAt:"2026-10-04T12:00:00Z", updatedAt:"2026-10-04T12:00:00Z", observation:"pending" };
describe("ChildPublications", () => {
 it("recovers an uncertain check with the same key and distinguishes confirmation from restart", async () => {
  const client = new FixtureDaemonClient(populatedDaemonFixtures());
  const refresh = vi.fn();
  const check = vi.spyOn(client,"checkChildPublication").mockRejectedValueOnce(new Error("lost reply")).mockResolvedValueOnce({runId:"child", requestId:"one", publication:{...publication, state:"confirmed", needsHuman:false, observation:"confirmed", pullRequestUrl:"https://github.com/own/repo/pull/7",pullRequestNumber:7}});
  render(<ChildPublications client={client} runId="child" publications={[publication]} available refresh={refresh} />);
  fireEvent.click(screen.getByRole("button",{name:"Check pull request publication"}));
  fireEvent.click(await screen.findByRole("button",{name:"Retry same publication check"}));
  expect(await screen.findByText("Publication confirmed. This check did not restart the child.")).toBeInTheDocument();
  expect(screen.getByRole("link",{name:"Open published PR #7"})).toHaveAttribute("href","https://github.com/own/repo/pull/7");
  expect(check.mock.calls[1]).toEqual(check.mock.calls[0]);
  expect(check.mock.calls[0][2]).toEqual({action:"pr", expectedIntentDigest:publication.intentDigest});
  await waitFor(()=>expect(refresh).toHaveBeenCalledOnce());
 });
 it("keeps unresolved checks actionable and respects observation access", async () => {
  const client = new FixtureDaemonClient(populatedDaemonFixtures());
  const check = vi.spyOn(client,"checkChildPublication").mockResolvedValue({runId:"child",requestId:"one",publication:{...publication,observation:"branch_changed"}});
  const view=render(<ChildPublications client={client} runId="child" publications={[publication]} available refresh={()=>{}} />);
  fireEvent.click(screen.getByRole("button",{name:"Check pull request publication"}));
  expect(await screen.findByText("The remote branch changed. Human review is still needed.")).toBeInTheDocument();
  expect(screen.getByRole("button",{name:"Check pull request publication"})).toBeEnabled();
  view.rerender(<ChildPublications client={client} runId="child" publications={[publication]} available={false} reason="Your gaggle does not allow this check." refresh={()=>{}} />);
  expect(screen.queryByRole("button")).not.toBeInTheDocument();
  expect(screen.getByText("Your gaggle does not allow this check.")).toBeInTheDocument();
  expect(check).toHaveBeenCalledOnce();
 });
 it("shows prepared intent without offering an effect and rejects unsafe links", () => {
  const client = new FixtureDaemonClient(populatedDaemonFixtures());
  render(<ChildPublications client={client} runId="child" publications={[{...publication,state:"prepared",needsHuman:false,pullRequestUrl:"javascript:alert(1)"}]} available refresh={()=>{}} />);
  expect(screen.getByText("Prepared; no effect begun")).toBeInTheDocument();
  expect(screen.queryByRole("button")).not.toBeInTheDocument();
  expect(screen.queryByRole("link")).not.toBeInTheDocument();
 });
});
