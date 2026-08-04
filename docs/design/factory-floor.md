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
| Plant | `plant` | What the whole instance looks like from the boss's window: workflow bays, real stage machines, alarms, crates, and staff |

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

`FactoryPlant.tsx` (plant) selects exactly one coordinate system at a time.
`factoryPlantLayout.ts` is the canonical WebGL layout: it allocates one or more
integer-grid bay cells per workflow, places every station from its model
column/row and stable ID, carries exact model conveyor inputs, and derives
dynamic world bounds. `factoryClassicPlant.ts` remains the fixed 1450 by 950
projection, now used only by the approved bitmap fallback.

`FactoryWebGLScene.tsx` renders the canonical layout as a procedural Three.js
hall with an orthographic isometric camera, physical materials, lights,
shadows, workflow bay pads, stage machines, declared track segments,
work-order crates, and posted goobers. It does not load remote models or
textures. Theme and lens changes recolour retained resources; they do not
recreate the renderer or scene.

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

Workflow buttons, stage pins, workflow-bay summaries, crates, workers,
capacity, and alarms remain live React elements over the visual renderer. When
WebGL is ready they are placed by the live camera through the screen-space
overlay; when it is pending, fallen back, or unavailable the classic bitmap and
its classic coordinates are used instead. The two are never mixed. The complete
scene scales as one unit to the available viewport and never requires internal
scrolling. This keeps the factory readable as a whole even when the instance has
many workflows.

Plant creates a track only for a `FactoryConveyor` in the model and preserves
its edge ID, kind, branch/outcome/terminal metadata, and active truth. It never
invents a local belt per machine or a cross-workflow connector. Lines remains
the exact topology view and the authoritative place for every branch and
outcome label.

Every real stage remains a keyboard-accessible machine pin. Its placard opens
on hover, focus, selection, or alarm. World view keeps the scene quiet, Flow
reveals graph belts and outcome labels, and Risk suppresses healthy activity.
Each workflow bay carries its own aggregate summary. Display precedence is
blocked, held, unknown, running, then idle; the underlying station statuses,
including impeded, are retained unchanged. Lines remains the exact topology
when an operator needs every edge and branch.

The Plant legend explains beacon alarms, placard status, outcome docks, ready
commons, and the observed-order cue. The Lines legend remains limited to
concepts used by the line topology layout.

WebGL animation follows the same truth rules as the HTML presentation.
Instanced machine/track activity, crates, and posted goobers move or pulse only
for confirmed `running` or `starting` work. Risk suppresses healthy activity,
and reduced motion renders a static first frame without scheduling an
animation loop.

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
* a model, lens, theme, or motion change reuses the mounted WebGL runtime:
  no new context, no renderer disposal, and no scene rebuild
* an entity that survives a model update keeps its `Object3D` and its
  animation phase; a removed entity releases its resources exactly once
* the Plant renders with at most one outstanding animation frame, and stops it
  for idle work, Risk, reduced motion, a hidden document, and a lost context
* both layouts start in Fit All with no nested canvas scrollbars
* zoom and pan change only the camera; Fit All restores the complete scene
* the inspector reduces the camera's safe area instead of covering the scene:
  Fit All shows the whole world in the unobscured rectangle with the drawer
  open or closed, and selecting keeps the selected anchor visible
* in WebGL-ready mode every positioned semantic is placed by the live camera,
  and in fallback mode every one of them is placed by classic coordinates:
  the two coordinate systems are never mixed, and switching between them
  preserves selection and focus
* label packing is deterministic and priority-ordered, leaves no residual
  overlap or clipping, collapses only lower-priority labels, and reports the
  collapse as a truthful `+N` aggregate
* blocked and human-hold alarms carry text as well as colour in both layouts
* overflow, 50-run truncation, unknown topology and idle states stay honest in
  both layouts
* the plant projection stays deterministic, with no random or time-based
  geometry
* every outcome label is target-side, visible above machinery, and separated
  from sibling branch labels
* multiple repass edges use distinct return lanes
* inserting a workflow with prior allocation never moves an unchanged workflow
  or station; a bay that cannot grow in place relocates only that workflow
* neither layout renders a field outside the privacy whitelist below

### Plant measurement probe and harness (WP-0)

The WebGL Plant has an opt-in measurement surface for development and E2E use.
It is not installed during a normal portal load. Enable it with the page query
parameter before the hash:

```text
http://127.0.0.1:5173/?plant-probe=1#/factory?layout=plant
```

When enabled, `window.__plantProbe` exposes:

* `snapshot()` — returns the current renderer, scene, animation, canvas, model,
  viewport, and projection measurements
* `reset()` — zeros lifecycle and frame counters while retaining the currently
  observed renderer/model state, so the next theme or lens change is a bounded
  measurement interval
* `waitForFrames(count?, timeoutMs?)` — waits for additional rendered frames
* `remeasure()` — re-runs every registered measurement against the committed
  DOM and returns the resulting snapshot. Producers publish measurements when
  their inputs change, but the runtime publishes its camera before React
  commits the overlay, so a reading taken in the same tick as a camera change
  can carry one frame of React latency. Anything asserting on drift — the
  harness between viewport and inspector changes — settles the page and then
  pulls a fresh reading instead of trusting whatever the last change left
  behind.
* `loseContext()` and `restoreContext()` — call `WEBGL_lose_context` on the
  currently observed renderer and return `false` when the extension or an
  active context is unavailable. The extension handle is captured while the
  context is still alive, because `getExtension` returns `null` on a lost
  context and a probe that looks it up on demand can lose a context it can
  never restore.

Snapshots include renderer state; created and active context counts; context
losses/restores; renderer and scene disposals/builds; keyed entity
reconciliation totals (`entities`: reconcile passes, created, replaced,
updated, removed, and live objects) and the number of model generations
delivered (`modelUpdates`); frames, RAF callbacks and requests; motion state
and animated-object count; Three.js draw calls, triangles, programs,
geometries, and textures; canvas CSS/backing dimensions; model entity/count
bounds, lens, theme, and reduced-motion state; layout workflow/bay/station/
track/instance counts, dynamic world and projected bounds, collision totals,
batch and draw-call plans, and LOD DOM budgets; viewport and document overflow;
and per-entity semantic-to-WebGL projection drift.

Run the deterministic, dependency-free CDP harness from `portal`:

```powershell
npm run test:plant
```

The harness starts Vite on an available local port, discovers installed or
cached Chrome/Edge/Chromium, and intercepts `/api/v1/**` with a bounded
synthetic Factory fixture. It captures light World, dark World, light Risk,
dark Risk, context loss/restoration, a 640x480 viewport, and a separate
`--disable-webgl` fallback browser. Results and PNG captures are written under
`portal/.plant-harness/` (gitignored). TAP is written to stdout and the full
measurements are in `results.json`.

Useful options:

```powershell
node tools/plant-harness.mjs --base-url http://127.0.0.1:5173
node tools/plant-harness.mjs --browser "C:\path\to\chrome.exe"
node tools/plant-harness.mjs --output .plant-harness-custom
npm run test:plant:live -- --daemon-url http://127.0.0.1:8080
npm run test:plant:live -- --base-url http://127.0.0.1:5173
```

`--base-url` targets an already running portal. `--live-daemon` disables CDP
API fixtures; `--daemon-url` configures the Vite proxy only when the harness
starts Vite itself. The live mode intentionally has no fallback data: daemon
read failures remain failures.

Interpret the measurements as deltas, not budgets yet:

* a theme/lens interval with multiple context creations or scene builds exposes
  renderer rebuild churn; active contexts should return to one and disposals
  should account for replaced scenes
* increasing geometries/textures/programs after a completed replacement
  exposes retained GPU resources
* RAF requests/callbacks with `motion=false`, Risk, or reduced motion expose an
  unnecessary animation loop
* projection `maxDrift` and each entry's vector expose semantic hit-target
  alignment errors in pixels; since WP-4 the overlay measurement additionally
  reports collision, clipped-label, and inspector-occlusion counts, and the
  harness fails above 6 CSS pixels of drift or on any occluded selected or
  critical semantic
* the Plant-only screenshot luminance and dark-pixel ratio make dark/Risk
  combinations comparable without asserting a design threshold in WP-0
* document overflow must stay false and the Factory viewport must remain
  `overflow: hidden`

The harness exits non-zero for failed functional checks and records the
observed state instead of substituting defaults. WP-0 is measurement only: it
does not repair renderer lifecycle, projection, dark Risk, or layout behavior.

### Persistent Plant runtime (WP-1)

WP-0 measured what WP-1 repairs: a full renderer, scene, and animation rebuild
on every model, lens, theme, or motion change; animation phases restarting on
each rebuild; incomplete teardown; and a lost WebGL context that never came
back. The runtime now outlives every prop change on a mounted canvas.

The WebGL code lives entirely in the lazily imported chunk and splits by
responsibility:

* `components/plant/factoryPlantScheduler.ts` — one frame scheduler per canvas,
  with at most one outstanding `requestAnimationFrame`. Nothing here touches
  Three.js or the DOM; the clock and frame primitives are injected.
* `components/plant/factoryPlantEntities.ts` — pure derivation of a keyed
  entity specification list from the confirmed model, plus the reconciler.
* `components/plant/factoryPlantSceneGraph.ts` — the Three.js objects, the
  shared geometry cache, and the resource ledger that releases each geometry,
  material, texture, and shadow map exactly once.
* `components/plant/factoryWebGLRuntime.ts` — the runtime that owns exactly one
  renderer, scene, orthographic camera, resize/visibility/intersection
  observer, context listener pair, and scheduler for the life of the canvas.
* `components/FactoryWebGLScene.tsx` — a React adapter with no Three.js state:
  a mount-only effect creates and disposes the runtime, and a separate update
  effect forwards model, layout, lens, reduced motion, theme, and
  `animateTransitions`.

Entities reconcile by the identity the semantic HTML layer already uses:
`station.id` for a machine, the model conveyor ID for a declared track,
`carrier.runId` for a work order crate, and `placement.id` for a posted goober.
An entity that is still
present keeps its `Object3D`, its materials, and its animation phase; only a
structural shape change (a stage becoming a gate, for example) replaces the
object, and only a disappearance disposes it. Animation phase and conveyor
activity are keyed by identity, never taken from array position, so one new run
cannot re-phase every crate behind it.

One scheduler serves every reason to draw. Updates, resizes, theme changes, and
lens changes request a single coalesced frame. A continuous loop runs only for
confirmed operating motion or an active confirmed transfer, and stops for idle
work, the Risk lens, reduced motion, a hidden document, an offscreen canvas, a
lost context, and disposal. Frame deltas are capped so a backgrounded tab
cannot teleport the animation on return.

A confirmed stage change plays once. Each transfer carries a signature derived
from the run, the station it left, the station it reached, and its stage, so a
remount, a layout toggle, a theme change, or a lens change re-delivers the same
signature and is suppressed. Transfer offsets compose separately from the bob
so neither cancels the other.

Context loss cancels the frame loop immediately, shows the approved
illustration, and keeps the CPU scene and the latest snapshot. Restoration
re-applies size and requests a frame; readiness is reported by that frame, not
by the event, so the fallback stays up until pixels exist again. Repeated
loss and restoration cycles are supported.

The harness now proves the runtime claims directly:

* `theme change reuses the runtime and scene` and `lens change reuses the
  runtime and scene` — zero context creations, renderer disposals, scene
  builds, and scene disposals in the measured interval, with keyed
  reconciliation updating retained entities instead of creating them
* `Risk lens stops the frame loop` — motion off and the frame counter static
* `live model refresh reuses the runtime and scene` — a new model generation
  arriving from the live stream reconciles without a rebuild
* `restoration keeps the retained scene` and `repeated context loss and
  restoration keeps working`

WP-1 is bounded to lifecycle, reconciliation, and scheduling. Projection drift,
dark and Risk palettes, scalable layout, and Risk visuals were left for later
work packages.

### Deterministic workflow-bay layout (WP-2/WP-3)

`factoryPlantLayout.ts` is a pure renderer-neutral planner. Its output contains:

* a serializable `FactoryPlantAllocation`, keyed by workflow and station ID
* workflow bay cells on an integer grid, with a fixed gutter between cells
* station/machine transforms, carrier and worker anchors, docks, yards, and
  exact model track inputs and segments
* world-space overlay anchors for the later DOM projection wave
* a dynamic world AABB plus its canonical isometric projected bounds
* workflow and whole-plant aggregate summaries
* deterministic detail/bay/overview LOD thresholds and bounded DOM plans
* renderer-neutral instance batches keyed by mesh/material archetype
* collision, unresolved-track, batch, instance, and draw-call-plan metrics

The standard cell is 32 by 32 world units and exposes a 20 by 20 station slot
lattice. A normal linear workflow with 20 stages therefore occupies one cell
without machine overlap. Workflows whose topology needs more columns or rows
receive a rectangular set of additional cells. Station coordinates start from
the model's `column` and `row`; stable station IDs retain prior slots when a
later topology read inserts or reorders stages.

Initial allocation sorts stable workflow IDs before first-fit packing, so input
array order cannot affect geometry. A later build receives the prior
allocation. Unchanged bays keep their origin and span. A growing bay first
attempts positive-X/positive-Z expansion while every other prior bay remains
reserved. If that footprint is occupied, only the growing workflow is removed
and first-fit relocated; other workflows and stations do not move. Deleting a
workflow frees its cells without compacting survivors.

WebGL no longer maps stages into six bitmap anchors, creates five decorative
districts, or chooses machine/conveyor positions by array index. Every machine
uses its station ID and allocated slot. Every rendered track corresponds to one
model conveyor; unresolved endpoints are measured rather than fabricated.
Workflow bay summaries replace the former five-zone aggregate mapping.

The retained runtime applies each new layout in place:

* floor, grid, walls, lights, beams, and gantry scale from `hall.floor`
* bay pads, machine bodies, and declared track segments use `InstancedMesh`
  batches, while keyed anchor objects preserve reconciliation identity
* crates and workers retain their keyed one-shot/bob objects
* the same orthographic camera is refit from projected dynamic bounds for the
  current canvas aspect ratio with explicit safe padding
* a layout update can replace instance counts or camera extents, but never the
  renderer, scene, listeners, scheduler, or WebGL context

Aggregate display precedence is:

```text
blocked > held > unknown > running > idle
```

`impeded` remains a real station status and contributes to the aggregate
blocked-attention tier; the planner does not rewrite the station. LOD switching
is not enabled yet. The metadata currently fixes 180 projected pixels as the
detail threshold, 64 pixels as the bay-summary threshold, and 240 as the
maximum detail DOM candidate count.

#### Fallback and projection boundary

The canonical geometry boundary is deliberate:

* WebGL consumes only `FactoryPlantLayout`.
* `factoryClassicPlant.ts` remains for the approved bitmap fallback and its
  classic DOM placement, and for nothing else.
* The layout's `overlayAnchors` are the sole input to the screen-space overlay
  (WP-4, below).

Pure tests cover 1x1, 6x6, 12x12, 24-workflow insertion, a blocked in-place
growth relocation, and 50x20 stress. They assert unique station coordinates,
non-overlapping bay cells and machine footprints, exact station coverage,
shuffled-input determinism, prior-position stability, finite bounds, aggregate
totals, and bounded instance/DOM plans. The browser harness keeps its 17
runtime checks and additionally records layout counts, bounds, collisions,
batch/instance plans, and actual versus planned draw calls.

### Live-camera projection, overlay, and viewport (WP-4)

WP-2/WP-3 left WebGL drawing dynamic workflow bays while the DOM hit targets and
cards were still placed by fixed classic bitmap coordinates. The two disagreed
by up to about 734 CSS pixels, and the inspector drawer covered the right of the
stage that Fit All had just filled. WP-4 replaces both with one contract.

#### One projection source

`plantProjection.ts` is the contract every consumer shares. It is pure: no
Three.js, no DOM, so it stays out of the lazy renderer chunk and can be tested
directly. It defines the published state — projection source, revision, canvas
rectangle, safe rectangle, and the view-projection matrix — plus the projector
that turns a world point into a screen point, the rectangle algebra the overlay
and probe both use, and `PLANT_PROJECTION_TOLERANCE_PX`.

The runtime is the only producer. `FactoryWebGLRuntime` implements
`PlantProjectionController`: `projection()` publishes the state,
`project(world)` maps a world point through the camera that actually drew the
last frame, `projectEntity(id, world)` applies a live confirmed carrier transfer,
`subscribe()` reports camera changes, `subscribeAnimation()` reports only
entities that moved that frame, and `pick()` resolves a ray hit back to a
semantic. The matrix is read from the live orthographic camera
(`projectionMatrix x matrixWorldInverse`) after every layout, resize, safe-area,
or camera-fit change, and a new revision is published only when the signature
changes. The hand-rolled fixed-camera `projectedPointStyle` is deleted;
`factoryWebGL.ts` now exports only `webGLMotionEnabled`, and the classic
`pointStyle` survives for the bitmap fallback alone.

#### Screen-space semantic overlay

`factoryPlantOverlay.ts` derives, from the canonical overlay anchors, exactly
what exists on screen and what it means: bay signs and aggregate cards, station
buttons, carrier buttons, worker buttons, queued/run/staff overflow
affordances, and commons/yard affordances. Each item carries a stable semantic
id, its world anchor, its truth-derived tier, its selection, its accessible
name, and its hit size. It is renderer-neutral and pure.

`FactoryPlantOverlay.tsx` renders that list. The renderer first supplies a
canvas-local point, then the outer viewport camera converts it to final CSS
viewport coordinates; hit targets, labels, collision packing, clipping, and
probe occlusion are all resolved there. Each item root remains a zero-size
absolutely positioned button at the projected canvas point and is counter-scaled
by the viewport zoom so a hit target never shrinks below its minimum. A zero-size
`data-plant-anchor-origin` span marks the projected point itself, which is what
the probe measures, so label packing cannot move the thing being measured.
Anchors are keyed by semantic id, so they are stable across frames, models, and
renderer states.

Updates are batched. The runtime emits camera state only when the projection
signature changes, and `usePlantProjection` coalesces a burst into one animation
frame and one React update. During a confirmed stage transfer the runtime
publishes only the moving carrier; the overlay updates that button imperatively
from the same eased position as the WebGL crate, so the DOM and probe follow the
crate every frame without re-rendering the complete overlay.

#### Deterministic label packing

`plantLabelPacking.ts` is a pure, deterministic solver. Labels are ordered
selected > focused > alarm > blocked/held/unknown > active > workflow/bay sign >
idle, then placed against a bounded spatial hash of already-placed labels and of
the hit targets themselves, testing a fixed ladder of candidate offsets. Every
placement is clamped inside the visible safe rectangle. A label that cannot be
placed collapses into a truthful `+N` chip for its group — never a silent
overlap, and never a number that does not match the ids behind it. Each chip
derives its aggregate action from those hidden ids (workflow line, stage, or
overview), gives that action an exact accessible name, and is itself at least
32x32 on desktop or 44x44 on touch. A critical
semantic is never collapsed: if its anchor is off the safe rectangle it is
relocated to free space rather than hidden. Hit targets stay at least 32x32 on
desktop and 44x44 on touch.

#### Inspector safe area

`factoryViewportSafeArea.ts` is the single source of the inspector's geometry,
and `styles.css` declares the same numbers as custom properties; a test asserts
the two agree, because a drawer that is wider in CSS than in the camera's
arithmetic hides exactly the thing the operator selected. `FactoryViewport` is
the sole navigation camera and rigidly transforms the classic image, WebGL
canvas, and semantic overlay together. The retained internal WebGL camera fits
only the layout and canvas (or an explicit direct-embedding safe area), never an
outer pan or zoom pose. Fit All therefore shows 100% of the fixed Plant canvas
inside the unobscured rectangle without a second camera compensating underneath
it. Selecting a semantic asks the outer camera to keep the counter-scaled hit
target visible. On narrow viewports the inspector remains a bottom sheet and
the safe area shrinks vertically instead of horizontally; on very short
viewports it may truthfully shrink below the old 120px floor, including to zero,
rather than extending underneath the inspector.

#### Pointer and keyboard

Scene geometry is stamped with its semantic key at creation. Every non-pan
pointer click raycasts even when the browser event target is a semantic button,
so the nearest rendered machine, crate, or worker wins over later DOM rectangle
order. Equal-depth results have a stable semantic tie-break. A ray miss falls
back to the expanded DOM target, while keyboard-generated clicks remain
authoritative on the focused DOM button. Selection and focus draw an in-scene
ring tied to the projected geometry.

#### Renderer-state switching

When the renderer is fallback, pending, or unavailable, the plant renders the
classic bitmap and classic controls. When it is ready, it renders the projected
overlay. Never both — duplicate accessible controls would be worse than drift.
Switching sources changes only where a control is drawn; the outer camera pose
does not change. Before replacing controls the Plant captures
`document.activeElement`'s semantic anchor and restores real keyboard focus to
the matching projected or classic control after the switch.

#### Measurement

The probe records the projected screen point and the measured DOM anchor centre
for every semantic, plus the safe rectangle, maximum and mean drift, collision
count, clipped-label count, and inspector occlusion counts. The harness proves
alignment at 1440x1000, 1280x800, and 1100x900 with the inspector open and
closed, then measures rendered rectangles at outer zoom 0.6, 0.8, 1, 1.25, and
2. It also verifies rigid pan, renderer fallback pose continuity, and the
640x320 and 360x200 short-viewport safe-area cases. The gates remain maximum
drift at most 6 CSS pixels, zero priority-label overlaps or clipping, zero
occluded selected or critical semantics, minimum hit sizes, and Fit All
contained in the safe rectangle.

Alignment is read through `remeasure()` rather than from whatever the last
camera change published. The runtime publishes its camera synchronously and the
overlay commits on the next animation frame, so a same-tick reading can report
one frame of React latency as drift; the harness settles the page and then asks
for a fresh measurement, which makes the number it asserts on the steady state
the contract actually promises.

Palette and Risk visual quality are covered by the following work package.

### Work package 5: authored palette, truthful Risk, and legibility gates

The Plant's first WebGL passes were procedural: the scene read UI panel tokens,
the Risk lens dimmed everything it could not confirm, and both decisions were
invisible to the test suite. WP-5 replaces that with an authored visual system
whose contract is machine-checked.

#### Authored scene palette, not UI tokens

`plantPalette.ts` owns the plant's colour. It is a plain data module: two frozen
`PlantScenePalette` records — `PLANT_LIGHT_SCENE_PALETTE` and
`PLANT_DARK_SCENE_PALETTE` — naming every surface the scene paints
(background, floor, deck pads and their edges, aisles, walls and trim,
structure, machine bodies and caps, guardrails, consoles, lights, status,
worker, crate, focus rings, and the unknown and stale treatments) plus the three
light colours.

Reading a scene colour out of a UI token was the original defect. Panel tokens
are authored for text on a card; in dark theme the token that stood in for the
key light was very nearly black, so the dark Plant rendered as a black rectangle
with faint grey rectangles on it. The palette is therefore authored
independently of the panel system, and the key, fill, and rim entries are real
light colours in both themes — a dark theme is a dark *room*, not an unlit one.
Brand accent survives only as trim: signage, focus rings, and console faces.

Theme changes are a material and uniform update. `plantScenePalette(theme)`
returns the record, the scene graph writes it into the retained materials,
instanced colour attributes, and light intensities, and nothing is rebuilt: the
harness asserts `scene.builds === 0`, `scene.disposals === 0`, and
`renderer.contexts === 0` across a theme swap, and separately asserts that the
rendered pixels actually changed, so "no rebuild" cannot be satisfied by not
repainting.

#### Industrial art direction

The hall is neutral-first and diagram-grade. No fog, no bloom, no tone mapping
(`NoToneMapping`), no simulated activity, and no photorealism: everything drawn
corresponds to something read. Depth comes from cheap, honest cues — a floor
grid, deck pads with contrasting kerbs, aisles between bays, guardrails,
consoles, wall trim, and contact shadows — not from atmosphere.

Stage kind is carried by silhouette so it survives monochrome, colour-vision
differences, and forced colours:

| Kind | Silhouette |
| --- | --- |
| `agentic` | Tall body with a raised, stepped cap |
| `gate` | Narrow portal frame with a cross-beam |
| `evaluator` | Wide body with an inspection arm |
| `deterministic` | Plain rectangular press |
| `terminal` | Squat body with a capped outfeed |
| `unknown` | Open wireframe cage |

`factoryPlantLayout.ts` carries `evaluator` alongside the model's
`GraphNodeKind`, so an evaluator stage is a distinct machine rather than a
recoloured deterministic one. Safety markings are restrained: kerb hatching on
deck edges and aisle margins, nothing decorative.

#### Truthful Risk model

`plantRisk.ts` is pure — types and functions, no React, no Three.js, no DOM —
and is unit-tested on its own. Its precedence is fixed:

    blocked > held > impeded > unknown/incomplete > healthy

`isConfirmedRiskLevel` admits only `blocked`, `held`, and `impeded`. Everything
else is a completeness question, and completeness is modelled *orthogonally* to
hazard. `PlantRiskCompleteness` records `stale`, `degraded`, `truncated`,
`unreadTopology`, and `unknownCapacity` as separate booleans; a scene can be
entirely healthy and entirely incomplete at the same time, and the model says
so. An unconfirmed carrier (`confirmed === false`) or a station reporting
`unknown` never resolves to `blocked` or `held` — it resolves to `unknown`, and
`plantCompletenessGaps` explains why.

The headline vocabulary is a closed set, and the distinction the operator needs
is in the wording rather than in a footnote:

- `No confirmed current risk` — only when the read is complete and fresh.
- `No confirmed risk in what was read` — clear, but incomplete; suffixed with
  `· N unread` when topology was observed but not read.
- `N confirmed blocked` / `N confirmed on human hold` / `N confirmed impeded`.

Freshness is threaded, never inferred. `FactoryPage` already computes
`live | refreshing | degraded` from the query state for its own status chip;
that same value is passed to `FactoryPlant`, into the runtime, and into
`summarizePlantRisk`. The harness reads the DOM chip's `data-state` and the
probe's `model.freshness` in a single evaluate and requires them to be equal, so
the scene cannot drift into guessing staleness from what it happens to be
painting.

#### Risk visual treatment

The Risk lens no longer erases the hall. There is no fog and no global
near-blackout. At-risk entities render at full legibility with a status beacon,
a ring, and always-priority text. Healthy context stays visible: it is
desaturated (`PLANT_RISK_CONTEXT_DESATURATION`) and slightly dimmed, with
opacity floored at `PLANT_RISK_CONTEXT_MIN_OPACITY` = 0.75, which the harness
verifies against rendered pixels rather than trusting the constant.

Unknown and incomplete entities get their own neutral, wireframe-and-hatch
treatment. They are deliberately *not* hold amber: painting an unread thing in
the colour of a confirmed human hold is the exact lie this work package exists
to remove.

Reduced motion makes beacons static rather than removing them, and the healthy
operating motion that the World lens shows stays suppressed in Risk in every
motion mode — Risk is a reading, not an animation.

#### Contrast gates and luminance bands

`PLANT_CONTRAST_GATES` is published from the palette module and asserted twice:
once as pure arithmetic in `plantPalette.test.ts`, and once against rendered
browser pixels in the harness.

| Gate | Minimum |
| --- | --- |
| Machine body vs floor | 3:1 |
| Risk marker vs machine body | 3:1 |
| Deck kerb vs deck pad | 3:1 |
| Focus ring vs machine body | 3:1 |
| Key text vs background | 4.5:1 |

`PLANT_LUMINANCE_BANDS` closes the loophole that a contrast ratio alone cannot:
a scene can pass every ratio and still be a black rectangle. The bands assert
the *rendered* frame's mean luminance, its dark-pixel ratio (below 0.2), and its
near-black ratio (below 0.06). Dark World and dark Risk must land inside an
authored band rather than at the ~98% black the procedural version produced.

Arithmetic on authored albedo is still not proof, because a lighting rig can
violate it silently: a body authored at 5:1 against the deck renders as a black
blob once its vertical faces fall away from the key light. The harness therefore
also measures contrast on the pixels the operator sees. It samples each
machine's body at the projected station anchor — the same anchors the alignment
checks prove sit within 6 CSS pixels of the geometry — against the deck
immediately beside that machine, and requires 3:1 in both themes. Only neutral
pixels count as body, so a chromatic beacon cannot stand in for the surface it
is supposed to be readable against. This gate is what caught the first rig,
whose key-to-fill ratio crushed every vertical face, and the `unknown` status
painting a machine *body* with the near-black unknown *marker* colour.

Contrast geometry constrains the palette more than it first appears. Requiring
both `machine vs deck >= 3` and `kerb vs deck >= 3` in a dark theme forces the
machine bodies *lighter* than the deck — a machine darker than a deck at any
usable deck luminance cannot reach 3:1 — which in turn forces the deck up out of
near-black. The dark palette is authored around that constraint, not tuned by
eye.

The rig itself is authored for a matte, diagram-grade read: fill carries most of
the energy so shaded faces stay near their albedo, and the key carries enough to
keep contact shadows as a depth cue without crushing anything into a silhouette
of one flat value.

#### Forced colours

When `(forced-colors: active)` matches, the Plant does not mount WebGL at all.
A canvas is exempt from the system's colour substitution, so a WebGL plant in
forced-colours mode is a rectangle the operator's own accessibility setting
cannot reach. `plantForcedColors.ts` detects the query and observes changes, and
`FactoryWebGLScene` skips mounting and hands over the DOM plant, which the
system can recolour: system-colour outlines, status carried by shape, and no
semantic hidden behind a colour that is about to be replaced. The harness runs a
dedicated Chrome scenario under `Emulation.setEmulatedMedia` and asserts that
WebGL never initialises and that the DOM plant is complete.

#### Asset and load budget

`/factory-plant-base.png` is 540 KB and is now requested only on a degraded
path. The successful WebGL-ready path never fetches it, which the harness
asserts by recording every request for it, tagged with the scenario that caused
it, and requiring zero on the healthy-context scenarios.

The tradeoff is deliberate and bounded: on context loss the complete fallback is
the authored CSS backdrop plus the DOM machines, crates, signs, and controls —
all already in the document — and the bitmap arrives afterwards as progressive
enhancement. The harness times this in-page, from `loseContext()` to a committed
fallback with a backdrop and controls present, and gates it at 200ms.

Three.js stays lazy. The rendering budget is draw calls at most 120, triangles
at most 180k, GPU backing pixels capped by `PLANT_MAX_BACKING_PIXELS`, and zero
animation frames while idle or backgrounded.

#### Vocabulary

The HUD and `FloorLegend` publish the same grammar the inspector uses: the
silhouette table above, the status set (`running`, `impeded`, `held`, `blocked`,
`idle`, `unknown`), and the completeness modifiers (`stale`, `degraded`,
`truncated`, `unread`, `unknown capacity`). Selected and focused entities carry
an in-scene ring at 3:1 or better against the body they surround. Status and
completeness wording is generated from `plantRisk.ts` in every surface, so the
legend cannot describe one vocabulary while the inspector describes another.

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
