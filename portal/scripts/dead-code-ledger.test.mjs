import { describe, expect, it } from "vitest";
import { findingsFromKnipReport, reviewFindings } from "./dead-code-ledger.mjs";

const exemption = {
  type: "files",
  file: "src/test/fixture.ts",
  symbol: "src/test/fixture.ts",
  reason: "Test-only fixture.",
};

describe("dead-code exemption ledger", () => {
  it("converts top-level unused files and per-file unused exports", () => {
    expect(
      findingsFromKnipReport({
        files: ["src/orphan.ts"],
        issues: [
          {
            file: "src/nested-orphan.ts",
            files: [{ name: "src/nested-orphan.ts" }],
          },
          {
            file: "src/imported.ts",
            exports: [{ name: "unusedExport" }],
            nsExports: [{ name: "unusedNamespaceExport" }],
          },
        ],
      }),
    ).toEqual([
      { type: "files", file: "src/orphan.ts", symbol: "src/orphan.ts" },
      {
        type: "files",
        file: "src/nested-orphan.ts",
        symbol: "src/nested-orphan.ts",
      },
      { type: "exports", file: "src/imported.ts", symbol: "unusedExport" },
      { type: "nsExports", file: "src/imported.ts", symbol: "unusedNamespaceExport" },
    ]);
  });

  it("accepts an exact reviewed finding", () => {
    expect(reviewFindings([exemption], [exemption])).toEqual({ unexpected: [], stale: [] });
  });

  it("rejects unreviewed findings and stale exemptions", () => {
    const unreviewed = { ...exemption, file: "src/orphan.ts", symbol: "src/orphan.ts" };
    expect(reviewFindings([unreviewed], [exemption])).toEqual({
      unexpected: [unreviewed],
      stale: [exemption],
    });
  });

  it("rejects duplicate and unreasoned exemptions", () => {
    expect(() => reviewFindings([], [exemption, exemption])).toThrow(
      "duplicate dead-code exemption",
    );
    expect(() => reviewFindings([], [{ ...exemption, reason: "" }])).toThrow(
      "dead-code exemption has no reason",
    );
  });
});
