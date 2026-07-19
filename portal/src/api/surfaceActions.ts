import rawRegistry from "./surfaceActions.json";
import {
  actionClasses,
  runtimeMutationCapabilities,
  type ActionClass,
  type RuntimeMutationCapabilityId,
  type SurfaceAction,
} from "./contract.generated";

export type UIActionID = keyof typeof rawRegistry;

type RawSurfaceAction = {
  class: string;
  capability?: string;
};

const validActionClasses = new Set<string>(Object.values(actionClasses));
const validRuntimeCapabilities = new Set<string>(runtimeMutationCapabilities);

function isActionClass(value: string): value is ActionClass {
  return validActionClasses.has(value);
}

function isRuntimeMutationCapability(value: string): value is RuntimeMutationCapabilityId {
  return validRuntimeCapabilities.has(value);
}

function isUIActionID(value: string): value is UIActionID {
  return Object.hasOwn(rawRegistry, value);
}

function parseSurfaceAction(id: UIActionID, raw: RawSurfaceAction): SurfaceAction {
  if (!isActionClass(raw.class)) {
    throw new Error(`UI action ${id} has unknown class ${raw.class}`);
  }
  if (raw.class === actionClasses.runtimeMutation) {
    if (!raw.capability || !isRuntimeMutationCapability(raw.capability)) {
      throw new Error(`UI runtime action ${id} has unknown capability ${raw.capability ?? ""}`);
    }
    return {
      id,
      class: actionClasses.runtimeMutation,
      capability: raw.capability,
    };
  }
  if (raw.capability) {
    throw new Error(`Excluded UI action ${id} cannot register capability ${raw.capability}`);
  }
  return { id, class: raw.class };
}

export const uiSurfaceActions = Object.entries(rawRegistry).map(([id, raw]) => {
  if (!isUIActionID(id)) {
    throw new Error(`UI action ${id} is not registered`);
  }
  return parseSurfaceAction(id, raw);
});

type UIActionHandler = (...args: never[]) => unknown;

export function bindUIActions<Handlers extends Record<UIActionID, UIActionHandler>>(
  handlers: Handlers,
): Handlers {
  for (const { id } of uiSurfaceActions) {
    if (!isUIActionID(id) || typeof handlers[id] !== "function") {
      throw new Error(`UI action ${id} has no bound handler`);
    }
  }
  return handlers;
}
