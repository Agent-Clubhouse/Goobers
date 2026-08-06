import { StrictMode } from "react";
import { act, cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { FactoryWebGLScene } from "./FactoryWebGLScene";
import type {
  FactoryWebGLRuntime,
  FactoryWebGLRuntimeOptions,
  FactoryWebGLRuntimeUpdate,
} from "./plant/factoryWebGLRuntime";
import { plantFixture } from "../test/plantFixtures";

/**
 * The adapter is judged on one thing: React props must reach a runtime that
 * already exists. Any code path that builds a second runtime for a theme, lens,
 * or model change is the defect this component was rewritten to remove.
 */

interface FakeRuntime extends FactoryWebGLRuntime {
  updates: FactoryWebGLRuntimeUpdate[];
  disposals: number;
}

function createHarness(options: { fail?: boolean } = {}) {
  const runtimes: FakeRuntime[] = [];
  const states: Array<(state: "pending" | "ready" | "fallback") => void> = [];
  const createRuntime = (runtimeOptions: FactoryWebGLRuntimeOptions) => {
    if (options.fail) {
      return undefined;
    }
    const runtime = {
      disposals: 0,
      dispose: () => {
        runtime.disposals += 1;
      },
      inspect: () => {
        throw new Error("not used");
      },
      resize: () => {},
      scene: undefined as never,
      update: (next: FactoryWebGLRuntimeUpdate) => {
        runtime.updates.push(next);
      },
      updates: [] as FactoryWebGLRuntimeUpdate[],
    } as unknown as FakeRuntime;
    if (runtimeOptions.onState) {
      states.push(runtimeOptions.onState);
    }
    runtimes.push(runtime);
    return runtime;
  };
  return { createRuntime, runtimes, states };
}

const fixture = plantFixture();

afterEach(() => {
  // Unmount first: a theme attribute removed under a live MutationObserver
  // would push a React state update outside of act.
  cleanup();
  document.documentElement.removeAttribute("data-theme");
});

describe("FactoryWebGLScene adapter", () => {
  it("creates the runtime once and forwards every prop change to it", () => {
    const { createRuntime, runtimes } = createHarness();
    const view = render(
      <FactoryWebGLScene
        animateTransitions
        createRuntime={createRuntime}
        freshness="live"
        lens="world"
        model={fixture.model}
        reducedMotion={false}
        layout={fixture.layout}
      />,
    );

    expect(runtimes).toHaveLength(1);
    expect(runtimes[0].updates).toHaveLength(1);

    view.rerender(
      <FactoryWebGLScene
        animateTransitions
        createRuntime={createRuntime}
        freshness="live"
        lens="risk"
        model={fixture.model}
        reducedMotion={false}
        layout={fixture.layout}
      />,
    );
    view.rerender(
      <FactoryWebGLScene
        animateTransitions
        createRuntime={createRuntime}
        freshness="live"
        lens="risk"
        model={fixture.model}
        reducedMotion
        layout={fixture.layout}
      />,
    );
    const refreshed = plantFixture();
    view.rerender(
      <FactoryWebGLScene
        animateTransitions
        createRuntime={createRuntime}
        freshness="live"
        lens="risk"
        model={refreshed.model}
        reducedMotion
        layout={refreshed.layout}
      />,
    );

    expect(runtimes).toHaveLength(1);
    expect(runtimes[0].disposals).toBe(0);
    expect(runtimes[0].updates).toHaveLength(4);
    expect(runtimes[0].updates.map((entry) => entry.lens)).toEqual([
      "world",
      "risk",
      "risk",
      "risk",
    ]);
    expect(runtimes[0].updates.at(-1)?.model).toBe(refreshed.model);
    expect(runtimes[0].updates.at(-1)?.layout).toBe(refreshed.layout);

    view.unmount();
    expect(runtimes[0].disposals).toBe(1);
  });

  it("does not re-render identical props into the runtime", () => {
    const { createRuntime, runtimes } = createHarness();
    const element = (
      <FactoryWebGLScene
        animateTransitions
        createRuntime={createRuntime}
        freshness="live"
        lens="world"
        model={fixture.model}
        reducedMotion={false}
        layout={fixture.layout}
      />
    );
    const view = render(element);
    view.rerender(element);
    view.rerender(element);

    expect(runtimes).toHaveLength(1);
    expect(runtimes[0].updates).toHaveLength(1);
    view.unmount();
  });

  it("forwards a document theme change without rebuilding the runtime", async () => {
    const { createRuntime, runtimes } = createHarness();
    render(
      <FactoryWebGLScene
        animateTransitions
        createRuntime={createRuntime}
        freshness="live"
        lens="world"
        model={fixture.model}
        reducedMotion={false}
        layout={fixture.layout}
      />,
    );
    expect(runtimes[0].updates.at(-1)?.theme).toBe("light");

    await act(async () => {
      document.documentElement.dataset.theme = "dark";
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(runtimes[0].updates.at(-1)?.theme).toBe("dark");
    expect(runtimes).toHaveLength(1);
    expect(runtimes[0].disposals).toBe(0);
  });

  it("reports renderer state on the element the stylesheet reads", async () => {
    const { createRuntime, states } = createHarness();
    render(
      <FactoryWebGLScene
        animateTransitions
        createRuntime={createRuntime}
        freshness="live"
        lens="world"
        model={fixture.model}
        reducedMotion={false}
        layout={fixture.layout}
      />,
    );
    const host = document.querySelector(".factory-plant-renderer");
    expect(host?.getAttribute("data-webgl")).toBe("pending");

    act(() => states[0]("ready"));
    expect(host?.getAttribute("data-webgl")).toBe("ready");

    act(() => states[0]("fallback"));
    expect(host?.getAttribute("data-webgl")).toBe("fallback");
    // The approved illustration stays mounted for every state.
    expect(document.querySelector(".factory-plant-backdrop")).toBeTruthy();
  });

  it("falls back when the runtime cannot be created", () => {
    const { createRuntime } = createHarness({ fail: true });
    render(
      <FactoryWebGLScene
        animateTransitions
        createRuntime={createRuntime}
        freshness="live"
        lens="world"
        model={fixture.model}
        reducedMotion={false}
        layout={fixture.layout}
      />,
    );
    expect(
      document.querySelector(".factory-plant-renderer")?.getAttribute("data-webgl"),
    ).toBe("fallback");
  });

  it("survives a StrictMode remount with one live runtime", () => {
    const { createRuntime, runtimes } = createHarness();
    const view = render(
      <StrictMode>
        <FactoryWebGLScene
          animateTransitions
          createRuntime={createRuntime}
          freshness="live"
          lens="world"
          model={fixture.model}
          reducedMotion={false}
          layout={fixture.layout}
        />
      </StrictMode>,
    );

    const live = runtimes.filter((runtime) => runtime.disposals === 0);
    expect(live).toHaveLength(1);
    expect(live[0].updates).toHaveLength(1);

    view.unmount();
    expect(runtimes.every((runtime) => runtime.disposals === 1)).toBe(true);
  });

  it("forwards the page's own read state instead of inferring it", () => {
    const { createRuntime, runtimes } = createHarness();
    const view = render(
      <FactoryWebGLScene
        animateTransitions
        createRuntime={createRuntime}
        freshness="live"
        lens="risk"
        model={fixture.model}
        reducedMotion={false}
        layout={fixture.layout}
      />,
    );
    expect(runtimes[0].updates.at(-1)?.freshness).toBe("live");

    view.rerender(
      <FactoryWebGLScene
        animateTransitions
        createRuntime={createRuntime}
        freshness="degraded"
        lens="risk"
        model={fixture.model}
        reducedMotion={false}
        layout={fixture.layout}
      />,
    );
    expect(runtimes[0].updates.at(-1)?.freshness).toBe("degraded");
    expect(runtimes).toHaveLength(1);
  });

  /*
   * Asset budget: the 540 KB illustration is the fallback's progressive
   * enhancement, not a prerequisite for the hall. The authored CSS backdrop is
   * always present, so a lost context still has a complete picture in the same
   * frame.
   */
  it("keeps the bitmap fallback off the successful WebGL path", () => {
    const { createRuntime, states } = createHarness();
    render(
      <FactoryWebGLScene
        animateTransitions
        createRuntime={createRuntime}
        freshness="live"
        lens="world"
        model={fixture.model}
        reducedMotion={false}
        layout={fixture.layout}
      />,
    );
    expect(document.querySelector(".factory-plant-backdrop-authored")).toBeTruthy();
    expect(document.querySelector("img.factory-plant-backdrop")).toBeNull();

    act(() => states[0]("ready"));
    expect(document.querySelector("img.factory-plant-backdrop")).toBeNull();

    act(() => states[0]("fallback"));
    const bitmap = document.querySelector("img.factory-plant-backdrop");
    expect(bitmap?.getAttribute("src")).toBe("/factory-plant-base.png");
  });
});

describe("FactoryWebGLScene under forced colours", () => {
  const realMatchMedia = window.matchMedia;

  function forceColors(active: boolean) {
    const listeners = new Set<(event: MediaQueryListEvent) => void>();
    window.matchMedia = ((queryText: string) =>
      ({
        addEventListener: (_: string, listener: (event: MediaQueryListEvent) => void) =>
          listeners.add(listener),
        addListener: (listener: (event: MediaQueryListEvent) => void) =>
          listeners.add(listener),
        dispatchEvent: () => false,
        matches: queryText === "(forced-colors: active)" ? active : false,
        media: queryText,
        onchange: null,
        removeEventListener: (
          _: string,
          listener: (event: MediaQueryListEvent) => void,
        ) => listeners.delete(listener),
        removeListener: (listener: (event: MediaQueryListEvent) => void) =>
          listeners.delete(listener),
      }) as unknown as MediaQueryList) as typeof window.matchMedia;
    return listeners;
  }

  afterEach(() => {
    window.matchMedia = realMatchMedia;
  });

  it("never mounts a canvas the operating system cannot recolour", () => {
    forceColors(true);
    const { createRuntime, runtimes } = createHarness();
    render(
      <FactoryWebGLScene
        animateTransitions
        createRuntime={createRuntime}
        freshness="live"
        lens="risk"
        model={fixture.model}
        reducedMotion={false}
        layout={fixture.layout}
      />,
    );

    const host = document.querySelector(".factory-plant-renderer");
    expect(host?.getAttribute("data-forced-colors")).toBe("true");
    expect(host?.getAttribute("data-webgl")).toBe("fallback");
    expect(runtimes).toHaveLength(0);
    expect(document.querySelector("canvas.factory-plant-webgl")).toBeNull();
  });

  it("hands the operator a usable authored fallback with no bitmap", () => {
    forceColors(true);
    const { createRuntime } = createHarness();
    render(
      <FactoryWebGLScene
        animateTransitions
        createRuntime={createRuntime}
        freshness="live"
        lens="world"
        model={fixture.model}
        reducedMotion={false}
        layout={fixture.layout}
      />,
    );

    expect(document.querySelector(".factory-plant-backdrop-authored")).toBeTruthy();
    // The illustration is author colour too, so forced colours never fetch it.
    expect(document.querySelector("img.factory-plant-backdrop")).toBeNull();
  });

  it("builds the runtime once forced colours are turned off", async () => {
    const listeners = forceColors(true);
    const { createRuntime, runtimes } = createHarness();
    const view = render(
      <FactoryWebGLScene
        animateTransitions
        createRuntime={createRuntime}
        freshness="live"
        lens="world"
        model={fixture.model}
        reducedMotion={false}
        layout={fixture.layout}
      />,
    );
    expect(runtimes).toHaveLength(0);

    await act(async () => {
      for (const listener of listeners) {
        listener({ matches: false } as MediaQueryListEvent);
      }
      await new Promise((resolve) => setTimeout(resolve, 0));
    });

    expect(runtimes).toHaveLength(1);
    expect(document.querySelector("canvas.factory-plant-webgl")).toBeTruthy();

    view.rerender(
      <FactoryWebGLScene
        animateTransitions
        createRuntime={createRuntime}
        freshness="live"
        lens="risk"
        model={fixture.model}
        reducedMotion={false}
        layout={fixture.layout}
      />,
    );

    expect(runtimes[0].updates).toHaveLength(2);
    expect(runtimes[0].updates[1].lens).toBe("risk");
  });
});
