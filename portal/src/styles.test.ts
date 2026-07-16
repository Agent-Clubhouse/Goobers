import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

const styles = readFileSync(resolve(process.cwd(), "src/styles.css"), "utf8");
const tokens = readFileSync(resolve(process.cwd(), "src/foundation/tokens.css"), "utf8");

describe("portal style foundations", () => {
  it("defines independently tuned light and dark semantic tokens", () => {
    expect(tokens).toContain(":root {");
    expect(tokens).toContain(':root[data-theme="dark"]');
    expect(tokens).toContain("--bg: #f4f3ef");
    expect(tokens).toContain("--bg: #111115");
    expect(tokens).toContain("--focus: #7a5de0");
    expect(tokens).toContain("--focus: #b29aff");
  });

  it("provides a 320px-safe responsive baseline for navigation and controls", () => {
    expect(styles).toContain("@media (max-width: 480px)");
    expect(styles).toMatch(/\.sidebar\s*\{[\s\S]*grid-template-columns: 32px minmax\(0, 1fr\)/);
    expect(styles).toMatch(/\.playback-controls input\[type="range"\]\s*\{[\s\S]*min-width: 0/);
    expect(styles).toMatch(/\.ledger-item button\s*\{[\s\S]*minmax\(0, 1fr\)/);
  });

  it("honors reduced motion without hiding state", () => {
    expect(styles).toContain("@media (prefers-reduced-motion: reduce)");
    expect(styles).toContain('data-replay-motion="reduced"');
    expect(styles).not.toContain("active-sweep");
  });
});
