/**
 * The Plant's single frame scheduler.
 *
 * One mounted canvas owns exactly one scheduler, and one scheduler owns at most
 * one outstanding animation frame. Everything that wants pixels — a model
 * update, a lens or theme change, a resize, a restored context, or truthful
 * operating motion — goes through here, so an update can never race a second
 * loop into existence and a stopped loop can never keep a frame alive.
 *
 * The scheduler is deliberately free of Three.js and of the DOM: the host
 * supplies the clock and the frame primitives, so it can be driven
 * deterministically in tests.
 */

export interface PlantSchedulerHost {
  now: () => number;
  requestAnimationFrame: (callback: (time: number) => void) => number;
  cancelAnimationFrame: (handle: number) => void;
}

export interface PlantFrame {
  /** Seconds since the scheduler started, accumulated from capped deltas. */
  elapsed: number;
  /** Capped seconds since the previous frame. */
  delta: number;
  /** True when the frame was produced by an animation frame callback. */
  raf: boolean;
  /** True when the frame belongs to a continuous motion loop. */
  motion: boolean;
}

export interface PlantSchedulerOptions {
  host: PlantSchedulerHost;
  render: (frame: PlantFrame) => void;
  /** Reports every animation frame request, one-shot or continuous. */
  onSchedule?: () => void;
  /**
   * Largest delta a single frame may advance the animation clock.
   *
   * A backgrounded tab, a paused debugger, or a lost context hands the next
   * frame a delta measured in seconds. Uncapped, every deterministic phase
   * jumps forward at once and the plant appears to teleport when the operator
   * returns. Capping keeps the motion continuous instead of truthful about
   * wall-clock time, which is what a status animation actually needs.
   */
  maxDeltaSeconds?: number;
}

export interface PlantSchedulerState {
  motion: boolean;
  paused: boolean;
  disposed: boolean;
  /** An animation frame is currently outstanding. */
  scheduled: boolean;
  /** A render was requested but could not be scheduled yet. */
  pending: boolean;
  frames: number;
  elapsed: number;
}

export interface PlantScheduler {
  /** Coalesced single frame. Never adds a second frame to a running loop. */
  requestRender: () => void;
  /** Synchronous frame, used for the first paint and for measurement. */
  renderNow: () => void;
  setMotion: (motion: boolean) => void;
  setPaused: (paused: boolean) => void;
  state: () => PlantSchedulerState;
  dispose: () => void;
}

const DEFAULT_MAX_DELTA_SECONDS = 0.1;

export function createPlantScheduler(options: PlantSchedulerOptions): PlantScheduler {
  const { host, render } = options;
  const maxDelta = options.maxDeltaSeconds ?? DEFAULT_MAX_DELTA_SECONDS;
  let handle: number | undefined;
  let lastTime: number | undefined;
  let elapsed = 0;
  let frames = 0;
  let motion = false;
  let paused = false;
  let disposed = false;
  let pending = false;

  const cancel = () => {
    if (handle === undefined) {
      return;
    }
    host.cancelAnimationFrame(handle);
    handle = undefined;
  };

  const schedule = () => {
    if (disposed || paused || handle !== undefined) {
      return;
    }
    options.onSchedule?.();
    handle = host.requestAnimationFrame(onFrame);
  };

  const runFrame = (time: number, raf: boolean) => {
    const delta =
      lastTime === undefined
        ? 0
        : Math.min(Math.max(0, (time - lastTime) / 1000), maxDelta);
    lastTime = time;
    elapsed += delta;
    frames += 1;
    pending = false;
    render({ delta, elapsed, motion, raf });
  };

  function onFrame(time: number) {
    handle = undefined;
    if (disposed) {
      return;
    }
    runFrame(time, true);
    if ((motion && !paused && !disposed) || pending) {
      schedule();
    }
  }

  return {
    requestRender: () => {
      if (disposed) {
        return;
      }
      pending = true;
      schedule();
    },
    renderNow: () => {
      if (disposed) {
        return;
      }
      if (paused) {
        pending = true;
        return;
      }
      runFrame(host.now(), false);
      if (motion) {
        schedule();
      }
    },
    setMotion: (next: boolean) => {
      if (disposed || motion === next) {
        return;
      }
      motion = next;
      if (motion) {
        // A resumed loop must not bill the operator for the idle interval.
        lastTime = undefined;
        schedule();
      } else if (!pending) {
        cancel();
      }
    },
    setPaused: (next: boolean) => {
      if (disposed || paused === next) {
        return;
      }
      paused = next;
      if (paused) {
        cancel();
        lastTime = undefined;
        return;
      }
      lastTime = undefined;
      if (motion || pending) {
        schedule();
      }
    },
    state: () => ({
      disposed,
      elapsed,
      frames,
      motion,
      paused,
      pending,
      scheduled: handle !== undefined,
    }),
    dispose: () => {
      if (disposed) {
        return;
      }
      disposed = true;
      motion = false;
      pending = false;
      cancel();
    },
  };
}
