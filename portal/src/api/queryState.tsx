import type { ReactNode } from "react";

export type QueryState<T, E = Error> =
  | { status: "loading" }
  | { status: "empty" }
  | { status: "error"; error: E }
  | { status: "stale"; data: T; error?: E }
  | { status: "ready"; data: T };

export interface QueryStateBoundaryProps<T, E = Error> {
  state: QueryState<T, E>;
  loading: ReactNode;
  empty: ReactNode;
  error: (error: E) => ReactNode;
  stale: (data: T, error?: E) => ReactNode;
  children: (data: T) => ReactNode;
}

export function QueryStateBoundary<T, E = Error>({
  state,
  loading,
  empty,
  error,
  stale,
  children,
}: QueryStateBoundaryProps<T, E>) {
  switch (state.status) {
    case "loading":
      return loading;
    case "empty":
      return empty;
    case "error":
      return error(state.error);
    case "stale":
      return stale(state.data, state.error);
    case "ready":
      return children(state.data);
  }
}
