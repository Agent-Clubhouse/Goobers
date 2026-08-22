# The residency model: a lifecycle for gaggle memory

Status: design framing / roadmap. The shipped system (`agent-memory.md`) implements
a *flat, permanent* memory: proposals accumulate, the dream consolidates them into
active memory, recall injects them, forever, at a fixed cadence. This note describes
the lifecycle that should sit on top of it. Nothing here is built yet; it's the shape
the next phase should take.

## The flat model has no arc

The flat store treats every memory the same for its whole life: once promoted, it
lives in the recall set indefinitely, and every run pays the tax of pulling it. There
is no notion of a memory becoming *settled* — so well-established knowledge and a
just-learned hypothesis compete for the same context budget on equal terms, and the
dream keeps re-examining things that were decided long ago.

Medical residency is the missing frame. A resident does an assessment, then consults
the attending, who reviews the findings and gives feedback. Crucially, residency is
not a permanent rank — it is a front-loaded, high-supervision period *designed to
end*. The entire apparatus (rounds, case consults, attending review) exists to move
knowledge from the attending into the resident until the supervision can withdraw.

Standing up a gaggle from an experienced, memory-equipped practitioner (a long-lived
assistant project with years of accreted context) is exactly that transition. The
practitioner is the attending; the fresh gaggle is the resident; the memory system is
the teaching apparatus. That framing implies mechanisms the flat model lacks.

## The lifecycle

Memory should move through phases, per area, not sit flat forever:

1. **Intensive transfer** — the practitioner's accumulated knowledge is seeded into
   the gaggle (`init-from-claude`) and keeps flowing in (`sync-claude`). Recall is
   dense; the wizard gate fires often; most of what it catches becomes a memory.
2. **Supervised practice** — the gaggle does the work; the wizard reviews against the
   accumulated store; the dream consolidates the lessons that recur. This is the bulk
   of residency, and it is what the shipped system does today.
3. **Graduation into curriculum** — knowledge that has proven itself stops being a
   note you fetch and becomes part of who the agent *is*: it moves out of dynamic
   recall and into the goober's standing instructions. The area is now trusted; the
   gate can pull back.
4. **Supervision retargeted to the frontier** — mature areas run with light recall
   and a quiet gate; supervision concentrates where uncertainty still lives.

## Graduation is the core new mechanism

A resident does not consult the attending on their two-hundredth central line; that
knowledge moved from "look it up" into reflex. The gaggle equivalent: a memory that
has been recalled many times and *never violated*, in an area the wizard has gated
repeatedly and never blocked, has earned its way **out** of dynamic recall and into
the goober's standing `instructions.md` — the curriculum.

So the dream's job grows. Today it promotes proposals into active memory. Under the
residency model it also **graduates** settled active memory into the base
instructions and retires it from the recall set. The wizard's own hit-rate is the
graduation clock: when the gate stops firing on an area for long enough, that is the
signal to bake that area's memories into curriculum and pull the gate back.

## The dream does not go away — it retargets

The tempting end state is "residency completes, the dream shuts off." That is wrong,
for two reasons.

First, the world does not hold still. Upstream moves; new fragilities appear; a
refactor invalidates yesterday's settled procedure. An attending never stops
supervising the novel or high-stakes case — they stop supervising the *routine*. So a
mature gaggle still dreams, but only about areas still throwing new failures, and goes
quiet on the settled ones. The taper is per-area, driven by the local failure rate,
not a global off-switch.

Second, and more fundamentally: **the residents never grow their own brains.** A
stateless harness session internalizes nothing between runs. So "graduation" here
cannot mean the resident learned it. It can only mean the *curriculum* consolidated
it — knowledge moved from the dynamic recall layer to the static instructions layer.
The internalization lives in the config, not the agent. That is why the curriculum is
always load-bearing and supervision can never fully leave: there is no muscle memory
underneath the standing orders, only the standing orders.

## Attending lineage: gaggle → gaggle

A resident becomes an attending. A gaggle that has graduated — dense curriculum, quiet
frontier — is exactly what should seed the next one. `init-from-claude` bootstraps a
gaggle from a practitioner project; the mature form is **gaggle → gaggle**: a proven
workforce's consolidated curriculum becomes a new or forked gaggle's starting
knowledge, so residency is not re-run from scratch every time a workforce is stood up
for a new repository. The experienced practitioner teaching the next resident.

## The hard part: graduation criteria and calcification

Graduation is more dangerous than promotion, and the danger is specific. Once a memory
is baked into standing instructions and pulled from the recall set, if it *later*
becomes wrong it is now **invisible** — no longer surfaced for scrutiny, just quietly
shaping every run. Promotion adds something you can still see and challenge;
graduation removes it from view.

So a criterion like "recalled N times, never violated, wizard quiet for M runs" is a
starting point but not sufficient on its own. Graduation needs:

- the same source-skepticism ladder promotion uses (a human/practitioner-sourced
  lesson graduates more readily than a single-run observation);
- a **curriculum audit** — a periodic pass where the wizard re-examines what has been
  baked into instructions against recent reality, and can *demote* a graduated memory
  back into active recall (or archive) when the world has moved. The attending
  re-checking that the standing orders still hold.

Skip the audit and you get the failure mode of a residency program that stopped
updating its own teaching: confident, fluent, and years out of date. Calcified
doctrine is the specific risk this model must design against.

## Relationship to the shipped system

Everything here is additive on top of `agent-memory.md`:

- The store already separates `active/` from `proposed/`; graduation adds a third
  destination (the goober's `instructions.md`) and the audit adds a demotion path back.
- The dream workflow already emits a decisions file applied by `gaggle-memory promote`;
  graduation and demotion are new decision `op`s (`graduate`, `demote`) with their own
  code-enforced rules, and the curriculum audit is a new scheduled pass.
- Per-area supervision level (how dense recall is, how strict the gate) is new state
  the dream maintains from the wizard's hit-rate.

None of it changes the trust boundary: agents still only *propose*, and the wizard/
human still decide what graduates, what demotes, and what stays under supervision.
