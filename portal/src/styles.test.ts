import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

const styles = readFileSync(resolve(process.cwd(), "src/styles.css"), "utf8");
const tokens = readFileSync(resolve(process.cwd(), "src/foundation/tokens.css"), "utf8");

function token(name: string) {
  const match = tokens.match(new RegExp(`--${name}:\\s*(#[0-9a-f]{6})`, "i"));
  if (!match?.[1]) {
    throw new Error(`missing ${name} token`);
  }
  return match[1];
}

function luminance(hex: string) {
  const channels = hex.slice(1).match(/.{2}/g);
  if (!channels || channels.length !== 3) {
    throw new Error(`invalid color ${hex}`);
  }
  const [red, green, blue] = channels.map((channel) => {
    const value = Number.parseInt(channel, 16) / 255;
    return value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * red + 0.7152 * green + 0.0722 * blue;
}

function contrastRatio(foreground: string, background: string) {
  const lighter = Math.max(luminance(foreground), luminance(background));
  const darker = Math.min(luminance(foreground), luminance(background));
  return (lighter + 0.05) / (darker + 0.05);
}

describe("portal style foundations", () => {
  it("defines independently tuned light and dark semantic tokens", () => {
    expect(tokens).toContain(":root {");
    expect(tokens).toContain(':root[data-theme="dark"]');
    expect(tokens).toContain("--bg: #f4f3ef");
    expect(tokens).toContain("--bg: #111115");
    expect(tokens).toContain("--focus: #7a5de0");
    expect(tokens).toContain("--focus: #b29aff");
  });

  it("keeps light warning text above the WCAG AA contrast threshold", () => {
    expect(contrastRatio(token("warning"), token("warning-soft"))).toBeGreaterThanOrEqual(4.5);
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
