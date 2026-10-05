import { describe, expect, it } from "vitest";
import styles from "./styles.css?inline";

describe("compact shell styles", () => {
  it("applies asymmetric safe-area insets to the matching topbar edges", () => {
    const padding = "padding: 0 max(8px, env(safe-area-inset-right, 0px)) 0 max(8px, env(safe-area-inset-left, 0px));";
    const reversedPadding = "padding: 0 max(8px, env(safe-area-inset-left, 0px)) 0 max(8px, env(safe-area-inset-right, 0px));";

    expect(styles.split(padding)).toHaveLength(3);
    expect(styles).not.toContain(reversedPadding);
  });

  describe("cost chart styles", () => {
    it("colors the total bars and gives both legend series a visible key", () => {
      expect(styles).toMatch(/\.usage-trend-bar\s*\{[^}]*fill:\s*var\(--accent\)/);
      expect(styles).toMatch(/\.usage-trend-key-total\s*\{[^}]*background:\s*var\(--accent\)/);
      expect(styles).toMatch(/\.usage-trend-key-p95\s*\{[^}]*border-top:\s*2px solid var\(--accent-ink\)/);
      expect(styles).toMatch(/\.usage-trend-gridline \.usage-trend-secondary-tick\s*\{[^}]*text-anchor:\s*start/);
    });
  });

  it("reserves the work-item status gutter for every row", () => {
    expect(styles).toMatch(
      /\.work-item-grid\.data-row\s*\{[^}]*border-left:\s*3px solid transparent/,
    );
    expect(styles).toMatch(
      /\.work-item-row-done\s*\{[^}]*border-left:\s*3px solid var\(--success\)/,
    );
    expect(styles).toMatch(
      /\.work-item-row-bad-terminal\s*\{[^}]*border-left:\s*3px solid var\(--danger\)/,
    );
  });
});
