import { describe, expect, it } from "vitest";
import {
  configAuthoringErrorCodes,
  configAuthoringRoutes,
} from "./contract.generated";
import type {
  ConfigDocumentChange,
  ConfigSourceCapabilities,
} from "./types";

describe("configuration authoring contract", () => {
  it("publishes stable source-neutral routes", () => {
    expect(configAuthoringRoutes).toEqual({
      configSources: {
        method: "GET",
        path: "/api/v1/config/sources",
        actionClass: "read-only-navigation",
      },
      configSourceDocuments: {
        method: "GET",
        path: "/api/v1/config/sources/{source}/documents",
        actionClass: "read-only-navigation",
      },
      configSourceDocument: {
        method: "GET",
        path: "/api/v1/config/sources/{source}/document",
        actionClass: "read-only-navigation",
      },
      configSourcePreview: {
        method: "POST",
        path: "/api/v1/config/sources/{source}/preview",
        actionClass: "config-time",
      },
      configSourceChanges: {
        method: "PUT",
        path: "/api/v1/config/sources/{source}/changes",
        actionClass: "config-time",
      },
    });
  });

  it("keeps authoring error codes generated from Go", () => {
    expect(configAuthoringErrorCodes).toEqual([
      "config_source_not_found",
      "config_document_not_found",
      "config_stale_revision",
      "config_unsupported_capability",
      "config_validation_failed",
      "config_authorization_failed",
      "config_projection_lag",
    ]);
  });

  it("represents direct and review write capabilities independently", () => {
    const direct: ConfigSourceCapabilities = {
      read: true,
      validate: true,
      directWrite: true,
      reviewWrite: false,
    };
    const review: ConfigSourceCapabilities = {
      ...direct,
      directWrite: false,
      reviewWrite: true,
    };

    expect(direct.directWrite).toBe(true);
    expect(review.reviewWrite).toBe(true);
  });

  it("uses a discriminated change union", () => {
    const emptyUpsert: ConfigDocumentChange = {
      path: "gaggles/core/gaggle.yaml",
      operation: "upsert",
      content: "",
    };
    const deletion: ConfigDocumentChange = {
      path: "gaggles/core/goobers/old.yaml",
      operation: "delete",
      baseEtag: "sha256:old",
    };

    expect(emptyUpsert.content).toBe("");
    expect(deletion.operation).toBe("delete");
  });
});
