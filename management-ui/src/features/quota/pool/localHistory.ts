/**
 * Browser-local quota history.
 *
 * The server exposes no quota-history endpoint, so the pool charts record what
 * this browser actually observed: every successful Claude/Codex quota read is
 * appended here with its observation time. Nothing is back-filled or invented.
 * Gaps (page closed, other browsers, server-side probes) stay gaps in the data;
 * the chart joins consecutive readings with straight lines and says so.
 *
 * Storage is scoped per management API base so two servers never share a log.
 * Pure helpers take and return plain objects; only load/save touch storage.
 */

import type { PooledProvider } from './planUnits';

/** [observedAtMs, usedPercent, resetAtMs | 0] — compact for localStorage. */
export type StoredReading = [number, number, number];

export interface StoredSeries {
  windowMinutes: number;
  readings: StoredReading[];
}

export interface StoredCredential {
  provider: PooledProvider;
  /** Window id → readings. Ids match the live windows ('seven-day', 'weekly', 'five-hour'). */
  series: Record<string, StoredSeries>;
}

export interface LocalQuotaHistory {
  version: 1;
  /** First reading ever stored in this log (survives pruning of old readings). */
  startedAtMs: number | null;
  credentials: Record<string, StoredCredential>;
}

export interface HistoryReading {
  atMs: number;
  usedPercent: number;
  resetAtMs: number | null;
}

export interface HistorySeries {
  windowMinutes: number;
  points: HistoryReading[];
}

export interface ObservedWindow {
  id: string;
  usedPercent: number | null;
  resetAtMs?: number | null;
  periodHours?: number | null;
}

export const HISTORY_RETENTION_MS = 28 * 24 * 60 * 60 * 1000;
export const MAX_READINGS_PER_SERIES = 3000;
/** A reading this close to the previous one replaces it instead of adding a point. */
export const MIN_READING_GAP_MS = 30 * 1000;

const STORAGE_PREFIX = 'cliproxy.quotaHistory.v1:';

export const emptyHistory = (): LocalQuotaHistory => ({
  version: 1,
  startedAtMs: null,
  credentials: {},
});

const credentialId = (provider: PooledProvider, key: string) => `${provider}:${key}`;

const isFiniteNumber = (value: unknown): value is number =>
  typeof value === 'number' && Number.isFinite(value);

/**
 * Append one observation of a credential's windows. Returns the same object
 * when nothing usable was observed, so callers can skip a storage write.
 */
export function recordReading(
  history: LocalQuotaHistory,
  provider: PooledProvider,
  key: string,
  windows: ObservedWindow[],
  atMs: number
): LocalQuotaHistory {
  const usable = windows.filter(
    (window) => isFiniteNumber(window.usedPercent) && isFiniteNumber(window.periodHours)
  );
  if (usable.length === 0 || !isFiniteNumber(atMs)) return history;

  const id = credentialId(provider, key);
  const previous = history.credentials[id];
  const series: Record<string, StoredSeries> = { ...(previous?.series ?? {}) };

  usable.forEach((window) => {
    const used = Math.min(100, Math.max(0, window.usedPercent as number));
    const reset = isFiniteNumber(window.resetAtMs) && window.resetAtMs > 0 ? window.resetAtMs : 0;
    const existing = series[window.id];
    const readings = existing ? existing.readings.slice() : [];
    const last = readings[readings.length - 1];
    const reading: StoredReading = [atMs, used, reset];
    if (last && atMs < last[0]) return; // never rewrite the past
    if (last && atMs - last[0] < MIN_READING_GAP_MS) readings[readings.length - 1] = reading;
    else readings.push(reading);
    series[window.id] = {
      windowMinutes: Math.round((window.periodHours as number) * 60),
      readings: readings.slice(-MAX_READINGS_PER_SERIES),
    };
  });

  return {
    version: 1,
    startedAtMs: history.startedAtMs ?? atMs,
    credentials: { ...history.credentials, [id]: { provider, series } },
  };
}

/** Drop readings older than the retention window and credentials that no longer exist. */
export function pruneHistory(
  history: LocalQuotaHistory,
  now: number,
  liveCredentialIds?: Set<string>
): LocalQuotaHistory {
  const cutoff = now - HISTORY_RETENTION_MS;
  const credentials: Record<string, StoredCredential> = {};
  Object.entries(history.credentials).forEach(([id, credential]) => {
    if (liveCredentialIds && !liveCredentialIds.has(id)) return;
    const series: Record<string, StoredSeries> = {};
    Object.entries(credential.series).forEach(([windowId, item]) => {
      const readings = item.readings.filter((reading) => reading[0] >= cutoff);
      if (readings.length > 0) series[windowId] = { ...item, readings };
    });
    if (Object.keys(series).length > 0) credentials[id] = { ...credential, series };
  });
  return { ...history, credentials };
}

export const historyCredentialId = credentialId;

export function seriesFor(
  history: LocalQuotaHistory,
  provider: PooledProvider,
  key: string,
  windowId: string
): HistorySeries | null {
  const item = history.credentials[credentialId(provider, key)]?.series[windowId];
  if (!item || item.readings.length === 0) return null;
  return {
    windowMinutes: item.windowMinutes,
    points: item.readings.map(([atMs, usedPercent, resetAtMs]) => ({
      atMs,
      usedPercent,
      resetAtMs: resetAtMs > 0 ? resetAtMs : null,
    })),
  };
}

/** Oldest reading and reading count for one provider's weekly series. */
export function historyCoverage(
  history: LocalQuotaHistory,
  provider: PooledProvider,
  windowId: string
): { oldestMs: number | null; readings: number } {
  let oldestMs: number | null = null;
  let readings = 0;
  Object.values(history.credentials).forEach((credential) => {
    if (credential.provider !== provider) return;
    const item = credential.series[windowId];
    if (!item || item.readings.length === 0) return;
    readings += item.readings.length;
    const first = item.readings[0][0];
    oldestMs = oldestMs === null ? first : Math.min(oldestMs, first);
  });
  return { oldestMs, readings };
}

/* ------------------------------------------------------------- storage */

const isStoredReading = (value: unknown): value is StoredReading =>
  Array.isArray(value) && value.length === 3 && value.every(isFiniteNumber);

/** Validate untrusted storage content; anything malformed is dropped, not repaired. */
export function parseHistory(raw: string | null): LocalQuotaHistory {
  if (!raw) return emptyHistory();
  try {
    const parsed = JSON.parse(raw) as Partial<LocalQuotaHistory>;
    if (!parsed || parsed.version !== 1 || typeof parsed.credentials !== 'object') {
      return emptyHistory();
    }
    const credentials: Record<string, StoredCredential> = {};
    Object.entries(parsed.credentials ?? {}).forEach(([id, credential]) => {
      if (!credential || (credential.provider !== 'claude' && credential.provider !== 'codex')) {
        return;
      }
      const series: Record<string, StoredSeries> = {};
      Object.entries(credential.series ?? {}).forEach(([windowId, item]) => {
        if (!item || !isFiniteNumber(item.windowMinutes) || !Array.isArray(item.readings)) return;
        const readings = item.readings.filter(isStoredReading).sort((a, b) => a[0] - b[0]);
        if (readings.length > 0) series[windowId] = { windowMinutes: item.windowMinutes, readings };
      });
      credentials[id] = { provider: credential.provider, series };
    });
    return {
      version: 1,
      startedAtMs: isFiniteNumber(parsed.startedAtMs) ? parsed.startedAtMs : null,
      credentials,
    };
  } catch {
    return emptyHistory();
  }
}

const storageKey = (scope: string) => `${STORAGE_PREFIX}${scope || 'default'}`;

export function loadHistory(scope: string): LocalQuotaHistory {
  if (typeof window === 'undefined') return emptyHistory();
  try {
    return parseHistory(window.localStorage.getItem(storageKey(scope)));
  } catch {
    return emptyHistory();
  }
}

export function saveHistory(scope: string, history: LocalQuotaHistory): void {
  if (typeof window === 'undefined') return;
  try {
    window.localStorage.setItem(storageKey(scope), JSON.stringify(history));
  } catch {
    // Storage full or blocked: keep running with in-memory history only.
  }
}

export function clearHistory(scope: string): void {
  if (typeof window === 'undefined') return;
  try {
    window.localStorage.removeItem(storageKey(scope));
  } catch {
    // ignore
  }
}
