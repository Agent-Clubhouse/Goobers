export interface Authenticator {
  readonly enabled: boolean;
  initialize(): Promise<void>;
  getAccessToken(): Promise<string | undefined>;
  signIn(): Promise<void>;
  signOut(): Promise<void>;
}

export interface AuthConfiguration {
  issuer?: string;
  clientId?: string;
  redirectUri?: string;
  scopes?: readonly string[];
}

export interface ConfiguredAuth {
  issuer: string;
  clientId: string;
  redirectUri?: string;
  scopes: readonly string[];
}

export type AuthenticatorFactory = (config: ConfiguredAuth) => Authenticator;

const defaultScopes = ["openid", "profile", "email"] as const;

export class NullAuthenticator implements Authenticator {
  readonly enabled = false;

  async initialize(): Promise<void> {}

  async getAccessToken(): Promise<undefined> {
    return undefined;
  }

  async signIn(): Promise<void> {}

  async signOut(): Promise<void> {}
}

export function selectAuthenticator(
  config: AuthConfiguration,
  factory: AuthenticatorFactory,
): Authenticator {
  const issuer = configuredValue(config.issuer);
  const clientId = configuredValue(config.clientId);
  const redirectUri = configuredValue(config.redirectUri);
  if (!issuer && !clientId) {
    return new NullAuthenticator();
  }
  if (!issuer || !clientId) {
    throw new Error("Portal auth requires both an issuer and a client ID.");
  }
  return factory({
    issuer,
    clientId,
    ...(redirectUri ? { redirectUri } : {}),
    scopes: config.scopes ?? defaultScopes,
  });
}

function configuredValue(value: string | undefined): string | undefined {
  const trimmed = value?.trim();
  return trimmed || undefined;
}
