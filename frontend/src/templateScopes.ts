import qwen25Template from "./templates/qwen2.5-7b-instruct.chat_template.jinja?raw";
import qwen3Template from "./templates/qwen3-8b.chat_template.jinja?raw";

/**
 * Scope grammar derived from chat template sources.
 *
 * The templates are vendored as reference data (see templates/README.md).
 * We never execute the Jinja; we extract the closed-scope grammar it uses:
 * the ChatML segment markers and every XML-style tag pair the template
 * itself emits (tools, tool_call, tool_response, think, ...). Adding another
 * model template means vending its file and adding one import.
 */
export interface TemplateScope {
  name: string;
  open: string;
  close: string;
  templates: string[];
}

export interface TemplateScopeRegistry {
  templateNames: string[];
  segment: { open: string; close: string };
  scopes: TemplateScope[];
}

const OPEN_TAG = /<([A-Za-z_][A-Za-z0-9_.:-]*)>/g;
const CLOSE_TAG = /<\/([A-Za-z_][A-Za-z0-9_.:-]*)>/g;
const MAX_TAG_NAME = 48;

function closeTag(name: string): string {
  return "</" + name + ">";
}

/** Extract every balanced tag pair literal that appears in the sources. */
export function parseTemplateScopes(sources: Record<string, string>): TemplateScopeRegistry {
  const scopes = new Map<string, TemplateScope>();
  let segmentOpen = "<|im_start|>";
  let segmentClose = "<|im_end|>";
  for (const [templateName, source] of Object.entries(sources)) {
    if (!source.includes(segmentOpen) || !source.includes(segmentClose)) {
      // A template without ChatML markers cannot define this dialect.
      continue;
    }
    const closes = new Set<string>();
    for (const match of source.matchAll(CLOSE_TAG)) {
      if (match[1].length <= MAX_TAG_NAME) closes.add(match[1]);
    }
    for (const match of source.matchAll(OPEN_TAG)) {
      const name = match[1];
      if (name.length > MAX_TAG_NAME || !closes.has(name)) continue;
      const existing = scopes.get(name);
      if (existing) {
        existing.templates.push(templateName);
      } else {
        scopes.set(name, {
          name,
          open: `<${name}>`,
          close: closeTag(name),
          templates: [templateName],
        });
      }
    }
  }
  return {
    templateNames: Object.keys(sources),
    segment: { open: segmentOpen, close: segmentClose },
    scopes: [...scopes.values()],
  };
}

let cached: TemplateScopeRegistry | undefined;

/** Registry for the vendored Qwen templates. */
export function loadQwenScopeRegistry(): TemplateScopeRegistry {
  if (!cached) {
    cached = parseTemplateScopes({
      "Qwen2.5-7B-Instruct": qwen25Template,
      "Qwen3-8B": qwen3Template,
    });
  }
  return cached;
}

export function scopeByName(registry: TemplateScopeRegistry, name: string): TemplateScope | undefined {
  return registry.scopes.find((scope) => scope.name === name);
}
