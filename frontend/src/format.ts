/** Small shared display formatters. */

/** Compact occupancy formatting: 987, 52.3k, 1.2M. Unknown stays undefined. */
export function formatTokens(tokens: number | undefined): string {
  if (tokens === undefined || !Number.isFinite(tokens) || tokens < 0) return "—";
  if (tokens < 1000) return String(tokens);
  if (tokens < 1_000_000) {
    const value = tokens / 1000;
    return `${value >= 100 ? Math.round(value) : value.toFixed(1)}k`;
  }
  return `${(tokens / 1_000_000).toFixed(1)}M`;
}
