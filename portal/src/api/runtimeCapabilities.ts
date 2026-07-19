import registry from "./runtimeCapabilities.json";
import type { RuntimeMutationCapabilityId } from "./contract.generated";

type UnexpectedRuntimeMutationCapability = Exclude<
  keyof typeof registry,
  RuntimeMutationCapabilityId
>;

const exactRegistry: Record<RuntimeMutationCapabilityId, boolean> &
  Record<UnexpectedRuntimeMutationCapability, never> = registry;

export const uiRuntimeMutationCapabilities = exactRegistry;
