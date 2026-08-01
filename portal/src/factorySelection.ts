export type FactorySelection =
  | { kind: "overview" }
  | { kind: "gaggle"; name: string }
  | { kind: "lane"; id: string }
  | { kind: "station"; id: string }
  | { kind: "run"; id: string }
  | { kind: "worker"; id: string };

export const overviewSelection: FactorySelection = { kind: "overview" };

export function isSelected(
  current: FactorySelection,
  candidate: FactorySelection,
): boolean {
  if (current.kind !== candidate.kind) {
    return false;
  }
  if (current.kind === "overview" || candidate.kind === "overview") {
    return true;
  }
  if (current.kind === "gaggle" && candidate.kind === "gaggle") {
    return current.name === candidate.name;
  }
  return "id" in current && "id" in candidate && current.id === candidate.id;
}
