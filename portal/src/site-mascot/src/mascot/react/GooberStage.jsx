// mascot-react: <GooberStage> island (MS4/#138, part of #28). SSR-safe per
// §7 — every THREE.js/WebGL/window/document touch below lives inside
// useEffect, which React guarantees never runs during server rendering; the
// component's render function only returns plain JSX (refs + inline
// styles), so there's nothing here for SSR to choke on.
//
// A parent (an Astro page's client-side script) drives the stage through
// the forwarded ref's `.director` — `stage.current.director.cast(...)`,
// `.on(scroll(...), ...)`, per MASCOTS.md §6's Director API (director.js).
// GooberStage owns the THREE.Scene/renderer/render-loop; Director owns
// casting/triggers and stays framework-free (§7's package split), which is
// why it's constructed here rather than inside GooberStage's own module.
//
// Most Astro pages aren't a React tree, so a ref often isn't reachable from
// a plain <script> beside the island — listen for the `stage:ready`
// CustomEvent on the container instead (`e.detail.director`), no ref
// needed. Verified against exactly that pattern (a page-level <script>,
// not a React effect): if you instead try to catch this event from
// *another* React component's useEffect, mount ordering can race it —
// GooberStage's own effect (which dispatches the event) can run before a
// sibling/parent's effect finishes attaching its listener, so the event
// fires into nothing. React-to-React consumers should use the forwarded
// ref instead; the event is specifically for non-React listeners, which
// don't have this race since they attach before React hydrates at all.
//
// Two more per-frame hooks for a page's own render-loop needs,
// both optional and no-op-compatible with the defaults if omitted:
// `getFloorY(actor)` (per-actor floor height — different actors can stand
// on different floors, e.g. one bound to a FloorAnchor, one static) and
// `onFrame(dt, director)` (runs after every actor's update(), before
// render — for cross-actor concerns no single actor's own update() can
// see, e.g. spatial.js's applySeparation(director.actors(), dt) for the
// §6 contact-bump mechanic).
//
// Static fallback (Tier R per §3, not Tier F — §3's 2026-08-02 correction)
// is passed in as the `fallback` prop and cross-fades out once the canvas
// is ready, per §7's "swap is a fade on a beat, not a pop." The actual
// WebGL-unavailable / reduced-motion / Save-Data fallback *decision* is
// MS6's job (this component always attempts to boot); a page wires that
// gating before choosing whether to render <GooberStage> at all.
import { forwardRef, useEffect, useImperativeHandle, useRef, useState } from 'react';
import * as THREE from 'three';
import { Director } from '../core/director.js';
import { CANON_FOV, applyCanonLighting } from '../core/stage-look.js';

const FADE_MS = 300;

export const GooberStage = forwardRef(function GooberStage(
  // `assetUrl` is gone: rigs are built from the reference model, not fetched
  // (see core/procedural-rig.js). Callers still passing it are harmless —
  // it is simply not read — but it should be removed at the call site.
  { fallback, className, style, getFloorY, onFrame, isPaused, getTimeScale }, ref,
) {
  const containerRef = useRef(null);
  const canvasRef = useRef(null);
  const directorRef = useRef(null);
  const [ready, setReady] = useState(false);

  useImperativeHandle(ref, () => ({
    get director() { return directorRef.current; },
  }), []);

  useEffect(() => {
    const container = containerRef.current;
    let renderer;
    try {
      renderer = new THREE.WebGLRenderer({ canvas: canvasRef.current, antialias: true, alpha: true });
    } catch {
      return; // no WebGL2 — the fallback stays up, same precedent as src/mascot/stage.js
    }
    renderer.setPixelRatio(Math.min(devicePixelRatio, 2));
    renderer.setClearColor(0x000000, 0);

    const scene = new THREE.Scene();
    // Lens and lights come from stage-look.js, not from here — this stage
    // had drifted to its own FOV (35 vs the lander's 42) and was missing the
    // fill light entirely, which made its goobers read harder on the shadow
    // side than the reference no matter what the geometry did.
    const camera = new THREE.PerspectiveCamera(CANON_FOV, 1, 0.05, 30);
    camera.position.set(0, 1.2, 3);
    applyCanonLighting(scene);

    const director = new Director(scene);
    directorRef.current = director;

    function resize() {
      const w = container.clientWidth, h = container.clientHeight;
      if (!w || !h) return;
      renderer.setSize(w, h, false);
      camera.aspect = w / h;
      camera.updateProjectionMatrix();
    }
    const resizeObserver = new ResizeObserver(resize);
    resizeObserver.observe(container);
    resize();

    // §7 performance budget: "Tab hidden or stage off-viewport: renderer
    // paused, zero rAF work."
    let rafId = null;
    let visible = true, focused = !document.hidden;
    const clock = new THREE.Clock();
    function frame() {
      rafId = requestAnimationFrame(frame);
      const dt = Math.min(clock.getDelta(), 0.05);
      if (isPaused?.()) {
        renderer.render(scene, camera);
        return;
      }
      let scaledDt = dt * Math.max(1, getTimeScale?.() ?? 1);
      while (scaledDt > 0) {
        const stepDt = Math.min(scaledDt, 0.05);
        for (const actor of director.actors()) {
          actor.update(stepDt, { floorY: getFloorY ? getFloorY(actor) : 0 });
        }
        // Runs after actor physics, before render — a page's own script uses
        // this for cross-actor concerns the actors' own update() can't see,
        // e.g. calling spatial.js's applySeparation(director.actors(), dt)
        // for the §6 contact-bump mechanic (MS5's scroll-fall companion +
        // hero idler need this; GooberStage itself stays actor-count-agnostic
        // rather than hardcoding a two-actor assumption).
        if (onFrame) onFrame(stepDt, director);
        scaledDt -= stepDt;
      }
      renderer.render(scene, camera);
    }
    function sync() {
      const shouldRun = visible && focused;
      if (shouldRun && rafId === null) { clock.getDelta(); frame(); }
      else if (!shouldRun && rafId !== null) { cancelAnimationFrame(rafId); rafId = null; }
    }
    const intersectionObserver = new IntersectionObserver(([entry]) => {
      visible = entry.isIntersecting;
      sync();
    });
    intersectionObserver.observe(canvasRef.current);
    const onVisibility = () => { focused = !document.hidden; sync(); };
    document.addEventListener('visibilitychange', onVisibility);

    setReady(true);
    sync();
    // Astro pages drive the stage from a plain <script>, not a React
    // parent — useImperativeHandle's ref only reaches other React code.
    // This is the non-React path to the same Director instance:
    // document.querySelector('#my-stage').addEventListener('stage:ready',
    // (e) => { e.detail.director.cast(...); e.detail.camera.position.z = 5; }).
    // Bubbles so a page can listen
    // on an ancestor instead of needing the exact container element.
    container.dispatchEvent(new CustomEvent('stage:ready', { detail: { director, camera, scene }, bubbles: true }));

    return () => {
      if (rafId !== null) cancelAnimationFrame(rafId);
      intersectionObserver.disconnect();
      resizeObserver.disconnect();
      document.removeEventListener('visibilitychange', onVisibility);
      director.dispose();
      renderer.dispose();
    };
  }, []);

  return (
    <div ref={containerRef} className={className} style={{ position: 'relative', ...style }} aria-hidden="true">
      {fallback && (
        <div style={{ position: 'absolute', inset: 0, opacity: ready ? 0 : 1, transition: `opacity ${FADE_MS}ms ease`, pointerEvents: 'none' }}>
          {fallback}
        </div>
      )}
      <canvas
        ref={canvasRef}
        style={{ position: 'absolute', inset: 0, width: '100%', height: '100%', display: 'block', opacity: ready ? 1 : 0, transition: `opacity ${FADE_MS}ms ease` }}
      />
    </div>
  );
});
