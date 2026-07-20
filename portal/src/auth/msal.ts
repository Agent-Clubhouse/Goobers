import {
  PublicClientApplication,
  type Configuration,
  type RedirectRequest,
  type SilentRequest,
} from "@azure/msal-browser";
import type { Authenticator, ConfiguredAuth } from "./authenticator";

export class MsalAuthenticator implements Authenticator {
  readonly enabled = true;
  private readonly instance: PublicClientApplication;
  private readonly scopes: string[];
  private initialization?: Promise<void>;

  constructor(config: ConfiguredAuth) {
    this.instance = new PublicClientApplication(msalConfiguration(config));
    this.scopes = [...config.scopes];
  }

  initialize(): Promise<void> {
    this.initialization ??= this.initializeInstance();
    return this.initialization;
  }

  async getAccessToken(): Promise<string | undefined> {
    await this.initialize();
    const account = this.activeAccount();
    if (!account) {
      return undefined;
    }
    const request: SilentRequest = { account, scopes: this.scopes };
    return (await this.instance.acquireTokenSilent(request)).accessToken;
  }

  async signIn(): Promise<void> {
    await this.initialize();
    const request: RedirectRequest = { scopes: this.scopes };
    await this.instance.loginRedirect(request);
  }

  async signOut(): Promise<void> {
    await this.initialize();
    const account = this.activeAccount();
    await this.instance.logoutRedirect(account ? { account } : undefined);
  }

  private async initializeInstance(): Promise<void> {
    await this.instance.initialize();
    const result = await this.instance.handleRedirectPromise();
    if (result?.account) {
      this.instance.setActiveAccount(result.account);
    } else {
      this.activeAccount();
    }
  }

  private activeAccount() {
    const account = this.instance.getActiveAccount() ?? this.instance.getAllAccounts()[0] ?? null;
    if (account && !this.instance.getActiveAccount()) {
      this.instance.setActiveAccount(account);
    }
    return account;
  }
}

export function createMsalAuthenticator(config: ConfiguredAuth): Authenticator {
  return new MsalAuthenticator(config);
}

export function msalConfiguration(config: ConfiguredAuth): Configuration {
  return {
    auth: {
      clientId: config.clientId,
      authority: config.issuer,
      redirectUri: config.redirectUri ?? window.location.origin,
    },
    cache: {
      cacheLocation: "sessionStorage",
      storeAuthStateInCookie: false,
    },
  };
}
