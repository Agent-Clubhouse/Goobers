import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { RunAttributionResult } from "../api/types";
import { goWireFixtures } from "../api/wire.generated";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { LiveDataProvider } from "../liveData";
import { RunAttributionPanel } from "./RunAttributionPanel";

function mount(value: RunAttributionResult = structuredClone(goWireFixtures.runAttribution)) {
 const fixtures = populatedDaemonFixtures(); fixtures.runAttribution = {"run-123": value};
 const client = new FixtureDaemonClient(fixtures); const navigate = vi.fn();
 render(<LiveDataProvider client={client}><RunAttributionPanel client={client} runId="run-123" gaggle="core" navigate={navigate} /></LiveDataProvider>);
 return {navigate,client};
}
describe("recorded Backprop attribution",()=>{
 it("preserves signed scores, unsigned share and exact event navigation",async()=>{
  const {navigate}=mount();
  expect(await screen.findByText("-0.600")).toBeInTheDocument();
  expect(screen.getByText("0.600")).toBeInTheDocument();
  expect(screen.getByText(/Run phase: failed/)).toBeInTheDocument();
  expect(screen.getByText(/incorrect · human/)).toBeInTheDocument();
  fireEvent.click(screen.getByText("Implement · stage"));
  fireEvent.click(screen.getByRole("button",{name:"Open exact event 9"}));
  expect(navigate).toHaveBeenCalledWith({page:"run",id:"run-123",tab:"journal",seq:9,event:true});
 });
 it.each(["absent","pending","unsupported","limit-exceeded"] as const)("discloses %s without a successful analysis",async availability=>{
  mount({...goWireFixtures.runAttribution,availability,record:undefined});
  await screen.findByText(/Read /);
  expect(screen.queryByRole("table")).not.toBeInTheDocument();
  expect(screen.queryByText("Loading run attribution…")).not.toBeInTheDocument();
 });
 it("rejects a response belonging to another run",async()=>{
  mount({...goWireFixtures.runAttribution,runId:"foreign"});
  expect(await screen.findByRole("alert")).toHaveTextContent("mismatched run attribution");
  expect(screen.queryByRole("table")).not.toBeInTheDocument();
 });
 it("renders untrusted labels as text and does not make span artifact links",async()=>{
  const value=structuredClone(goWireFixtures.runAttribution) as RunAttributionResult;
  value.record!.attribution.contributions=[{...value.record!.attribution.contributions![0],label:"<img src=x onerror=alert(1)>"}];
  value.evidence=[{index:0,kind:"span",status:"unavailable"}];
  mount(value);await screen.findByText("<img src=x onerror=alert(1)> · stage");
  expect(document.querySelector("img[src=x]")).toBeNull();
  expect(screen.queryByRole("button",{name:"Inspect artifact"})).not.toBeInTheDocument();
 });
 it("shows recorded failure without confusing it with missing attribution",async()=>{
  const value=structuredClone(goWireFixtures.runAttribution) as RunAttributionResult;
  value.record!.status="failed";value.record!.failure="analysis unavailable";value.record!.attribution.contributions=null;mount(value);
  expect(await screen.findByRole("alert")).toHaveTextContent("Recorded analysis failure: analysis unavailable");
 });
});

it("indexes a large evidence set and bounds rendered details",async()=>{
 const value=structuredClone(goWireFixtures.runAttribution) as RunAttributionResult;
 const link=value.record!.evidence![0];
 value.record!.evidence=Array.from({length:10000},(_,i)=>({...link,detail:`evidence-${i}`}));
 value.evidence=Array.from({length:10000},(_,i)=>({index:i,kind:"journal",status:"verified"}));
 mount(value);await screen.findByText(/10000 recorded/);
 expect(screen.getAllByRole("button",{name:"Open exact event 9",hidden:true})).toHaveLength(100);
 expect(screen.queryByText("evidence-9999")).not.toBeInTheDocument();
});
