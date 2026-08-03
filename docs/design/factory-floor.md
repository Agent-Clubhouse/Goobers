# Factory Floor read model

## Status

Implemented in the portal from existing daemon reads. The optimized endpoint in
this document is a proposal only.

## Purpose

Factory Floor is the main operational view of a Goobers instance. It presents
configured workflows as production lines, declared stages as machines, active
runs as carriers, and goobers at the stages they own while work is present.

The view is factual. Geometry is deterministic. A confirmed stage change
produces a one-shot transfer, while confirmed active work powers a restrained
operating cycle on that workflow's machines, belts, work orders, and posted
goobers. Idle workflows stay still. A red alarm means every run at that stage is confirmed held and
at least one is hard blocked. An amber alarm means every run is confirmed
paused at a human gate. The page does not add simulated work, decorative
traffic, or inferred owners.

## Two layouts, one model

Factory Floor draws the same floor model in two layouts, chosen by a top-level
toggle:

| Layout | Route value | What it answers |
|---|---|---|
| Lines | `lines` (default) | Exactly how a workflow is wired: declared stages in graph order, each edge, each outcome, each terminal |
| Plant | `plant` | What the whole instance looks like from the boss's window: production zones, real stage machines, alarms, crates, and staff |

Layout is presentation only. Both layouts:

* consume the same `FactoryFloorModel` from the same `useFactoryFloor` hook
* render the same entities with the same IDs (`lane`, `station`, `run`,
  `worker`, `gaggle`)
* share one `FactorySelection` state and one `FactoryInspector`
* share accessible names, built once in `factoryLabels.ts`
* honour the same truncation, overflow, alarm, and unknown-topology rules

Factory is a dedicated viewport workspace inside the portal shell. It consumes
the full area below the global top bar rather than inheriting the padded,
maximum-width document layout used by report pages. `FactoryViewport.tsx`
measures that area, fits the complete world on first render and resize, clips
the scene without scrollbars, and provides explicit zoom, drag-to-pan, and Fit
All controls. Fit and resize change only the camera and never replay work
motion.

The shared inspector is an overlay drawer on desktop and a bottom sheet on
narrow screens. It does not reserve a permanent column or reduce the floor's
fit calculation. Inspector content is the only Factory region that may scroll.

Switching layout must not trigger a daemon read, rebuild the model, change the
scope, change the lens, or drop a selection. There is no layout-specific data
contract and no layout-specific daemon field.

The lens (`world`, `flow`, `risk`) is a separate control. Layout chooses how the
floor is drawn; the lens chooses what the floor emphasises. Both layouts
implement all three lenses.

### Route contract

```
#/factory?gaggle=<name>&workflow=<name>&lens=<world|flow|risk>&layout=<lines|plant>
```

All four values are optional and independent. `layout=lines` and `lens=world`
are the defaults and are omitted from the hash. An unrecognised `layout` falls
back to `lines`, exactly as an unrecognised `lens` falls back to `world`.

### Layout responsibilities

`FactoryFloor.tsx` (lines) reads the model's own floor-space coordinates:
lane bands, station rectangles, conveyor paths, docks, the inbound yard, and the
ready commons.

`FactoryPlant.tsx` (plant) places the same model onto a fixed 1450 by 950
boss's-window scene. `factoryClassicPlant.ts` deterministically maps declared
stage order onto the factory's intake, planning, build, quality, and shipping
areas. There is no random placement or simulated production. A crate changes
position only after its run reports a real stage transition.

`FactoryWebGLScene.tsx` renders that projection as a procedural Three.js hall
with an orthographic isometric camera, physical materials, lights, shadows,
district pads, stage machines, local conveyor modules, work-order crates, and
posted goobers. It does not load remote models or textures. Colors are read
from the active portal theme, and changing theme rebuilds the scene with the
new palette.

The approved factory image remains mounted underneath the canvas. The canvas
becomes visible only after a renderer has initialized and completed its first
render. A browser without WebGL, or a renderer that throws during
initialization, therefore continues to show the complete image-based Plant
instead of a blank floor. A lost WebGL context immediately reveals the image
and a restored context redraws the scene. Renderer resources, geometries,
materials, resize observers, and animation frames are released when the scene
is replaced or unmounted. The WebGL module and Three.js are loaded only when
Plant is selected, so Lines and the rest of the portal do not pay the renderer
download cost.

Workflow buttons, stage pins, production-zone callouts, crates, workers,
capacity, and alarms remain live React elements over the visual renderer. When
WebGL is ready, duplicate HTML artwork becomes transparent while the semantic
buttons, focus rings, tooltips, and accessible names remain available. The
complete scene scales as one unit to the available viewport and never requires
internal scrolling. This keeps the factory readable as a whole even when the
instance has many workflows. Machine, crate, and goober hit targets use their
original image coordinates in fallback mode and the matching orthographic
camera projection when WebGL is ready.

Plant does not redraw topology edges over the illustration. Arbitrary
point-to-point connectors cross the scene and duplicate the conveyors already
painted into the factory, so active work is shown locally at machines, work
orders, and posted goobers. Lines remains the exact always-visible topology
view, with orthogonal circuit-style traces on an isolating floor-colour bed.

Every real stage remains a keyboard-accessible machine pin. Its placard opens
on hover, focus, selection, or alarm. World view keeps the scene quiet, Flow
reveals graph belts and outcome labels, and Risk suppresses healthy activity.
The five production-zone cards are honest aggregates of configured stages;
Lines remains the exact topology when an operator needs every edge and branch.

The Plant legend explains beacon alarms, placard status, outcome docks, ready
commons, and the observed-order cue. The Lines legend remains limited to
concepts used by the line topology layout.

WebGL animation follows the same truth rules as the HTML presentation.
Rotors, local rollers, crates, and posted goobers move only for confirmed
`running` or `starting` work. Risk suppresses healthy activity, and reduced
motion renders a static first frame without scheduling an animation loop.

Transition metadata belongs to a newly observed model generation. FactoryPage
consumes it for the layout that was mounted when the update arrived. Mounting
another layout, including an immediate toggle after invalidation, renders the
new confirmed position without replaying crate or belt motion.

### Testing expectations

Any change to either layout must keep these true:

* route parse and hash round-trip for every layout, lens and scope combination,
  with an unknown layout falling back to the default
* a layout change performs no new daemon read and preserves scope, lens and
  selection
* both layouts expose the same workflow, stage, run and goober entities under
  the same accessible names
* a click or keyboard activation in either layout updates the same inspector
* a real stage change moves that one run in either layout and leaves siblings
  still
* a layout toggle never replays a transition from the current model
* both layouts start in Fit All with no nested canvas scrollbars
* zoom and pan change only the camera; Fit All restores the complete scene
* opening the inspector overlays the floor instead of resizing its camera
* blocked and human-hold alarms carry text as well as colour in both layouts
* overflow, 50-run truncation, unknown topology and idle states stay honest in
  both layouts
* the plant projection stays deterministic, with no random or time-based
  geometry
* every outcome label is target-side, visible above machinery, and separated
  from sibling branch labels
* multiple repass edges use distinct return lanes
* adding a district or later topology never changes the tile or existing
  machine coordinates
* neither layout renders a field outside the privacy whitelist below

## Current read composition

The current implementation composes the view in the browser from existing
daemon calls.

### Shared inventory snapshot

The shared operational snapshot reads:

1. health and instance summary
2. paginated gaggle inventory
3. paginated goober inventory for each gaggle
4. paginated workflow summaries for each gaggle
5. the latest terminal outcome per workflow for recent attention

The shared inventory loader also reads repository connection summaries for
other portal pages. Factory Floor does not copy repository identities or
connection data into its view model.

### Factory detail

The floor adds these scoped reads:

1. one active run list with `phase=running`, optional gaggle and workflow
   filters, and a limit of 50
2. up to 12 workflow detail reads for declared stages and graph edges
3. one signal read for every visible active run, with concurrency limited to 6

Workflow detail selection is stable. Explicit selected scope comes first.
Workflows with active runs come next in stable workflow identity order, followed
by the remaining configured workflows in configured order. The selection never
ranks by volatile active-run count. A previously confirmed workflow detail is
retained if a later batch does not include it or its refresh fails.

For a known gate stage, the signal read uses run events because `gate.paused` is
recorded in the journal. For a known non-gate stage, it uses the current stage
attempt list. If topology is unread and the stage kind is unknown, it uses run
events because those events can represent both gate pauses and stage completion
status. All reads receive the page cancellation signal.

### Live invalidation and fallback

Factory Floor subscribes to scoped `run` and `workflow` invalidations from the
portal SSE controller. An invalidation marks the last confirmed floor stale,
cancels an older detail request, and starts a replacement read. The stale plant
remains visible until the replacement completes.

The SSE controller resumes from its stored cursor, coalesces invalidations, and
uses the daemon heartbeat to detect a silent stream. After repeated stream
failures it enters the existing polling fallback. The default fallback interval
is 5 seconds, with backoff after failed refreshes. Health continues to refresh
on the shared operational cadence.

## Safe frontend view model

Only the fields below belong in the Factory Floor model. Optional fields are
marked with `?`.

### Floor

| Field | Meaning |
|---|---|
| `scope.gaggle?`, `scope.workflow?` | Validated configured identifiers |
| `gaggles` | Safe gaggle entities |
| `workflows` | Safe workflow identities and lane IDs |
| `lanes`, `stations`, `carriers`, `workers` | Render-ready plant entities |
| `commons` | Deterministic idle-goober placement |
| `attention` | Confirmed held runs and recent terminal failures |
| `counts` | Gaggle, workflow, goober, WIP, held, unread, blocked, and queued counts |
| `capacity` | WIP, known limit sum, unknown limit count, and saturation |
| `emptyReason?` | `no-gaggles`, `no-workflows`, or `no-active-runs` |
| `runsTruncated` | True when more active runs exist beyond the 50-run bound |
| `width`, `height` | Deterministic canvas size |

### Gaggle

`name`, `displayName`, `status`, `workflowCount`, `gooberCount`, `activeRuns`,
`unreadRuns`, and `blockedStages`.

### Workflow and lane

Workflow identity contains `gaggle`, `name`, `displayName`, and `laneId`.

A lane contains those identifiers plus `source`, `stageCount`, station, dock,
conveyor and inbound-yard geometry, `activeRuns`, `blockedRuns`, `unreadRuns`,
`limit?`, and `saturation?`.

`stageCount` is always the configured count from workflow inventory. The UI
states `N configured, M drawn` when the detail batch did not supply every
stage. A lane with no readable topology says that topology was not read in this
batch. Observed fallback stages are grouped without arrows and explicitly say
that order is unknown.

`source` is:

* `declared` when a confirmed workflow graph is available
* `observed` when only stages reported by active runs can be shown

### Stage

A stage contains:

* safe identity: `id`, `laneId`, `stageId`, `gaggle`, `workflow`,
  `workflowDisplayName`
* declared classification: `kind`, `evaluator?`, `owner?`, `source`, `isStart`
* deterministic geometry: `column`, `row`, `x`, `y`, `width`, `height`
* live state: `wip`, `limit?`, `saturation?`, `blockedCount`,
  `hardBlockedCount`, `pausedCount`, `unknownCount`, `status`, `alarm`,
  complete `runIds` and `workerIds`, rendered ID subsets, and overflow counts

Stage status is `idle`, `running`, `impeded`, `held`, `blocked`, or `unknown`.

The blocked alarm is on only when:

1. WIP is greater than zero
2. `unknownCount` is zero
3. every run at the stage is confirmed `blocked` or `paused`
4. at least one run is hard blocked

The hold alarm uses the same first three conditions and additionally requires
every run to be paused at a human gate. An unread signal prevents either
definitive alarm.

The machine face shows stage WIP only. The workflow-wide active count and
`maxConcurrentRuns` gauge belongs to the lane plaque and is labeled `workflow
limit`.

### Goober

A goober contains `id`, `gaggle`, `gaggleDisplayName`, `name`, `displayName`,
`harness`, `status`, owned stage identities, `activeRunCount`,
`activeStationIds`, deterministic placements, and `idle`.

A goober is placed at an owned stage only while that stage has WIP. A configured
goober with no active owned stage is placed in the ready commons. Role text,
skills, capabilities, prompts, and warnings are not part of this model.

Only one worker glyph is rendered per stage. Additional assigned workers remain
in the model and appear through a keyboard-accessible staffing aggregate tied
to the stage inspector. The ready commons renders at most 12 worker glyphs and
uses a `+N ready` control for the complete remainder.

### Carrier

A carrier contains:

* safe identity: `runId`, `gaggle`, `workflow`, `workflowDisplayName`,
  `laneId`, `stageId?`, `stationId`
* closed state: `phase`, `state`, `reason?`, `confirmed`, `triggerKind`
* bounded metrics: start and last-activity timestamps, duration, retry counts,
  repass count
* placement: `ownerWorkerId?`, stable `queueIndex`, `rendered`, `renderSlot?`,
  `x`, `y`
* transition metadata only for an arrival or a confirmed stage change

Carrier state is `running`, `paused`, `blocked`, `starting`, or `unknown`.
`unknown` always has `confirmed=false`. A failed signal read is never converted
to `running`.

Stable run-ID keyed slots keep a surviving carrier in place when a sibling
leaves. CSS transition treatment is added only when that same run changes its
reported stage.

The model keeps every bounded carrier and every run ID. Rendering is capped at
6 carriers per stage and 4 carriers in each inbound yard. A truthful `+N more`
or `+N queued` control represents the remainder. Stage overflow controls are
keyboard accessible and select the stage so the inspector can list every run.
Rendered carriers and workers stay inside their reserved lane regions.

### Attention, capacity, freshness, and truncation

Attention contains run ID, gaggle and workflow identifiers, optional stage ID,
closed phase or hold reason, and timestamp. It never contains raw event data or
error text.

Capacity sums only positive configured workflow limits. If any displayed
workflow has no usable limit, the unknown-limit count is shown and aggregate
saturation is not claimed.

Freshness is query state, not an inferred model field:

* `ready`: inventory and active detail completed
* `stale`: a last confirmed floor is visible while refresh is pending or failed
* `loading`: no active detail has completed for the current scope
* `error`: no confirmed floor is available

The `no-active-runs` state is shown only after a completed detail read. It keeps
all configured lanes, machines, topology, and ready commons visible with an
inline idle note. Only `no-gaggles` and `no-workflows` replace the floor. A cold
scope never renders zero WIP while its active-run list is still pending.

`runsTruncated=true` means the active-run response supplied a continuation
cursor. Every headline, capacity readout, legend, and plant verdict states that
the view is partial. Counts use a lower-bound form such as `50+`; the page never
claims a definitive healthy or blocked plant state from an incomplete set.

## Failure and degradation behavior

* A failed per-run signal read creates an explicit unknown carrier and increments
  unread counts.
* A failed workflow detail read keeps the last confirmed topology when one
  exists. Without prior detail, the lane shows only stages observed from active
  runs and labels the topology unread.
* A failed refresh keeps the last confirmed floor, marks freshness degraded, and
  uses fixed error copy.
* A cold detail failure shows a fixed unavailable state. Raw daemon messages are
  not rendered.
* Cancellation stops queued signal reads and prevents an obsolete response from
  replacing current scope data.
* Offline, hidden-tab, reconnecting, and polling states use the shared live-data
  behavior.

## Privacy rules

Factory Floor may show configured gaggle, workflow, stage, goober, and run
identifiers plus closed operational categories and counts.

It must not include or render:

* `error.message` or any other free-form daemon error
* raw event payloads or gate reasons
* trigger references
* repository owners, names, branches, refs, or connection details
* URLs or local paths
* artifact or transcript names, metadata, or contents
* prompts, goals, role text, skills, capabilities, or secrets
* unsafe HTML

Fixture and screenshot data must be synthetic.

## Proposed optimized read contract

This section is a design proposal. It is not implemented.

### Endpoint

`GET /v1/factory-floor`

Query parameters:

| Parameter | Meaning |
|---|---|
| `gaggle?` | Restrict to one configured gaggle |
| `workflow?` | Restrict to one configured workflow |
| `activeLimit?` | Maximum active carriers, capped by the server at 50 |

The endpoint would compose inventory, graph topology, active placement, run
signals, capacity, and recent attention in one bounded read. A local
implementation can query the local read model directly. Future hosted tiers can
serve the same response from their projection store.

When the active carrier list is truncated, the server selects confirmed held or
blocked runs first, oldest first within that group, followed by the oldest
remaining active runs. Stable run ID is the final tie-breaker.

The response must include aggregate counts computed independently of the
truncated carrier list: total running runs, total held runs, total blocked or
paused stages, and per-lane and per-stage WIP. These aggregates keep headlines,
capacity, and overflow controls truthful even when individual carriers are
omitted.

The response must be invalidatable through the existing SSE stream using
scoped instance, workflow, and run invalidations. It must carry the standard
read-state envelope and a projection cursor so the client can compare the read
with later invalidations.

### Synthetic example

```json
{
  "apiVersion": "v1",
  "scope": {
    "gaggle": "assembly",
    "workflow": "delivery"
  },
  "observedAt": "2026-08-01T20:00:00Z",
  "readState": {
    "kind": "current",
    "lagSeconds": 1
  },
  "limits": {
    "active": 50,
    "runsTruncated": false
  },
  "aggregates": {
    "running": 1,
    "held": 0,
    "blockedOrPausedStages": 0,
    "laneWip": {
      "assembly/delivery": 1
    },
    "stageWip": {
      "assembly/delivery/build": 1
    }
  },
  "gaggles": [
    {
      "name": "assembly",
      "displayName": "Assembly",
      "status": "configured"
    }
  ],
  "workflows": [
    {
      "gaggle": "assembly",
      "name": "delivery",
      "displayName": "Delivery",
      "maxConcurrentRuns": 4,
      "stages": [
        {
          "id": "prepare",
          "kind": "deterministic"
        },
        {
          "id": "build",
          "kind": "agentic",
          "owner": {
            "gaggle": "assembly",
            "name": "builder"
          }
        },
        {
          "id": "approve",
          "kind": "gate",
          "evaluator": "human"
        }
      ],
      "edges": [
        {
          "source": "prepare",
          "target": "build"
        },
        {
          "source": "build",
          "target": "approve"
        },
        {
          "source": "approve",
          "terminal": "complete",
          "outcome": "approved"
        }
      ]
    }
  ],
  "goobers": [
    {
      "gaggle": "assembly",
      "name": "builder",
      "displayName": "Builder",
      "harness": "copilot",
      "status": "configured"
    }
  ],
  "activeRuns": [
    {
      "runId": "01SYNTHETICRUN001",
      "gaggle": "assembly",
      "workflow": "delivery",
      "stage": "build",
      "phase": "running",
      "signal": {
        "state": "running",
        "confirmed": true
      },
      "startedAt": "2026-08-01T19:58:00Z",
      "lastActivityAt": "2026-08-01T19:59:30Z",
      "retryCount": 0,
      "repassCount": 0,
      "triggerKind": "item"
    }
  ],
  "recentAttention": []
}
```

### Contract constraints

* One request must replace the current workflow-detail and per-run N+1 reads.
* The server must enforce the active limit and report truncation.
* Truncated selection must prioritize held or blocked runs, then oldest active
  runs, with stable run ID as the final tie-breaker.
* Aggregate running, held, blocked or paused stage, lane WIP, and stage WIP
  counts must be independent of the truncated carrier list.
* Unknown signal and unknown limit states must remain explicit.
* The response must contain only the safe fields defined above.
* Local and hosted implementations must preserve the same ordering, derivation,
  scope, privacy, and invalidation semantics.
