import test from "node:test";
import assert from "node:assert/strict";
import { invoiceTotal } from "./invoice.mjs";

test("ordinary invoice retains its public API", () => {
  assert.equal(invoiceTotal(100, 20), 80);
  assert.equal(invoiceTotal(100), 100);
});
test("a discount cannot produce a negative invoice", () => {
  assert.equal(invoiceTotal(20, 30), 0);
});
test("non-finite values are rejected", () => {
  assert.throws(() => invoiceTotal(Infinity), TypeError);
});
