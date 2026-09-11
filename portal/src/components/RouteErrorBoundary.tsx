import { Component, type ErrorInfo, type ReactNode } from "react";

interface Props {
  children: ReactNode;
}

interface State {
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
  }

  render() {
    if (this.state.error) {
      return (
        <section className="daemon-state daemon-state-error" role="alert">
          <div>
            <h1>This page hit an error</h1>
            <p>Something went wrong rendering this page. Reload to try again.</p>
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
