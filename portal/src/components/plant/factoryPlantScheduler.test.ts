import { describe, expect, it } from "vitest";

import {
  createPlantScheduler,
  type PlantFrame,
  type PlantSchedulerHost,
} from "./factoryPlantScheduler";

/**
 * A deterministic animation-frame host.
 *
 * The scheduler's whole job is "at most one outstanding frame", which is only
 * provable if the test owns the frame queue instead of the browser.
 */
function createHost() {
  let time = 0;
  let nextHandle = 1;
  const queue = new Map<number, (time: number) => void>();
  let requests = 0;
  let cancels = 0;

  const host: PlantSchedulerHost = {
    cancelAnimationFrame: (handle) => {
      cancels += 1;
      queue.delete(handle);
    },
    now: () => time,
    requestAnimationFrame: (callback) => {
      requests += 1;
      const handle = nextHandle;
      nextHandle += 1;
      queue.set(handle, callback);
      return handle;
    },
  };

  return {
    host,
    get cancels() {
      return cancels;
    },
    get outstanding() {
      return queue.size;
    },
    get requests() {
      return requests;
    },
    advance(ms: number) {
      time += ms;
    },
    flush(ms = 16) {
      time += ms;
      const pending = [...queue.entries()];
      queue.clear();
      for (const [, callback] of pending) {
        callback(time);
      }
      return pending.length;
    },
  };
}

function collect() {
  const frames: PlantFrame[] = [];
  return { frames, render: (frame: PlantFrame) => frames.push(frame) };
}

describe("Plant frame scheduler", () => {
  it("keeps at most one animation frame outstanding while motion runs", () => {
    const clock = createHost();
    const { frames, render } = collect();
    const scheduler = createPlantScheduler({ host: clock.host, render });

    scheduler.setMotion(true);
    scheduler.requestRender();
    scheduler.requestRender();
    expect(clock.outstanding).toBe(1);

    clock.flush();
    expect(clock.outstanding).toBe(1);
    clock.flush();
    expect(clock.outstanding).toBe(1);
    expect(frames).toHaveLength(2);
    expect(frames.every((frame) => frame.raf)).toBe(true);

    scheduler.dispose();
  });

  it("coalesces repeated update renders into a single frame", () => {
    const clock = createHost();
    const { frames, render } = collect();
    const scheduler = createPlantScheduler({ host: clock.host, render });

    scheduler.requestRender();
    scheduler.requestRender();
    scheduler.requestRender();
    expect(clock.requests).toBe(1);

    clock.flush();
    expect(frames).toHaveLength(1);
    expect(clock.outstanding).toBe(0);

    scheduler.dispose();
  });

  it("stops the loop when motion stops and resumes without a time jump", () => {
    const clock = createHost();
    const { frames, render } = collect();
    const scheduler = createPlantScheduler({ host: clock.host, render });

    scheduler.setMotion(true);
    clock.flush();
    clock.flush();
    scheduler.setMotion(false);
    expect(clock.outstanding).toBe(0);
    expect(scheduler.state().motion).toBe(false);

    const framesWhileIdle = frames.length;
    clock.advance(30_000);
    expect(frames).toHaveLength(framesWhileIdle);

    scheduler.setMotion(true);
    clock.flush();
    // The resumed frame must not bill the idle interval to the animation clock.
    expect(frames.at(-1)?.delta).toBe(0);

    scheduler.dispose();
  });

  it("caps the delta so a backgrounded tab cannot teleport the animation", () => {
    const clock = createHost();
    const { frames, render } = collect();
    const scheduler = createPlantScheduler({
      host: clock.host,
      maxDeltaSeconds: 0.1,
      render,
    });

    scheduler.setMotion(true);
    clock.flush(16);
    clock.flush(5_000);

    expect(frames.at(-1)?.delta).toBe(0.1);
    expect(scheduler.state().elapsed).toBeLessThanOrEqual(0.2);

    scheduler.dispose();
  });

  it("pauses hard for context loss and never renders while paused", () => {
    const clock = createHost();
    const { frames, render } = collect();
    const scheduler = createPlantScheduler({ host: clock.host, render });

    scheduler.setMotion(true);
    clock.flush();
    const before = frames.length;

    scheduler.setPaused(true);
    expect(clock.outstanding).toBe(0);
    scheduler.requestRender();
    scheduler.renderNow();
    expect(frames).toHaveLength(before);

    scheduler.setPaused(false);
    expect(clock.outstanding).toBe(1);
    clock.flush();
    expect(frames.length).toBeGreaterThan(before);

    scheduler.dispose();
  });

  it("renders synchronously for the first paint", () => {
    const clock = createHost();
    const { frames, render } = collect();
    const scheduler = createPlantScheduler({ host: clock.host, render });

    scheduler.renderNow();
    expect(frames).toHaveLength(1);
    expect(frames[0].raf).toBe(false);
    expect(clock.outstanding).toBe(0);

    scheduler.dispose();
  });

  it("cancels the outstanding frame once and stays disposed", () => {
    const clock = createHost();
    const { frames, render } = collect();
    const scheduler = createPlantScheduler({ host: clock.host, render });

    scheduler.setMotion(true);
    expect(clock.outstanding).toBe(1);

    scheduler.dispose();
    scheduler.dispose();
    expect(clock.cancels).toBe(1);
    expect(clock.outstanding).toBe(0);

    scheduler.requestRender();
    scheduler.renderNow();
    scheduler.setMotion(true);
    expect(clock.requests).toBe(1);
    expect(frames).toHaveLength(0);
    expect(scheduler.state().disposed).toBe(true);
  });
});
