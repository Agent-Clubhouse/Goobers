import {
  selectAuthenticator,
  type AuthConfiguration,
  type AuthenticatorFactory,
} from "./authenticator";
import { createMsalAuthenticator } from "./msal";

export {
  NullAuthenticator,
  selectAuthenticator,
  type AuthConfiguration,
  type Authenticator,
  type AuthenticatorFactory,
  type ConfiguredAuth,
} from "./authenticator";
export { createMsalAuthenticator, MsalAuthenticator, msalConfiguration } from "./msal";

export function createPortalAuthenticator(
  config: AuthConfiguration,
  factory: AuthenticatorFactory = createMsalAuthenticator,
) {
  return selectAuthenticator(config, factory);
}

export const authenticator = createPortalAuthenticator({
  issuer: import.meta.env.VITE_OIDC_ISSUER as string | undefined,
  clientId: import.meta.env.VITE_OIDC_CLIENT_ID as string | undefined,
  redirectUri: import.meta.env.VITE_OIDC_REDIRECT_URI as string | undefined,
});
