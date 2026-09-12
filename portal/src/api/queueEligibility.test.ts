import { describe, expect, it } from "vitest";
import { MalformedResponseError } from "./errors";
import { assertQueueEligibility } from "./queueEligibility";
import { goWireFixtures } from "./wire.generated";

describe("queue evidence rendering boundary", () => {
  it("accepts the canonical Go wire fixture", () => {
    const value = goWireFixtures.queueEligibility;
    expect(() => assertQueueEligibility(value, value.gaggle, value.workflow)).not.toThrow();
  });

  it.each([
    ["null", () => null],
    ["foreign workflow", (v) => ({ ...v, workflow: "foreign" })],
    ["unknown status", (v) => ({ ...v, status: "claimable" })],
    ["unavailable with report", (v) => ({ ...v, status: "unavailable" })],
    ["different run", (v) => ({ ...v, sourceRunId: "other-run" })],
    ["bad date", (v) => ({ ...v, asOf: "not-a-date" })],
    ["malformed freshness", (v) => ({ ...v, readState: { completeness: "complete", lagSeconds: 0, degraded: {} } })],
    ["invalid lag", (v) => ({ ...v, readState: { completeness: "complete", lagSeconds: -1, degraded: [] } })],
    ["negative omissions", (v) => ({ ...v, report: { ...v.report, omittedItems: -1 } })],
    ["wrong count", (v) => ({ ...v, report: { ...v.report, matchingItems: 500 } })],
    ["missing claim", (v) => ({ ...v, report: { ...v.report, items: [{ ...v.report!.items[0], claim: null }] } })],
    ["missing policy boolean", (v) => ({ ...v, report: { ...v.report, items: [{ ...v.report!.items[0], eligible: undefined }] } })],
    ["duplicate PR", (v) => ({ ...v, report: { ...v.report, matchingItems: 2, omittedItems: 0, items: [v.report!.items[0], v.report!.items[0]] } })],
    ["oversized report", (v) => ({ ...v, report: { ...v.report, matchingItems: 1001, omittedItems: 0, items: Array.from({ length: 1001 }, (_, i) => ({ ...v.report!.items[0], number: i + 1 })) } })],
  ] satisfies Array<[string, (value: typeof goWireFixtures.queueEligibility) => unknown]>)("rejects %s", (_, corrupt) => {
    const value = structuredClone(goWireFixtures.queueEligibility);
    expect(() => assertQueueEligibility(corrupt(value), value.gaggle, value.workflow)).toThrow(MalformedResponseError);
  });
});
