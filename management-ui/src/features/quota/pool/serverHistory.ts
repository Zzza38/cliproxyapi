/**
 * Server-persisted weekly quota history.
 *
 * The server samples every Claude/Codex credential's weekly window once a minute
 * and keeps 28 days (`GET /credentials/quota/history`). It uses the same shape as
 * the browser-local log, so both merge into one in-memory history: server readings
 * win on duplicate timestamps and browser-only windows (five-hour, etc.) stay local.
 * Server history is never written to localStorage.
 *
 * Pure functions; the request loop lives in historySync.ts.
 */

import {
  HISTORY_RETENTION_MS,
  normalizeHistory,
  type LocalQuotaHistory,
  type StoredCredential,
  type StoredReading,
  type StoredSeries,
} from './localHistory';
import type { PooledProvider } from './planUnits';

/** Weekly window id per provider, as stored in history (matches WEEKLY_WINDOW_ID). */
const WEEKLY_SERIES_ID: Record<PooledProvider, string> = {
  claude: 'seven-day',
  codex: 'weekly',
};
export const WEEKLY_WINDOW_MINUTES = 7 * 24 * 60;

/** Server import limits. */
export const IMPORT_MAX_ROWS = 100_000;
export const IMPORT_MAX_BYTES = 8 * 1024 * 1024;

export type ServerHistoryResponse =
  { enabled: false } | { enabled: true; history: LocalQuotaHistory };

const clampPercent = (value: number) => Math.min(100, Math.max(0, value));

/**
 * Validate a GET response. Null when it is not a recognisable response (treated
 * like an unavailable endpoint). Only weekly series of correctly prefixed ids
 * are kept, and readings older than the retention window are dropped.
 */
export function parseServerHistory(payload: unknown, now: number): ServerHistoryResponse | null {
  if (!payload || typeof payload !== 'object') return null;
  const enabled = (payload as { enabled?: unknown }).enabled;
  if (enabled === false) return { enabled: false };
  if (enabled !== true) return null;
  const parsed = normalizeHistory(payload);
  if (!parsed) return null;

  const cutoff = now - HISTORY_RETENTION_MS;
  const credentials: Record<string, StoredCredential> = {};
  Object.entries(parsed.credentials).forEach(([id, credential]) => {
    if (!id.startsWith(`${credential.provider}:`)) return;
    const windowId = WEEKLY_SERIES_ID[credential.provider];
    const item = credential.series[windowId];
    if (!item) return;
    const readings: StoredReading[] = [];
    item.readings.forEach(([atMs, used, reset]) => {
      if (atMs < cutoff || atMs <= 0) return;
      readings.push([atMs, clampPercent(used), reset > 0 ? reset : 0]);
    });
    if (readings.length === 0) return;
    credentials[id] = {
      provider: credential.provider,
      series: { [windowId]: { windowMinutes: item.windowMinutes, readings } },
    };
  });
  return {
    enabled: true,
    history: { version: 1, startedAtMs: parsed.startedAtMs, credentials },
  };
}

/** Merge two time-sorted reading lists; `preferred` wins when timestamps are equal. */
function mergeReadings(other: StoredReading[], preferred: StoredReading[]): StoredReading[] {
  const out: StoredReading[] = [];
  let i = 0;
  let j = 0;
  while (i < other.length || j < preferred.length) {
    const a = other[i];
    const b = preferred[j];
    if (b === undefined || (a !== undefined && a[0] < b[0])) {
      out.push(a);
      i += 1;
    } else if (a === undefined || b[0] < a[0]) {
      out.push(b);
      j += 1;
    } else {
      out.push(b);
      i += 1;
      j += 1;
    }
  }
  return out;
}

/**
 * Local history with server history merged in. Untouched credentials and series
 * keep their object identity so converted series stay cached between merges.
 */
export function mergeHistories(
  local: LocalQuotaHistory,
  server: LocalQuotaHistory | null
): LocalQuotaHistory {
  if (!server || Object.keys(server.credentials).length === 0) return local;
  const credentials: Record<string, StoredCredential> = { ...local.credentials };
  Object.entries(server.credentials).forEach(([id, remote]) => {
    const own = credentials[id];
    if (!own || own.provider !== remote.provider) {
      credentials[id] = remote;
      return;
    }
    const series: Record<string, StoredSeries> = { ...own.series };
    Object.entries(remote.series).forEach(([windowId, item]) => {
      const mine = series[windowId];
      series[windowId] = mine
        ? {
            windowMinutes: item.windowMinutes,
            readings: mergeReadings(mine.readings, item.readings),
          }
        : item;
    });
    credentials[id] = { provider: own.provider, series };
  });
  const starts = [local.startedAtMs, server.startedAtMs].filter(
    (value): value is number => value !== null
  );
  return {
    version: 1,
    startedAtMs: starts.length > 0 ? Math.min(...starts) : null,
    credentials,
  };
}

const byteLength = (text: string) =>
  typeof TextEncoder !== 'undefined' ? new TextEncoder().encode(text).length : text.length * 3;

/**
 * Weekly browser readings to import into the server store, for credentials that
 * still exist. Newest readings are kept when over the row or size limit. Null
 * when there is nothing to import. The local log itself is left untouched.
 */
export function buildImportPayload(
  local: LocalQuotaHistory,
  liveCredentialIds: Set<string>,
  now: number
): LocalQuotaHistory | null {
  const cutoff = now - HISTORY_RETENTION_MS;
  const rows: { id: string; provider: PooledProvider; reading: StoredReading }[] = [];
  Object.entries(local.credentials).forEach(([id, credential]) => {
    if (!liveCredentialIds.has(id) || !id.startsWith(`${credential.provider}:`)) return;
    const item = credential.series[WEEKLY_SERIES_ID[credential.provider]];
    if (!item || item.windowMinutes !== WEEKLY_WINDOW_MINUTES) return;
    item.readings.forEach((reading) => {
      if (reading[0] >= cutoff && reading[0] <= now) {
        rows.push({ id, provider: credential.provider, reading });
      }
    });
  });
  if (rows.length === 0) return null;
  rows.sort((a, b) => b.reading[0] - a.reading[0]);

  const build = (count: number): LocalQuotaHistory => {
    const credentials: Record<string, StoredCredential> = {};
    rows.slice(0, count).forEach(({ id, provider, reading }) => {
      credentials[id] ??= {
        provider,
        series: {
          [WEEKLY_SERIES_ID[provider]]: { windowMinutes: WEEKLY_WINDOW_MINUTES, readings: [] },
        },
      };
      credentials[id].series[WEEKLY_SERIES_ID[provider]].readings.push(reading);
    });
    Object.values(credentials).forEach((credential) =>
      Object.values(credential.series).forEach((item) => item.readings.reverse())
    );
    return { version: 1, startedAtMs: local.startedAtMs, credentials };
  };

  let count = Math.min(rows.length, IMPORT_MAX_ROWS);
  for (;;) {
    const payload = build(count);
    if (byteLength(JSON.stringify(payload)) <= IMPORT_MAX_BYTES) return payload;
    count = Math.floor(count / 2);
    if (count === 0) return null;
  }
}

/** Number of readings a successful import reported, or null for an unexpected answer. */
export function parseImportResponse(payload: unknown): number | null {
  const imported = (payload as { imported?: unknown } | null)?.imported;
  return typeof imported === 'number' && Number.isFinite(imported) ? imported : null;
}
