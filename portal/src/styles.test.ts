import { describe, expect, it } from "vitest";
import styles from "./styles.css?inline";

describe("compact shell styles", () => {
  it("applies asymmetric safe-area insets to the matching topbar edges", () => {
    const padding = "padding: 0 max(8px, env(safe-area-inset-right, 0px)) 0 max(8px, env(safe-area-inset-left, 0px));";
    const reversedPadding = "padding: 0 max(8px, env(safe-area-inset-left, 0px)) 0 max(8px, env(safe-area-inset-right, 0px));";

    expect(styles.split(padding)).toHaveLength(3);
    expect(styles).not.toContain(reversedPadding);
  });
});
