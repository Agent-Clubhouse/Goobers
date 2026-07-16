import "@testing-library/jest-dom/vitest";
import { beforeEach } from "vitest";

const values = new Map<string, string>();
const localStorage: Storage = {
  get length() {
    return values.size;
  },
  clear() {
    values.clear();
  },
  getItem(key) {
    return values.get(key) ?? null;
  },
  key(index) {
    return [...values.keys()][index] ?? null;
  },
  removeItem(key) {
    values.delete(key);
  },
  setItem(key, value) {
    values.set(key, value);
  },
};

Object.defineProperty(window, "localStorage", {
  configurable: true,
  value: localStorage,
});

beforeEach(() => localStorage.clear());
