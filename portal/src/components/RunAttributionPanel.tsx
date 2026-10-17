import { useState } from "react";
import type { DaemonClient, RunAttributionResult, AttributionContribution, AttributionEvidence } from "../api/types";
import { MalformedResponseError } from "../api/errors";
import { dataCacheKey } from "../dataCache";
import { useLiveQuery } from "../liveQuery";
import type { Navigate } from "../routing";
import { Action } from "../ui/Action";
import { Timestamp } from "../ui/Timestamp";

const availabilityText = {
 absent: "No active attribution record is available for this run.",
 pending: "Terminal attribution is not yet available.",
 unsupported: "This journal needs migration before attribution can be read. Viewing it does not migrate it.",
 "limit-exceeded": "This run exceeds the attribution read budget. No partial analysis is shown.",
 recorded: "",
};

export function RunAttributionPanel({ client, runId, gaggle, navigate }: {
 client: DaemonClient; runId: string; gaggle: string; navigate: Navigate;
}) {
 const [page, setPage] = useState(0);
 const query = useLiveQuery<RunAttributionResult>({
  cacheKey: dataCacheKey("run-attribution", runId, gaggle),
  dependencies: [{ model: "run", gaggle }], models: ["run"], scope: { gaggle },
  isCurrent: (value) => value.runId === runId && value.gaggle === gaggle,
  load: async (signal) => {
   const value = await client.getRunAttribution(runId, gaggle, { signal });
   if (value.schema !== "goobers.dev/backprop/read/v1" || value.runId !== runId || value.gaggle !== gaggle ||
       !(value.availability in availabilityText) || !Array.isArray(value.evidence) ||
       (value.availability !== "recorded" && value.record !== undefined) ||
       (value.availability === "recorded" && (!value.record || value.record.runId !== runId || value.record.gaggle !== gaggle || !(Array.isArray(value.record.attribution.contributions) || value.record.attribution.contributions === null)))) {
    throw new MalformedResponseError("The daemon returned incompatible or mismatched run attribution.");
   }
   return value;
  },
  errorMessage: "Unable to read attribution. This daemon may not support run attribution.",
 });
 const value = query.state.status === "ready" || query.state.status === "stale" ? query.state.data : undefined;
 const record = value?.record;
 const contributions = [...(record?.attribution.contributions ?? [])].sort((a,b) => b.score-a.score || a.nodeId.localeCompare(b.nodeId));
 const evidenceByNode = indexAttributionEvidence(value);
 const currentPage = Math.min(page, Math.max(0, Math.ceil(contributions.length/50)-1));
 return <section aria-labelledby="run-tab-backprop" id="run-panel-backprop" role="tabpanel" className="run-attribution-panel">
  <h2>Backprop attribution</h2>
  <p>Recorded estimates for this run. Signed score estimates benefit or harm; responsibility share is unsigned.</p>
  <Action onClick={query.retry}>Refresh attribution</Action>
  {query.state.status === "loading" && <p role="status">Loading run attribution…</p>}
  {query.state.status === "error" && <p role="alert">Attribution unavailable. {query.state.error.message}</p>}
  {query.state.status === "stale" && <p role="status">This attribution observation may be stale. Refresh to check the record and evidence.</p>}
  {value && <>
   <p>Read <Timestamp value={value.observedAt} />. Run phase: {value.runPhase ?? "unknown"}.</p>
   {value.availability !== "recorded" && <p role="status">{availabilityText[value.availability]}</p>}
   {record && <>
    <dl>
     <dt>Analysis status</dt><dd>{record.status}</dd>
     <dt>Workflow</dt><dd>{record.workflow} · version {record.workflowVersion}</dd>
     <dt>Effective version</dt><dd className="mono">{record.effectiveVersion}</dd>
     <dt>Human correctness label</dt><dd>{value.groundTruth ? <>{value.groundTruth.outcome} · {value.groundTruth.source} · {value.groundTruth.labeledBy} · <Timestamp value={value.groundTruth.labeledAt} />{value.groundTruth.reason && <p>{value.groundTruth.reason}</p>}</> : "Not labeled"}</dd>
    </dl>
    {record.failure && <p role="alert">Recorded analysis failure: {record.failure}</p>}
    {record.stageComposition && <p>Stage checkpoints: {record.stageComposition.checkpoints}; through sequence {record.stageComposition.throughSequence ?? "unknown"}. {record.stageComposition.error}</p>}
    <p>{record.attribution.assumptions?.join(" · ")}</p>
    <div className="run-attribution-table"><table>
     <caption>Contributions ordered by signed score, highest first. {contributions.length} recorded contributions.</caption>
     <thead><tr><th scope="col">Contribution and evidence</th><th scope="col">Signed score</th><th scope="col">Responsibility share</th><th scope="col">Confidence</th><th scope="col">Uncertainty</th><th scope="col">Provenance</th></tr></thead>
     <tbody>{contributions.slice(currentPage*50,(currentPage+1)*50).map((item,index) => <ContributionRow key={`${item.nodeId}-${index}`} item={item} value={value} evidence={evidenceByNode.get(item.nodeId) ?? []} navigate={navigate} />)}</tbody>
    </table></div>
    <p>Page {currentPage+1} of {Math.max(1,Math.ceil(contributions.length/50))}</p>
    <Action disabled={currentPage===0} onClick={() => setPage(currentPage-1)}>Previous contributions</Action>{" "}
    <Action disabled={(currentPage+1)*50>=contributions.length} onClick={() => setPage(currentPage+1)}>Next contributions</Action>
    <details><summary>Claims and source information</summary>
     <p>Showing up to 100 claims and 100 unverified claims. Claims are evidence, not authority. Dropped claims: {record.attribution.droppedClaims ?? 0}.</p>
     {record.attribution.claims?.slice(0,100).map(claim => <p key={claim.id}>{claim.status}: {claim.statement}</p>)}
     {record.attribution.unverifiedClaims?.slice(0,100).map(claim => <p key={claim.id}>Unverified: {claim.statement} {claim.reasons?.join(" · ")}</p>)}
     <p>Record schema: {record.schema}; attribution contract: {record.contractVersion}.</p>
     <p className="mono">Record digest: {value.sourceDigest}</p>
     <p className="mono">Label revision: {value.labelRevision ?? "none"}; effective label: {value.groundTruth?.id ?? "none"}</p>
     <p>Input read: {value.inputBytes} bytes. Evidence reflects the files available at read time.</p>
    </details>
   </>}
  </>}
 </section>;
}

type CheckedEvidence = {link: AttributionEvidence; check?: RunAttributionResult["evidence"][number]};
function indexAttributionEvidence(value?: RunAttributionResult): Map<string, CheckedEvidence[]> {
 const result = new Map<string,CheckedEvidence[]>();
 const checks = new Map(value?.evidence.map(check=>[check.index,check]));
 value?.record?.evidence?.forEach((link,index)=>{
  const key=link.nodeId ?? ""; const links=result.get(key) ?? [];
  links.push({link,check:checks.get(index)}); result.set(key,links);
 });
 return result;
}
function ContributionRow({item,value,evidence,navigate}: {item: AttributionContribution; value: RunAttributionResult; evidence: CheckedEvidence[]; navigate: Navigate}) {
 const record = value.record!;

 return <tr>
  <th scope="row"><details><summary>{item.label || item.nodeId} · {item.kind}</summary>
   <p className="mono">{item.nodeId}</p>
   <p>Stage: {item.stage || "unknown"}; attempt: {item.attempt ?? "unknown"}</p>
   <p>Showing up to 100 path nodes and 100 evidence references ({evidence.length} recorded).</p>
   <ol>{(item.path ?? [item.nodeId]).slice(0,100).map((node,index)=><li key={`${node}-${index}`} className="mono">{node}</li>)}</ol>
   {record.attribution.causes?.filter(cause=>cause.nodeId===item.nodeId).slice(0,100).map((cause,index)=><div key={index}>
    <p>{cause.class}: {cause.summary} {cause.errorCode}</p>
    <p>Cause confidence: {cause.confidence}. {cause.assumptions?.join(" · ")}</p>
    <ul>{cause.evidence?.slice(0,100).map((text,i)=><li key={i}>{text}</li>)}</ul>
   </div>)}
   {evidence.length===0 && <p>No exact journal evidence was recorded for this contribution.</p>}
   <ul>{evidence.slice(0,100).map(({link,check},index)=><li key={index}>
    {link.detail} · {check?.kind ?? "unknown"} · {check?.status ?? "unavailable"}
    {check && link.journalSequence && <Action onClick={()=>navigate({page:"run",id:value.runId,tab:"journal",seq:link.journalSequence,event:true})}>Open exact event {link.journalSequence}</Action>}
    {check?.kind==="artifact" && check.status==="verified" && <Action onClick={()=>navigate({page:"run",id:value.runId,tab:"artifacts",seq:link.journalSequence,event:true})}>Inspect artifact</Action>}
    {link.artifactDigest && <span className="mono">{link.artifactDigest}</span>}
   </li>)}</ul>
  </details></th>
  <td>{item.score>0?"+":""}{item.score.toFixed(3)}</td><td>{item.share.toFixed(3)}</td><td>{item.confidence.toFixed(3)}</td><td>{item.uncertainty.toFixed(3)}</td><td>{item.provenance}</td>
 </tr>;
}
