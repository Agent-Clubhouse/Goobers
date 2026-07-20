import type {
  GraphTerminal,
  WorkflowGraph,
  WorkflowGraphEdge,
  WorkflowGraphNode,
} from "./api/types";

const NODE_WIDTH = 172;
const NODE_HEIGHT = 86;
const COLUMN_GAP = 92;
const ROW_GAP = 54;
const PADDING = 48;
const CYCLE_LANE_HEIGHT = 34;

export const MIN_GRAPH_ZOOM = 0.75;
export const MAX_GRAPH_ZOOM = 1.4;

export interface WorkflowLayoutStage {
  id: string;
  type: "stage";
  node: WorkflowGraphNode;
  x: number;
  y: number;
  width: number;
  height: number;
  depth: number;
}

export interface WorkflowLayoutTerminal {
  id: string;
  type: "terminal";
  terminal: GraphTerminal;
  x: number;
  y: number;
  width: number;
  height: number;
  depth: number;
}

export type WorkflowLayoutNode = WorkflowLayoutStage | WorkflowLayoutTerminal;

export interface WorkflowLayoutEdge {
  id: string;
  edge: WorkflowGraphEdge;
  path: string;
  labelX: number;
  labelY: number;
  repass: boolean;
}

export interface WorkflowGraphLayout {
  width: number;
  height: number;
  nodes: WorkflowLayoutNode[];
  edges: WorkflowLayoutEdge[];
  stageOrder: WorkflowGraphNode[];
}

interface PendingNode {
  id: string;
  type: "stage" | "terminal";
  node?: WorkflowGraphNode;
  terminal?: GraphTerminal;
  depth: number;
  order: number;
}

export function layoutWorkflowGraph(graph: WorkflowGraph): WorkflowGraphLayout {
  if (graph.nodes.length === 0) {
    return { width: 0, height: 0, nodes: [], edges: [], stageOrder: [] };
  }

  const nodeById = new Map(graph.nodes.map((node) => [node.id, node]));
  const outgoing = new Map<string, WorkflowGraphEdge[]>();
  for (const edge of graph.edges) {
    const edges = outgoing.get(edge.source) ?? [];
    edges.push(edge);
    outgoing.set(edge.source, edges);
  }

  const depths = new Map<string, number>();
  const discovery = new Map<string, number>();
  let discoveryIndex = 0;

  const visitFrom = (root: string, rootDepth: number) => {
    const queue = [root];
    depths.set(root, rootDepth);
    discovery.set(root, discoveryIndex++);
    for (let index = 0; index < queue.length; index += 1) {
      const source = queue[index];
      const sourceDepth = depths.get(source) ?? rootDepth;
      for (const edge of outgoing.get(source) ?? []) {
        if (edge.terminal || !nodeById.has(edge.target) || depths.has(edge.target)) {
          continue;
        }
        depths.set(edge.target, sourceDepth + 1);
        discovery.set(edge.target, discoveryIndex++);
        queue.push(edge.target);
      }
    }
  };

  if (nodeById.has(graph.start)) {
    visitFrom(graph.start, 0);
  }
  for (const node of graph.nodes) {
    if (!depths.has(node.id)) {
      visitFrom(node.id, maxDepth(depths) + 1);
    }
  }

  const pending: PendingNode[] = graph.nodes.map((node, index) => ({
    id: node.id,
    type: "stage",
    node,
    depth: depths.get(node.id) ?? 0,
    order: discovery.get(node.id) ?? index,
  }));
  const terminalByEdge = new Map<number, string>();
  graph.edges.forEach((edge, index) => {
    if (!edge.terminal) {
      return;
    }
    const id = `terminal-${index}-${edge.terminal}`;
    terminalByEdge.set(index, id);
    pending.push({
      id,
      type: "terminal",
      terminal: edge.terminal,
      depth: (depths.get(edge.source) ?? 0) + 1,
      order: graph.nodes.length + index,
    });
  });

  const levels = new Map<number, PendingNode[]>();
  for (const node of pending) {
    const level = levels.get(node.depth) ?? [];
    level.push(node);
    levels.set(node.depth, level);
  }
  for (const level of levels.values()) {
    level.sort((left, right) => left.order - right.order || left.id.localeCompare(right.id));
  }

  const deepest = Math.max(...levels.keys());
  const maxRows = Math.max(...[...levels.values()].map((level) => level.length));
  const repassCount = graph.edges.filter(
    (edge) =>
      !edge.terminal &&
      nodeById.has(edge.target) &&
      (depths.get(edge.target) ?? 0) <= (depths.get(edge.source) ?? 0),
  ).length;
  const contentHeight =
    PADDING * 2 + maxRows * NODE_HEIGHT + Math.max(0, maxRows - 1) * ROW_GAP;
  const height = contentHeight + repassCount * CYCLE_LANE_HEIGHT;
  const width = PADDING * 2 + (deepest + 1) * NODE_WIDTH + deepest * COLUMN_GAP;
  const nodes: WorkflowLayoutNode[] = [];

  for (const [depth, level] of levels) {
    const levelHeight =
      level.length * NODE_HEIGHT + Math.max(0, level.length - 1) * ROW_GAP;
    const top = PADDING + (contentHeight - PADDING * 2 - levelHeight) / 2;
    level.forEach((pendingNode, row) => {
      const positioned = {
        id: pendingNode.id,
        x: PADDING + depth * (NODE_WIDTH + COLUMN_GAP),
        y: top + row * (NODE_HEIGHT + ROW_GAP),
        width: NODE_WIDTH,
        height: NODE_HEIGHT,
        depth,
      };
      if (pendingNode.type === "stage" && pendingNode.node) {
        nodes.push({ ...positioned, type: "stage", node: pendingNode.node });
      } else if (pendingNode.type === "terminal" && pendingNode.terminal) {
        nodes.push({ ...positioned, type: "terminal", terminal: pendingNode.terminal });
      }
    });
  }

  nodes.sort(
    (left, right) =>
      left.depth - right.depth || left.y - right.y || left.id.localeCompare(right.id),
  );
  const positionedById = new Map(nodes.map((node) => [node.id, node]));
  let repassLane = 0;
  const edges = graph.edges.flatMap<WorkflowLayoutEdge>((edge, index) => {
    const source = positionedById.get(edge.source);
    const target = positionedById.get(
      edge.terminal ? terminalByEdge.get(index) ?? "" : edge.target,
    );
    if (!source || !target) {
      return [];
    }

    const repass = !edge.terminal && target.depth <= source.depth;
    let path: string;
    let labelX: number;
    let labelY: number;
    if (repass) {
      const laneY = contentHeight + repassLane * CYCLE_LANE_HEIGHT + CYCLE_LANE_HEIGHT / 2;
      repassLane += 1;
      const sourceX = source.x + source.width / 2;
      const targetX = target.x + target.width / 2;
      const sourceY = source.y + source.height;
      const targetY = target.y + target.height;
      path = `M ${sourceX} ${sourceY} C ${sourceX} ${laneY}, ${targetX} ${laneY}, ${targetX} ${targetY}`;
      labelX = (sourceX + targetX) / 2;
      labelY = laneY - 7;
    } else {
      const sourceX = source.x + source.width;
      const sourceY = source.y + source.height / 2;
      const targetX = target.x;
      const targetY = target.y + target.height / 2;
      const midpoint = (sourceX + targetX) / 2;
      path = `M ${sourceX} ${sourceY} C ${midpoint} ${sourceY}, ${midpoint} ${targetY}, ${targetX} ${targetY}`;
      labelX = midpoint;
      labelY = (sourceY + targetY) / 2 - 9;
    }

    return [
      {
        id: `${edge.source}-${edge.target || edge.terminal}-${edge.outcome || index}`,
        edge,
        path,
        labelX,
        labelY,
        repass,
      },
    ];
  });

  return {
    width,
    height,
    nodes,
    edges,
    stageOrder: nodes.flatMap((node) => (node.type === "stage" ? [node.node] : [])),
  };
}

export function fitGraphZoom(
  graphWidth: number,
  graphHeight: number,
  viewportWidth: number,
  viewportHeight: number,
): number {
  if (graphWidth <= 0 || graphHeight <= 0 || viewportWidth <= 0 || viewportHeight <= 0) {
    return 1;
  }
  const ratio = Math.min(
    1,
    (viewportWidth - 24) / graphWidth,
    (viewportHeight - 24) / graphHeight,
  );
  return clampGraphZoom(Math.floor(ratio * 100) / 100);
}

export function clampGraphZoom(zoom: number): number {
  return Math.min(MAX_GRAPH_ZOOM, Math.max(MIN_GRAPH_ZOOM, Math.round(zoom * 100) / 100));
}

export function terminalLabel(terminal: GraphTerminal): string {
  switch (terminal) {
    case "abort":
      return "Abort";
    case "complete":
      return "Complete";
    case "escalate":
      return "Escalate";
  }
}

function maxDepth(depths: Map<string, number>): number {
  return depths.size === 0 ? -1 : Math.max(...depths.values());
}
