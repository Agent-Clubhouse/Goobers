import { useIsAuthenticated, useMsal } from "@azure/msal-react";
import { isEntraConfigured, loginRequest } from "../auth/msal";

interface AuthGateProps {
  children: React.ReactNode;
}

export function AuthGate({ children }: AuthGateProps) {
  const { instance } = useMsal();
  const isAuthenticated = useIsAuthenticated();

  if (!isEntraConfigured) {
    return (
      <>
        <div className="auth-banner" role="status">
          Entra ID auth scaffold is in local-dev mode. Set VITE_ENTRA_CLIENT_ID and VITE_ENTRA_TENANT_ID to enforce SSO.
        </div>
        {children}
      </>
    );
  }

  if (!isAuthenticated) {
    return (
      <main className="auth-card">
        <p className="eyebrow">Microsoft Entra ID</p>
        <h1>Sign in to Goobers Portal</h1>
        <p>Portal routes are guarded by MSAL. Sign in to view workforce telemetry and runtime gates.</p>
        <button type="button" onClick={() => void instance.loginRedirect(loginRequest)}>
          Sign in with Microsoft
        </button>
      </main>
    );
  }

  return <>{children}</>;
}
