/**
 * Per-provider token totals from `GET /observability/usage/tokens` (server default:
 * the last 24 hours). The server records them from request usage, independently of
 * quota polling. `inputTokens` already includes cache reads and writes; `totalTokens`
 * is the server's own total and is shown as is, never re-summed here.
 */

export interface TokenTotals {
  requests: number;
  inputTokens: number;
  outputTokens: number;
  cacheReadTokens: number;
  cacheWriteTokens: number;
  totalTokens: number;
}

export interface TokenUsage {
  fromMs: number;
  toMs: number;
  providers: Record<string, TokenTotals>;
}

export type TokenUsageResponse = { enabled: false } | { enabled: true; usage: TokenUsage };

export const TOKEN_FIELDS = [
  'requests',
  'inputTokens',
  'outputTokens',
  'cacheReadTokens',
  'cacheWriteTokens',
  'totalTokens',
] as const satisfies readonly (keyof TokenTotals)[];

export const emptyTokenTotals = (): TokenTotals => ({
  requests: 0,
  inputTokens: 0,
  outputTokens: 0,
  cacheReadTokens: 0,
  cacheWriteTokens: 0,
  totalTokens: 0,
});

const count = (value: unknown): number =>
  typeof value === 'number' && Number.isFinite(value) && value > 0 ? value : 0;

/** Validate a response; null when it is not a recognisable token-usage answer. */
export function parseTokenUsage(payload: unknown): TokenUsageResponse | null {
  if (!payload || typeof payload !== 'object') return null;
  const body = payload as Record<string, unknown>;
  if (body.enabled === false) return { enabled: false };
  if (body.enabled !== true) return null;
  const { fromMs, toMs } = body;
  if (typeof fromMs !== 'number' || typeof toMs !== 'number' || !(toMs > fromMs)) return null;
  const providers: Record<string, TokenTotals> = {};
  if (body.providers && typeof body.providers === 'object') {
    Object.entries(body.providers as Record<string, unknown>).forEach(([provider, raw]) => {
      if (!raw || typeof raw !== 'object') return;
      const totals = emptyTokenTotals();
      TOKEN_FIELDS.forEach((field) => {
        totals[field] = count((raw as Record<string, unknown>)[field]);
      });
      providers[provider] = totals;
    });
  }
  return { enabled: true, usage: { fromMs, toMs, providers } };
}

const compactFormats = new Map<string, Intl.NumberFormat>();

/** "1.2M", "940K", "12" */
export function formatTokenCount(value: number, locale?: string): string {
  if (!Number.isFinite(value)) return '-';
  const key = locale ?? '';
  let format = compactFormats.get(key);
  if (!format) {
    try {
      format = new Intl.NumberFormat(locale, { notation: 'compact', maximumFractionDigits: 1 });
    } catch {
      format = new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 });
    }
    compactFormats.set(key, format);
  }
  return format.format(value);
}
