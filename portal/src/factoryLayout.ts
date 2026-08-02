/**
 * Factory Floor has two layouts over one model.
 *
 * `lines` is the precise workflow topology: declared stages in graph order with
 * their edges drawn as belts. `plant` is the isometric plant overview: the same
 * stages as machinery inside workflow districts of one hall.
 *
 * Layout is presentation only. It never changes which daemon reads happen, what
 * the model contains, which entities exist, or what an entity is called.
 */
export type FactoryLayout = "lines" | "plant";

export const FACTORY_LAYOUTS: readonly FactoryLayout[] = ["lines", "plant"];

export const DEFAULT_FACTORY_LAYOUT: FactoryLayout = "lines";

export function isFactoryLayout(value: string | undefined): value is FactoryLayout {
  return value === "lines" || value === "plant";
}

export function factoryLayoutLabel(layout: FactoryLayout): string {
  return layout === "plant" ? "Plant" : "Lines";
}

export function factoryLayoutDescription(layout: FactoryLayout): string {
  return layout === "plant"
    ? "Isometric plant overview: workflow districts, machinery, and belts in one hall."
    : "Line topology: declared stages in graph order with their edges.";
}
