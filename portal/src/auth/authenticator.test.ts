import { describe, expect, it, vi } from "vitest";
import {
  NullAuthenticator,
  selectAuthenticator,
  type Authenticator,
  type ConfiguredAuth,
} from "./authenticator";
import { msalConfiguration } from "./msal";

describe("portal authenticator seam", () => {
  it("uses no authentication when no issuer is configured", async () => {
    const factory = vi.fn<(config: ConfiguredAuth) => Authenticator>();
    const authenticator = selectAuthenticator({}, factory);

    expect(authenticator).toBeInstanceOf(NullAuthenticator);
    expect(authenticator.enabled).toBe(false);
    await expect(authenticator.initialize()).resolves.toBeUndefined();
    await expect(authenticator.getAccessToken()).resolves.toBeUndefined();
    await expect(authenticator.signIn()).resolves.toBeUndefined();
    await expect(authenticator.signOut()).resolves.toBeUndefined();
    expect(factory).not.toHaveBeenCalled();
  });

  it("accepts a pluggable authenticator factory", () => {
    const fake: Authenticator = {
      enabled: true,
      initialize: async () => undefined,
      getAccessToken: async () => "token",
      signIn: async () => undefined,
      signOut: async () => undefined,
    };
    const factory = vi.fn(() => fake);

    expect(
      selectAuthenticator(
        {
          issuer: "https://issuer.example",
          clientId: "portal-client",
          redirectUri: "https://portal.example/callback",
        },
        factory,
      ),
    ).toBe(fake);
    expect(factory).toHaveBeenCalledWith({
      issuer: "https://issuer.example",
      clientId: "portal-client",
      redirectUri: "https://portal.example/callback",
      scopes: ["openid", "profile", "email"],
    });
  });

  it("rejects partial issuer configuration", () => {
    const factory = vi.fn<(config: ConfiguredAuth) => Authenticator>();

    expect(() => selectAuthenticator({ issuer: "https://issuer.example" }, factory)).toThrow(
      "Portal auth requires both an issuer and a client ID.",
    );
    expect(() => selectAuthenticator({ clientId: "portal-client" }, factory)).toThrow(
      "Portal auth requires both an issuer and a client ID.",
    );
  });

  it("passes the configured issuer to MSAL without an Entra default", () => {
    expect(
      msalConfiguration({
        issuer: "https://issuer.example/tenant",
        clientId: "portal-client",
        redirectUri: "https://portal.example/callback",
        scopes: ["openid"],
      }).auth,
    ).toMatchObject({
      authority: "https://issuer.example/tenant",
      clientId: "portal-client",
      redirectUri: "https://portal.example/callback",
    });
  });
});
