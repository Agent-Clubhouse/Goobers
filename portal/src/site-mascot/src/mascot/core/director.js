// mascot-core: Director API (MS4/#138, part of #28). MASCOTS.md §6's
// composition API for a page team — framework-free (no React import here;
// mascot-react's GooberStage owns the THREE.Scene and constructs one
// Director per stage, per §7's "Director... framework-free TS" list).
//
// Honest scope note: §6's illustrative example uses `pip.walkAlong(path, t)`
// and `pip.interact('#gate', 'press')`. Neither exists on GooberActor
// (MS3) — the live demo (goober.js) never built them either; MS3 ported
// what the demo actually has (walkTo/hop/wave/emote/sleep-wake), not the
// full aspirational §6 vocabulary. MS5's actual described scope (scroll-fall
// companion + hero idler, floor-anchor + idle vocabulary only) doesn't need
// walkAlong or interact, so this Director targets the trigger/binding
// infrastructure — scroll/enter/event — composing with the real actor
// methods that exist today, rather than inventing gesture/path-following
// primitives no page yet needs. Adding walkAlong/interact later is a
// GooberActor change, not a Director one.
import { clamp } from '../../../demo/src/springs.js';
import { createRig } from './procedural-rig.js';
import { GooberActor } from './actor.js';
import { CAST } from '../../../demo/src/cast.js';

export const scroll = (selector) => ({ type: 'scroll', selector });
export const enter = (selector) => ({ type: 'enter', selector });
export const event = (name) => ({ type: 'event', name });

/**
 * One Director per stage. `scene` is the THREE.Scene actors' rigs get added
 * to.
 *
 * There is no longer an `assetUrl`: rigs are built from the reference model
 * (see procedural-rig.js) rather than fetched as a baked glTF, so a stage has
 * no asset to point at and no download to gate on. Callers that still pass
 * one are tolerated and ignored, so an in-flight page doesn't break on the
 * swap — remove it at the call site.
 */
export class Director {
  constructor(scene, _legacyOptions) {
    this.scene = scene;
    this.cast_ = {};
    this.avoidSelectors = [];
    this._eventTarget = new EventTarget();
    this._scrollBindings = [];
    this._observers = [];
    this._onScroll = () => { for (const b of this._scrollBindings) b(); };
    addEventListener('scroll', this._onScroll, { passive: true });
  }

  /**
   * Spawns a named cast member at a DOM anchor. `name` resolves against
   * CAST by default (MASCOTS.md §4); pass `preset` to cast a different
   * personality under a page-local name. `floor` is currently only
   * 'baseline' (y=0) — anchor-driven floor binding is FloorAnchor's job
   * (MS2); a page wires that up separately and drives the actor's y via
   * ctx.floorY in its own update loop, same as demo/src/main.js does.
   */
  async cast(name, { at, preset } = {}) {
    const p = preset || CAST.find((c) => c.name.toLowerCase() === name.toLowerCase());
    if (!p) throw new Error(`Director.cast: no CAST entry or preset for "${name}"`);
    const home = at ? this._anchorHome(at) : { x: 0, z: 0 };
    // Synchronous now (no asset fetch), but cast() stays async so existing
    // `await director.cast(...)` call sites are unaffected.
    const rig = createRig(p);
    this.scene.add(rig.root);
    // The reference model owns its contact shadow per-instance; the glTF
    // canon had excluded it and left it to the stage (MS4/MS6).
    if (rig.shadow) this.scene.add(rig.shadow);
    const actor = new GooberActor(rig, p, home);
    this.cast_[name] = actor;
    return actor;
  }

  get(name) {
    return this.cast_[name];
  }

  /** Every currently-cast actor, for a stage's render loop to update() each frame. */
  actors() {
    return Object.values(this.cast_);
  }

  /** Registers a DOM selector as a zone goobers should never walk through (§8's exclusion zones). Returns live rects via exclusionRects(), not a pathfinding guarantee — a consumer (e.g. a future spatial-layer integration) is responsible for actually avoiding them. */
  avoid(selector) {
    this.avoidSelectors.push(selector);
  }

  exclusionRects() {
    return this.avoidSelectors
      .map((sel) => document.querySelector(sel)?.getBoundingClientRect())
      .filter(Boolean);
  }

  /** Binds a trigger (scroll/enter/event) to a handler. scroll handlers receive a 0..1 section-progress float; enter fires once per intersection; event handlers receive whatever detail Director.emit() was called with. */
  on(trigger, handler) {
    if (trigger.type === 'scroll') return this._bindScroll(trigger.selector, handler);
    if (trigger.type === 'enter') return this._bindEnter(trigger.selector, handler);
    if (trigger.type === 'event') {
      const listener = (e) => handler(e.detail);
      this._eventTarget.addEventListener(trigger.name, listener);
      return () => this._eventTarget.removeEventListener(trigger.name, listener);
    }
    throw new Error(`Director.on: unknown trigger type "${trigger.type}"`);
  }

  /** Fires a named event() trigger — the page's own logic decides when (e.g. on a gate-pass), same as MASCOTS.md §6's `event('gate:pass')` example. */
  emit(name, detail) {
    this._eventTarget.dispatchEvent(new CustomEvent(name, { detail }));
  }

  _bindScroll(selector, handler) {
    const el = document.querySelector(selector);
    if (!el) throw new Error(`Director.on(scroll): target "${selector}" not found`);
    const update = () => {
      const rect = el.getBoundingClientRect();
      const vh = innerHeight;
      // 0 as the section's top enters the viewport bottom, 1 as its bottom
      // leaves the viewport top — a simple, page-agnostic progress measure.
      const t = clamp((vh - rect.top) / (rect.height + vh), 0, 1);
      handler(t);
    };
    this._scrollBindings.push(update);
    update();
    return () => { this._scrollBindings = this._scrollBindings.filter((b) => b !== update); };
  }

  _bindEnter(selector, handler) {
    const el = document.querySelector(selector);
    if (!el) throw new Error(`Director.on(enter): target "${selector}" not found`);
    const io = new IntersectionObserver((entries) => {
      for (const e of entries) if (e.isIntersecting) handler();
    });
    io.observe(el);
    this._observers.push(io);
    return () => io.disconnect();
  }

  _anchorHome(selector) {
    const el = document.querySelector(selector);
    if (!el) throw new Error(`Director.cast: anchor "${selector}" not found`);
    // Placeholder screen-to-world mapping (x only, centered) — a real
    // implementation needs the stage's camera/viewport, which the Director
    // doesn't own (GooberStage does). Sufficient for a single-anchor hero
    // idler; a full DOM-to-world projection is MS5 integration work.
    const rect = el.getBoundingClientRect();
    const cx = innerWidth / 2;
    return { x: ((rect.left + rect.width / 2 - cx) / innerWidth) * 4, z: 0 };
  }

  /** Tears down all listeners/observers — call on stage unmount. */
  dispose() {
    removeEventListener('scroll', this._onScroll);
    for (const io of this._observers) io.disconnect();
    this._scrollBindings = [];
    // Rigs are per-instance geometry/materials now rather than clones of one
    // loaded asset, so a stage that mounts and unmounts (an Astro island on a
    // client-side nav, the testbed's re-cast button) leaks without this.
    for (const actor of this.actors()) {
      this.scene.remove(actor.rig.root);
      if (actor.rig.shadow) this.scene.remove(actor.rig.shadow);
      actor.rig.dispose?.();
    }
    this.cast_ = {};
  }
}
