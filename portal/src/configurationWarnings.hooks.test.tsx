import { act, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { RequestOptions, ValidationWarning } from "./api/types";
import { ConfigurationWarnings } from "./components/ConfigurationWarnings";
import {
  type ConfigurationWarningClient,
  type ConfigurationWarningSource,
  useConfigurationWarnings,
} from "./configurationWarnings";

type UpdateModel = "instance" | "run" | "workflow";
type LiveListener = (
  models: ReadonlySet<UpdateModel>,
  reason: "initial" | "refresh",
) => boolean | void | Promise<boolean | void>;

const liveData = vi.hoisted(() => {
  const listeners = new Set<LiveListener>();
  const subscribe = vi.fn(
    (
      models: readonly UpdateModel[],
      listener: LiveListener,
    ) => {
      listeners.add(listener);
      void listener(new Set(models), "initial");
      return () => listeners.delete(listener);
    },
  );
  return { listeners, subscribe };
});

vi.mock("./liveData", () => ({
  useLiveData: () => ({ subscribe: liveData.subscribe }),
}));

const warning: ValidationWarning = {
  code: "VER003",
  severity: "warning",
  scope: "gaggles/core/workflows/implementation.yaml Workflow/implementation",
  explanation: "expected output is not emitted by the configured result file",
};
const noWarnings: readonly ValidationWarning[] = [];

interface PendingRead {
  resolve: (value: { warnings: ValidationWarning[] }) => void;
  signal: AbortSignal | undefined;
}

function gatedWarningClient(): {
  client: ConfigurationWarningClient;
  pending: PendingRead[];
} {
  const pending: PendingRead[] = [];
  const read = (options?: RequestOptions) =>
    new Promise<{ warnings: ValidationWarning[] }>((resolve) => {
      pending.push({ resolve, signal: options?.signal });
    });
  return {
    client: {
      getInstance: vi.fn(read),
      getWorkflow: vi.fn((_gaggle, _workflow, options) => read(options)),
    },
    pending,
  };
}

function WarningHarness({
  client,
  source,
}: {
  client: ConfigurationWarningClient;
  source: ConfigurationWarningSource;
}) {
  const query = useConfigurationWarnings(client, source, noWarnings);
  return (
    <ConfigurationWarnings
      context={source.kind === "workflow" ? "workflow" : "instance"}
      {...query}
    />
  );
}

describe("useConfigurationWarnings live invalidation", () => {
  beforeEach(() => {
    liveData.listeners.clear();
    liveData.subscribe.mockClear();
  });

  it.each([
    {
      eventModel: "instance" as const,
      source: { kind: "instance" } as const,
    },
    {
      eventModel: "workflow" as const,
      source: {
        kind: "workflow",
        gaggle: "core",
        workflow: "implementation",
      } as const,
    },
  ])(
    "lets an in-flight $eventModel warning read populate during an event burst",
    async ({ eventModel, source }) => {
      const { client, pending } = gatedWarningClient();
      render(<WarningHarness client={client} source={source} />);

      await waitFor(() => expect(pending).toHaveLength(1));
      const initial = pending[0];

      act(() => {
        for (let event = 0; event < 8; event += 1) {
          for (const listener of liveData.listeners) {
            void listener(new Set([eventModel]), "refresh");
          }
        }
      });

      expect(pending).toHaveLength(1);
      expect(initial.signal?.aborted).toBe(false);

      act(() => {
        initial.resolve({ warnings: [warning] });
      });

      expect(await screen.findByText(warning.explanation)).toBeInTheDocument();
      await waitFor(() => expect(pending).toHaveLength(2));
      expect(initial.signal?.aborted).toBe(false);

      act(() => {
        pending[1].resolve({ warnings: [warning] });
      });
      await waitFor(() => expect(screen.getByText(warning.explanation)).toBeInTheDocument());
    },
  );
});
