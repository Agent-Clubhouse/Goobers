import { type ComponentType, type ReactNode, useLayoutEffect, useRef } from "react";
import { GooberStage } from "../site-mascot/src/mascot/react/GooberStage.jsx";

interface GooberActor {
  emote(name: string): Promise<void>;
  hop(height: number): Promise<void>;
  wave(): Promise<void>;
}

interface MascotDirector {
  cast(name: string): Promise<GooberActor>;
}

interface StageReadyDetail {
  camera: {
    position: {
      set(x: number, y: number, z: number): void;
    };
  };
  director: MascotDirector;
}

const SiteGooberStage = GooberStage as ComponentType<{
  className?: string;
  fallback?: ReactNode;
}>;

const wait = (milliseconds: number) =>
  new Promise<void>((resolve) => window.setTimeout(resolve, milliseconds));

export function AnimatedGoober() {
  const containerRef = useRef<HTMLDivElement>(null);

  useLayoutEffect(() => {
    const container = containerRef.current;
    if (!container) {
      return;
    }

    let canceled = false;
    const handleReady = async (event: Event) => {
      const { camera, director } = (event as CustomEvent<StageReadyDetail>).detail;
      camera.position.set(0, 0.58, 3.2);
      const pip = await director.cast("Pip");

      while (!canceled) {
        await wait(900);
        if (canceled) break;
        await pip.wave();
        await wait(900);
        if (canceled) break;
        await pip.hop(0.22);
        await wait(1200);
        if (canceled) break;
        await pip.emote("dance");
      }
    };

    container.addEventListener("stage:ready", handleReady);
    return () => {
      canceled = true;
      container.removeEventListener("stage:ready", handleReady);
    };
  }, []);

  return (
    <div className="guided-mascot-host" ref={containerRef}>
      <SiteGooberStage
        className="guided-mascot-stage"
        fallback={
          <img
            alt=""
            className="guided-mascot-fallback"
            src="/goober-mascot-fallback.webp"
          />
        }
      />
    </div>
  );
}
