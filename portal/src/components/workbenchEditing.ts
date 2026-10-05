import type { BacklogEditCommand, BacklogItem, BacklogPatchInput, BacklogWriteField } from "../api/types";

export const editFields: BacklogWriteField[] = ["title", "description", "state", "labels", "assignees"];
export function controlLabel(value: string): boolean { return /^goobers(?::|\/status:)/i.test(value.trim()); }
export function fieldText(item: BacklogItem, field: BacklogWriteField): string {
  if (field === "labels") return (item.labels ?? []).filter((value) => !controlLabel(value)).join("\n");
  if (field === "assignees") return (item.assignees ?? []).join("\n");
  return item[field] ?? "";
}
export function editInput(item: BacklogItem, field: BacklogWriteField, text: string): BacklogPatchInput {
  const base = { sourceId: item.ref.sourceId, expectedRevision: item.revision ?? "", field };
  if (field === "labels" || field === "assignees") {
    const values = text.split("\n").map((value) => value.trim()).filter(Boolean);
    if (field === "labels") values.push(...(item.labels ?? []).filter(controlLabel));
    return { ...base, values };
  }
  return { ...base, value: text };
}
export function sameItem(value: BacklogItem, item: BacklogItem): boolean {
  return value.ref?.gaggleId === item.ref.gaggleId && value.ref?.sourceBindingId === item.ref.sourceBindingId && value.ref?.sourceId === item.ref.sourceId && value.ref?.kind === "work-item" && value.locator?.id === item.locator.id;
}
export function sameCommand(value: BacklogEditCommand, item: BacklogItem, input: BacklogPatchInput): boolean {
  return /^workbench-[0-9a-f]{32}$/.test(value.id) && value.gaggle === item.ref.gaggleId && value.sourceBindingId === item.ref.sourceBindingId && value.sourceId === item.ref.sourceId && value.itemId === item.locator.id && value.field === input.field && ["accepted", "attempting", "confirmed", "not-applied", "unknown"].includes(value.state) && !!value.actor?.issuer && !!value.actor?.subject;
}
