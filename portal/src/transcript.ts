// Parsing for captured agent transcripts.
//
// A transcript is newline-delimited JSON in the canonical GenAI event shape
// (internal/harness/transcript.go, schema goobers.dev/telemetry/genai-event/v1).
// Real transcripts run to hundreds of events and hundreds of kilobytes: one
// implement stage recorded 160+ events and 1.47M input tokens. Rendered as raw
// JSONL that is technically complete and practically unreadable — roles are
// indistinguishable, tool calls are not paired with their results, content is
// JSON-escaped, and the two most-wanted points (the concluding message and the
// usage record) sit at the far end of a wall of text.
//
// This module turns the byte stream into turns a UI can render. It never drops
// information: an unparseable line becomes a turn of its own rather than being
// skipped, because a transcript is evidence and silently discarding part of it
// would misrepresent what the agent did.

export type TranscriptRole = "system" | "user" | "assistant" | "tool" | "unknown";

export interface TranscriptUsage {
  inputTokens?: number;
  outputTokens?: number;
  cacheReadTokens?: number;
  cacheWriteTokens?: number;
  reasoningTokens?: number;
  requests?: number;
  cost?: number;
}

export interface TranscriptToolCall {
  id: string;
  name?: string;
  /** Pretty-printed arguments, or the raw scalar when not an object. */
  arguments?: string;
  success?: boolean;
}

/** One rendered turn. A tool call and its result are one turn, not two. */
export interface TranscriptTurn {
  index: number;
  role: TranscriptRole;
  content?: string;
  model?: string;
  usage?: TranscriptUsage;
  toolCall?: TranscriptToolCall;
  /** The paired tool result's content, when this turn is a tool call. */
  toolResult?: string;
  /** True when the line could not be parsed; content carries the raw line. */
  malformed?: boolean;
  truncated?: boolean;
}

export interface ParsedTranscript {
  turns: TranscriptTurn[];
  /** Total usage reported by the final usage-bearing event, when present. */
  usage?: TranscriptUsage;
  malformedLines: number;
}

const ROLES: TranscriptRole[] = ["system", "user", "assistant", "tool"];

function asRole(value: unknown): TranscriptRole {
  return typeof value === "string" && (ROLES as string[]).includes(value)
    ? (value as TranscriptRole)
    : "unknown";
}

function asNumber(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function parseUsage(raw: unknown): TranscriptUsage | undefined {
  if (!raw || typeof raw !== "object") {
    return undefined;
  }
  const record = raw as Record<string, unknown>;
  const usage: TranscriptUsage = {
    inputTokens: asNumber(record.input_tokens),
    outputTokens: asNumber(record.output_tokens),
    cacheReadTokens: asNumber(record.cache_read_tokens),
    cacheWriteTokens: asNumber(record.cache_write_tokens),
    reasoningTokens: asNumber(record.reasoning_tokens),
    requests: asNumber(record.requests),
    cost: asNumber(record.cost),
  };
  return Object.values(usage).some((value) => value !== undefined) ? usage : undefined;
}

// Arguments arrive as arbitrary JSON. Objects are pretty-printed so a shell
// command reads as a command; a bare string is shown as-is rather than wrapped
// in quotes it never had.
function parseToolArguments(raw: unknown): string | undefined {
  if (raw === undefined || raw === null) {
    return undefined;
  }
  if (typeof raw === "string") {
    return raw;
  }
  try {
    return JSON.stringify(raw, null, 2);
  } catch {
    return String(raw);
  }
}

function parseToolCall(raw: unknown): TranscriptToolCall | undefined {
  if (!raw || typeof raw !== "object") {
    return undefined;
  }
  const record = raw as Record<string, unknown>;
  return {
    id: typeof record.id === "string" ? record.id : "",
    name: typeof record.name === "string" ? record.name : undefined,
    arguments: parseToolArguments(record.arguments),
    success: typeof record.success === "boolean" ? record.success : undefined,
  };
}

/**
 * parseTranscript turns raw JSONL bytes into renderable turns, folding each
 * tool result into the assistant turn that called it.
 *
 * Pairing is by tool-call id, and only backwards onto a still-unpaired call:
 * an adapter that reuses an id, or emits a result with no matching call, must
 * not cause a result to overwrite an unrelated turn or vanish. An unmatched
 * result stays a turn of its own.
 */
export function parseTranscript(text: string): ParsedTranscript {
  const turns: TranscriptTurn[] = [];
  const pendingByToolId = new Map<string, number>();
  let usage: TranscriptUsage | undefined;
  let malformedLines = 0;

  for (const line of text.split("\n")) {
    const trimmed = line.trim();
    if (trimmed === "") {
      continue;
    }

    let record: Record<string, unknown>;
    try {
      const decoded: unknown = JSON.parse(trimmed);
      if (!decoded || typeof decoded !== "object" || Array.isArray(decoded)) {
        throw new Error("not an object");
      }
      record = decoded as Record<string, unknown>;
    } catch {
      malformedLines += 1;
      turns.push({ index: turns.length, role: "unknown", content: trimmed, malformed: true });
      continue;
    }

    const role = asRole(record.role);
    const toolCall = parseToolCall(record.tool_call);
    const content = typeof record.content === "string" ? record.content : undefined;
    const eventUsage = parseUsage(record.usage);
    if (eventUsage) {
      usage = eventUsage;
    }

    // A tool result folds into the call it answers, so the reader sees one
    // "ran X, got Y" unit instead of two rows to correlate by id.
    if (role === "tool" && toolCall?.id) {
      const target = pendingByToolId.get(toolCall.id);
      if (target !== undefined) {
        const turn = turns[target];
        turn.toolResult = content ?? "";
        if (toolCall.success !== undefined && turn.toolCall) {
          turn.toolCall.success = toolCall.success;
        }
        pendingByToolId.delete(toolCall.id);
        continue;
      }
    }

    const turn: TranscriptTurn = {
      index: turns.length,
      role,
      content,
      model: typeof record.model === "string" ? record.model : undefined,
      usage: eventUsage,
      toolCall,
      truncated: record.truncated === true,
    };
    turns.push(turn);

    if (role === "assistant" && toolCall?.id) {
      pendingByToolId.set(toolCall.id, turn.index);
    }
  }

  return { turns, usage, malformedLines };
}

/**
 * transcriptSearchMatches returns the indexes of turns matching a query,
 * case-insensitively, across content, tool name, arguments, and result — every
 * place a reader might have seen the string.
 */
export function transcriptSearchMatches(turns: TranscriptTurn[], query: string): number[] {
  const needle = query.trim().toLowerCase();
  if (needle === "") {
    return [];
  }
  const matches: number[] = [];
  for (const turn of turns) {
    const haystack = [turn.content, turn.toolCall?.name, turn.toolCall?.arguments, turn.toolResult]
      .filter((value): value is string => typeof value === "string")
      .join("\n")
      .toLowerCase();
    if (haystack.includes(needle)) {
      matches.push(turn.index);
    }
  }
  return matches;
}

/** Turns carrying no content at all — an empty role marker the UI can skip. */
export function isEmptyTurn(turn: TranscriptTurn): boolean {
  return !turn.content && !turn.toolCall && !turn.usage && !turn.toolResult;
}
