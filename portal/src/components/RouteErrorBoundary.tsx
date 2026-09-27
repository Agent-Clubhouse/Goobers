import { Component, type ErrorInfo, type ReactNode } from "react";

interface Props {
  children: ReactNode;
}

interface State {
  componentStack?: string;
  error: Error | null;
}

// A single page component throwing during render (e.g. a malformed API
// response) previously unmounted the entire portal — no error boundary
// existed anywhere in the tree (#4825). React error boundaries must be class
// components; there is no hook equivalent. Keyed by route in App.tsx so a
// fresh instance mounts on navigation, clearing any prior crash instead of
// pinning the user on the error card.
export class RouteErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error("goobers: page crashed", error, info.componentStack);
    this.setState({ componentStack: info.componentStack ?? undefined });
  }

  render() {
    if (this.state.error) {
      return (
        <section className="daemon-state daemon-state-error" role="alert">
          <div>
            <h1>This page hit an error</h1>
            <p>Something went wrong rendering this page. Reload to try again.</p>
            <details>
              <summary>Error details</summary>
              <pre>{[
                `${this.state.error.name}: ${this.state.error.message}`,
                this.state.componentStack,
              ].filter(Boolean).join("\n")}</pre>
            </details>
          </div>
          <button
            className="reconnect-button"
            onClick={() => window.location.reload()}
            type="button"
          >
            Reload
          </button>
        </section>
      );
    }
    return this.props.children;
  }
}
