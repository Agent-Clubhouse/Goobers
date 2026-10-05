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
    const rules = [
      styles.match(/\.work-item-grid\.data-row\s*\{[^}]*\}/)?.[0],
      styles.match(/\.work-item-grid\.work-item-row-done\s*\{[^}]*\}/)?.[0],
      styles.match(/\.work-item-grid\.work-item-row-bad-terminal\s*\{[^}]*\}/)?.[0],
    ];
    expect(rules).not.toContain(undefined);

    const style = document.createElement("style");
    style.textContent = rules
      .join("\n")
      .replace("var(--success)", "rgb(1, 2, 3)")
      .replace("var(--danger)", "rgb(4, 5, 6)");
    document.head.append(style);

    const rows = ["", "work-item-row-done", "work-item-row-bad-terminal"].map((status) => {
      const row = document.createElement("button");
      row.className = `work-item-grid data-row ${status}`;
      document.body.append(row);
      return row;
    });

    try {
      expect(getComputedStyle(rows[0]).borderLeftColor).toBe("rgba(0, 0, 0, 0)");
      expect(getComputedStyle(rows[1]).borderLeftColor).toBe("rgb(1, 2, 3)");
      expect(getComputedStyle(rows[2]).borderLeftColor).toBe("rgb(4, 5, 6)");
    } finally {
      rows.forEach((row) => row.remove());
      style.remove();
    }
  });
});
