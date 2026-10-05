import { describe, expect, it } from "vitest";
import { formatAIC } from "./aicFormat";

describe("formatAIC", () => {
  it.each([
    [12.49, "12 AIC"],
    [12.5, "13 AIC"],
    [0, "0 AIC"],
    [1_234_567.6, "1,234,568 AIC"],
  ])("formats %s as nearest-whole AIC", (value, expected) => {
    expect(formatAIC(value)).toBe(expected);
  });
});
