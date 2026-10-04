import type { SourceView } from "../api/types";
import { MetadataNodeInput } from "./MetadataRelationshipInput";
import type { EditableAlias } from "./workbenchMetadataEditing";

export function MetadataAliasInput({ gaggle, sources, alias, change }: { gaggle: string; sources: SourceView[]; alias: EditableAlias; change: (value: EditableAlias) => void }) {
  return <>
    <label>Alias name<input required maxLength={64} pattern="[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?" value={alias.name} onChange={(event) => change({ ...alias, name: event.target.value })} /></label>
    <MetadataNodeInput label="Alias target" gaggle={gaggle} sources={sources.filter((source) => source.kind !== "relationships")} value={alias.target} change={(target) => change({ ...alias, target })} />
    <p>An alias names an existing source identity. It does not create another objective or grant access to that target.</p>
  </>;
}
