import type { ExchangeSnapshot } from "./contracts";
import { normalizeContext, normalizeContextFromValue, type ContextBlock, type ContextDocument } from "./contextIr";
import { buildLiveStream, parseSseRecords, type LiveStreamState } from "./streamIr";

/**
 * Merged session view (docs/session-spec.md §6): one continuous context stream
 * for a session lineage.
 *
 * Structure comes from the harness behaviour (the lineage of session
 * placements), while turn content comes from wire truth: assistant turns are
 * rendered from the response artifacts (the authoritative source, including
 * reasoning the harness may strip later) and interstitials (tool results, new
 * user turns) come from the next request's new messages after the canonical
 * echo is removed.
 */

export interface MergedTurnInput {
  exchange: ExchangeSnapshot;
  requestBody?: string;
  requestIsSse?: boolean;
  responseBody?: string;
  responseIsSse?: boolean;
}

export interface MergedTurn {
  exchange: ExchangeSnapshot;
  depth: number;
  /** Turn one renders its full request document; later turns render only their new messages. */
  contextBlocks: ContextBlock[];
  contextDocument: ContextDocument | undefined;
  /** Assistant-authority blocks from this turn's response artifact. */
  responseBlocks: ContextBlock[];
  responseStream: LiveStreamState | undefined;
  markers: string[];
}

export interface MergedSession {
  turns: MergedTurn[];
  warnings: string[];
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function parseJSON(text: string | undefined): unknown {
  if (text === undefined) return undefined;
  try {
    return JSON.parse(text);
  } catch {
    return undefined;
  }
}

/**
 * The normalized message sequence of a request, matching the backend's
 * message_count semantics: the top-level system/instructions element is the
 * virtual first message.
 */
export function messageItems(protocol: string, request: unknown): unknown[] {
  if (!isRecord(request)) return [];
  if (protocol === "responses") {
    const items: unknown[] = [];
    if (request.instructions !== undefined) items.push(request.instructions);
    const input = request.input;
    if (Array.isArray(input)) items.push(...input);
    else if (input !== undefined) items.push(input);
    return items;
  }
  if (protocol === "anthropic_messages") {
    const items: unknown[] = [];
    if (request.system !== undefined) items.push({ role: "system", content: request.system });
    if (Array.isArray(request.messages)) items.push(...request.messages);
    return items;
  }
  if (Array.isArray(request.messages)) return [...request.messages];
  return [];
}

/** A message that repeats the previous turn's assistant response. */
function isAssistantEcho(protocol: string, message: unknown): boolean {
  if (!isRecord(message)) return false;
  if (protocol === "responses") {
    const type = typeof message.type === "string" ? message.type : "";
    if (type === "function_call" || type === "reasoning" || type === "custom_tool_call") return true;
    if (type === "message") return message.role === "assistant" || message.role === undefined;
    return false;
  }
  return message.role === "assistant";
}

/** The response-side assistant message count, used to flag rewritten echoes. */
function responseAssistantCount(protocol: string, response: unknown): number {
  if (!isRecord(response)) return 0;
  if (protocol === "responses") {
    if (!Array.isArray(response.output)) return 0;
    return response.output.filter((item) => isAssistantEcho(protocol, item)).length;
  }
  if (protocol === "anthropic_messages") {
    return response.content !== undefined || response.stop_reason !== undefined ? 1 : 0;
  }
  if (Array.isArray(response.choices)) {
    return response.choices.filter((choice) => isRecord(choice) && choice.message !== undefined).length;
  }
  return 0;
}

/** Message blocks from a synthetic request carrying only the given messages. */
function interstitialBlocks(protocol: string, messages: unknown[]): ContextBlock[] {
  if (messages.length === 0) return [];
  const carrier =
    protocol === "responses"
      ? { input: messages }
      : protocol === "anthropic_messages"
        ? { messages }
        : { messages };
  const document = normalizeContextFromValue(protocol, carrier);
  return document.blocks;
}

/** Assistant blocks from a JSON response body, keeping only content sections. */
function responseBlocksFromJson(protocol: string, body: string): ContextBlock[] {
  const document = normalizeContext(protocol, body);
  const prefixes: Record<string, string[]> = {
    chat_completions: ["/choices"],
    anthropic_messages: ["/content"],
    responses: ["/output"],
  };
  const allowed = prefixes[protocol] ?? [];
  return document.blocks.filter(
    (block) =>
      allowed.some((prefix) => block.sourcePointer.startsWith(prefix)) &&
      // A response message with null content yields an empty block; it carries
      // nothing to read next to the tool calls it accompanies.
      (block.text !== undefined || block.content.some((part) => part.value !== null && part.value !== undefined)),
  );
}

export function buildMergedSession(
  protocol: string,
  lineage: MergedTurnInput[],
  live: LiveStreamState | undefined,
  selectedExchangeId: string | undefined,
): MergedSession {
  const turns: MergedTurn[] = [];
  const warnings: string[] = [];
  for (let i = 0; i < lineage.length; i++) {
    const input = lineage[i];
    const assignment = input.exchange.session;
    const markers: string[] = [];
    if (i > 0) {
      if (assignment?.model_changed) markers.push("model changed");
      if (assignment?.tools_changed) markers.push("tools changed");
    }
    if (input.exchange.request.artifact_refs.some((ref) => ref.stage === "derived")) markers.push("request edited");

    const request = parseJSON(input.requestBody);
    let contextBlocks: ContextBlock[] = [];
    let contextDocument: ContextDocument | undefined;
    if (i === 0) {
      contextDocument = input.requestBody !== undefined ? normalizeContext(protocol, input.requestBody) : undefined;
      contextBlocks = contextDocument?.blocks ?? [];
    } else {
      // Interstitials: this request's messages beyond the parent's context,
      // with the canonical echo of the parent's response removed.
      const parentCount = lineage[i - 1].exchange.summary?.message_count;
      const items = messageItems(protocol, request);
      const prefix =
        typeof parentCount === "number" && parentCount > 0 ? Math.min(parentCount, items.length) : 0;
      let echoEnd = prefix;
      while (echoEnd < items.length && isAssistantEcho(protocol, items[echoEnd])) echoEnd++;
      const echoRun = items.slice(prefix, echoEnd);
      const rest = items.slice(echoEnd);

      const parentHasResponse = turns[i - 1].responseBlocks.length > 0 || turns[i - 1].responseStream !== undefined;
      if (!parentHasResponse && echoRun.length > 0) {
        // No authoritative response to render: keep the request-side echo so
        // nothing is silently dropped, marked for the operator.
        contextBlocks = interstitialBlocks(protocol, [...echoRun, ...rest]);
        markers.push("response unavailable · request echo shown");
      } else {
        contextBlocks = interstitialBlocks(protocol, rest);
        if (parentHasResponse && echoRun.length === 0 && rest.length > 0) {
          markers.push("canonical echo missing");
        }
        // A rewritten echo is visible without deep content comparison when
        // the assistant-message count disagrees with the JSON response.
        if (echoRun.length > 0 && !lineage[i - 1].responseIsSse) {
          const expected = responseAssistantCount(protocol, parseJSON(lineage[i - 1].responseBody));
          if (expected !== undefined && expected > 0 && echoRun.length !== expected) {
            markers.push("canonical echo differs");
          }
        }
      }
    }

    // The response side is the assistant authority. The selected turn may
    // still be streaming, in which case the live projection replaces it.
    const isSelected = input.exchange.exchange_id === selectedExchangeId;
    let responseBlocks: ContextBlock[] = [];
    let responseStream: LiveStreamState | undefined;
    if (isSelected && live && live.status === "streaming") {
      responseStream = live;
    } else if (input.responseIsSse && input.responseBody !== undefined) {
      responseStream = buildLiveStream(protocol, parseSseRecords(input.responseBody));
    } else if (input.responseBody !== undefined) {
      responseBlocks = responseBlocksFromJson(protocol, input.responseBody);
    }
    if (input.responseBody === undefined && !responseStream) {
      const refs = input.exchange.response.artifact_refs ?? [];
      if (refs.length > 0 && input.exchange.state !== "request_held") markers.push("response pending");
    }

    turns.push({
      exchange: input.exchange,
      depth: assignment?.depth ?? i + 1,
      contextBlocks,
      contextDocument,
      responseBlocks,
      responseStream,
      markers,
    });
  }
  return { turns, warnings };
}

/** The artifact actually forwarded (wire truth), falling back to inbound. */
export function forwardedRequestArtifact(exchange: ExchangeSnapshot) {
  const refs = exchange.request.artifact_refs ?? [];
  return refs.find((ref) => ref.stage === "request.upstream") ?? refs.find((ref) => ref.stage === "request.inbound") ?? refs[0];
}

/** The lineage of a session: selected exchange and its ancestors. */
export function sessionLineage(exchanges: ExchangeSnapshot[], selectedExchangeId: string | undefined): ExchangeSnapshot[] {
  if (!selectedExchangeId) return [];
  const byId = new Map(exchanges.map((exchange) => [exchange.exchange_id, exchange]));
  const lineage: ExchangeSnapshot[] = [];
  let current = byId.get(selectedExchangeId);
  const guard = new Set<string>();
  while (current && !guard.has(current.exchange_id)) {
    guard.add(current.exchange_id);
    lineage.unshift(current);
    const parentID = current.session?.parent_exchange_id;
    current = parentID ? byId.get(parentID) : undefined;
  }
  return lineage;
}
