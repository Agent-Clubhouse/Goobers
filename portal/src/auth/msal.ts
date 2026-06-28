import { PublicClientApplication, type Configuration } from "@azure/msal-browser";

const clientId = import.meta.env.VITE_ENTRA_CLIENT_ID as string | undefined;
const tenantId = import.meta.env.VITE_ENTRA_TENANT_ID as string | undefined;
const redirectUri = (import.meta.env.VITE_ENTRA_REDIRECT_URI as string | undefined) ?? window.location.origin;

export const isEntraConfigured = Boolean(clientId && tenantId);

export const msalConfig: Configuration = {
  auth: {
    clientId: clientId ?? "00000000-0000-0000-0000-000000000000",
    authority: tenantId ? `https://login.microsoftonline.com/${tenantId}` : "https://login.microsoftonline.com/common",
    redirectUri,
  },
  cache: {
    cacheLocation: "sessionStorage",
    storeAuthStateInCookie: false,
  },
};

export const loginRequest = {
  scopes: ["openid", "profile", "email"],
};

export const msalInstance = new PublicClientApplication(msalConfig);
