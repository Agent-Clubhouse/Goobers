export function invoiceTotal(subtotal, discount = 0) {
  if (!Number.isFinite(subtotal) || !Number.isFinite(discount)) {
    throw new TypeError("Amounts must be finite numbers");
  }
  return subtotal - discount;
}
