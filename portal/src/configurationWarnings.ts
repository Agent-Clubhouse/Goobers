import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { QueryState } from "./api/queryState";
import type {
  Instance,
  RequestOptions,
  ValidationWarning,
  WorkflowDetail,
} from "./api/types";
import { useLiveData } from "./liveData";

export type ConfigurationWarningSource =
  | { kind: "none" }
  | { kind: "instance" }
  | { kind: "workflow"; gaggle: string; workflow: string };

export interface ConfigurationWarningClient {
  getInstance(options?: RequestOptions): Promise<Pick<Instance, "warnings">>;
  getWorkflow(
    gaggle: string,
    workflow: string,
    options?: RequestOptions,
  ): Promise<Pick<WorkflowDetail, "warnings">>;
}

export function configurationWarningKey(warning: ValidationWarning): string {
  return JSON.stringify([
    warning.scope,
    warning.code,
    warning.severity,
    warning.explanation,
  ]);
}

export function sortConfigurationWarnings(
  warnings: readonly ValidationWarning[],
): ValidationWarning[] {
  return [...warnings].sort(
    (left, right) =>
      compare(left.scope, right.scope) ||
      compare(left.code, right.code) ||
      compare(left.explanation, right.explanation),
  );
}

export function useConfigurationWarnings(
  client: ConfigurationWarningClient | undefined,
  source: ConfigurationWarningSource,
  fixtureWarnings: readonly ValidationWarning[],
) {
  const sourceKey = warningSourceKey(source);
  const sourceGaggle = source.kind === "workflow" ? source.gaggle : "";
  const sourceWorkflow = source.kind === "workflow" ? source.workflow : "";
  const request = useRef<AbortController | undefined>(undefined);
  const { subscribe } = useLiveData();
  const [dismissedWarningKeys, setDismissedWarningKeys] = useState<ReadonlySet<string>>(
    () => new Set(),
  );
  const [query, setQuery] = useState<{
    sourceKey: string;
    state: QueryState<readonly ValidationWarning[]>;
  }>(() => ({
    sourceKey,
    state: client ? { status: "loading" } : warningState(fixtureWarnings),
  }));

  const refreshWarnings = useCallback(async (): Promise<boolean> => {
    request.current?.abort();
    request.current = undefined;
    if (source.kind === "none") {
      setQuery({ sourceKey, state: { status: "empty" } });
      return true;
    }
    if (!client) {
      setQuery({ sourceKey, state: warningState(fixtureWarnings) });
      return true;
    }

    const controller = new AbortController();
    request.current = controller;
    setQuery((current) => ({
      sourceKey,
      state:
        current.sourceKey === sourceKey &&
        (current.state.status === "ready" || current.state.status === "stale")
          ? { status: "stale", data: current.state.data }
          : { status: "loading" },
    }));

    try {
      const warnings = await readWarnings(client, source, { signal: controller.signal });
      if (controller.signal.aborted) {
        return true;
      }
      setQuery({ sourceKey, state: warningState(warnings) });
      return true;
    } catch (cause: unknown) {
      if (controller.signal.aborted) {
        return true;
      }
      const error =
        cause instanceof Error ? cause : new Error("Unable to read configuration warnings.");
      setQuery((current) => ({
        sourceKey,
        state:
          current.sourceKey === sourceKey && current.state.status === "stale"
            ? { status: "stale", data: current.state.data, error }
            : { status: "error", error },
      }));
      return false;
    } finally {
      if (request.current === controller) {
        request.current = undefined;
      }
    }
  }, [
    client,
    fixtureWarnings,
    sourceGaggle,
    source.kind,
    sourceWorkflow,
    sourceKey,
  ]);

  useEffect(() => {
    if (source.kind === "none") {
      void refreshWarnings();
      return;
    }
    const unsubscribe = subscribe(
      [source.kind === "instance" ? "instance" : "workflow"],
      refreshWarnings,
    );
    return () => {
      unsubscribe();
      request.current?.abort();
      request.current = undefined;
    };
  }, [refreshWarnings, source.kind, subscribe]);

  const dismiss = useCallback((warning: ValidationWarning) => {
    setDismissedWarningKeys((current) => {
      const next = new Set(current);
      next.add(configurationWarningKey(warning));
      return next;
    });
  }, []);
  const refresh = useCallback(() => {
    setDismissedWarningKeys(new Set());
    void refreshWarnings();
  }, [refreshWarnings]);

  return useMemo(
    () => ({
      dismissedWarningKeys,
      onDismiss: dismiss,
      onRefresh: refresh,
      state: query.sourceKey === sourceKey ? query.state : ({ status: "loading" } as const),
    }),
    [dismiss, dismissedWarningKeys, query, refresh, sourceKey],
  );
}

async function readWarnings(
  client: ConfigurationWarningClient,
  source: Exclude<ConfigurationWarningSource, { kind: "none" }>,
  options: RequestOptions,
): Promise<readonly ValidationWarning[]> {
  if (source.kind === "instance") {
    return (await client.getInstance(options)).warnings;
  }
  return (await client.getWorkflow(source.gaggle, source.workflow, options)).warnings;
}

function warningState(
  warnings: readonly ValidationWarning[],
): QueryState<readonly ValidationWarning[]> {
  return warnings.length === 0
    ? { status: "empty" }
    : { status: "ready", data: sortConfigurationWarnings(warnings) };
}

function warningSourceKey(source: ConfigurationWarningSource): string {
  return source.kind === "workflow"
    ? `workflow:${source.gaggle}/${source.workflow}`
    : source.kind;
}

function compare(left: string, right: string): number {
  return left < right ? -1 : left > right ? 1 : 0;
}
