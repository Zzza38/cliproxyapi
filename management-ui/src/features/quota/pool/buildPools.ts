/**
 * Glue between the quota cache, the credential list and the pool/routing models.
 * React-free so tests can drive it with plain objects.
 */

import type { AuthFileItem, ClaudeQuotaState, CodexQuotaState } from '@/types';
import { resolveCodexPlanType } from '@/utils/quota';
import type { HistorySeries, LocalQuotaHistory } from './localHistory';
import { seriesFor } from './localHistory';
import { planUnitsFor, type PooledProvider } from './planUnits';
import {
  buildPoolAccount,
  summarizePool,
  WEEKLY_WINDOW_ID,
  type FutureUsage,
  type PoolSummary,
} from './poolModel';
import { predictNewSessionOrder, type RoutingEntry } from './routingOrder';

export type PooledQuotaState = ClaudeQuotaState | CodexQuotaState;

export interface PoolSourceEntry {
  key: string;
  label: string;
  file: AuthFileItem;
  quota: PooledQuotaState | undefined;
  /** When `quota` was read, if known (recorder timestamps; Claude also carries its own). */
  observedAtMs?: number | null;
  /** Latest time `quota` can have been read, when the exact time is unknown. */
  observedByMs?: number | null;
}

export interface ProviderPool {
  provider: PooledProvider;
  accountCount: number;
  summary: PoolSummary;
  routing: RoutingEntry[];
  /** Latest successful read across the provider's accounts (browser or server). */
  lastReadMs: number | null;
  /** Oldest successful read among accounts that have one (for staleness hints). */
  oldestReadMs: number | null;
}

export interface BuildPoolOptions {
  now: number;
  span: { startMs: number; endMs: number };
  burnWindowHours: number;
  futureUsage: FutureUsage;
}

const planTypeOf = (provider: PooledProvider, entry: PoolSourceEntry): string | null => {
  const fromQuota = entry.quota?.planType ?? null;
  if (fromQuota) return fromQuota;
  return provider === 'codex' ? resolveCodexPlanType(entry.file) : null;
};

const hasData = (quota: PooledQuotaState | undefined): quota is PooledQuotaState =>
  Boolean(
    quota && quota.windows.length > 0 && (quota.status === 'success' || quota.status === 'loading')
  );

const finite = (value: unknown): value is number =>
  typeof value === 'number' && Number.isFinite(value);

/** Exact read time of the live quota, when known. */
const liveReadAt = (provider: PooledProvider, entry: PoolSourceEntry): number | null => {
  if (finite(entry.observedAtMs)) return entry.observedAtMs;
  if (provider !== 'claude') return null;
  const parsed = Date.parse((entry.quota as ClaudeQuotaState | undefined)?.observedAt ?? '');
  return Number.isFinite(parsed) ? parsed : null;
};

/** Latest time the live quota can have been read. */
const liveReadBy = (provider: PooledProvider, entry: PoolSourceEntry): number | null => {
  const exact = liveReadAt(provider, entry);
  if (exact !== null) return exact;
  return finite(entry.observedByMs) ? entry.observedByMs : null;
};

const lastPointAtOrBefore = (series: HistorySeries | null, now: number) => {
  if (!series) return null;
  for (let i = series.points.length - 1; i >= 0; i -= 1) {
    if (series.points[i].atMs <= now) return series.points[i];
  }
  return null;
};

const manualResetExpiries = (
  provider: PooledProvider,
  quota: PooledQuotaState | undefined
): number[] => {
  // The server only folds Codex reset credits into routing; Claude grants are ignored there too.
  if (provider !== 'codex' || !quota) return [];
  return ((quota as CodexQuotaState).rateLimitResetCredits ?? [])
    .filter((credit) => credit.status.toLowerCase() === 'available')
    .map((credit) => Date.parse(credit.expiresAt))
    .filter((ms) => Number.isFinite(ms));
};

export function buildProviderPool(
  provider: PooledProvider,
  entries: PoolSourceEntry[],
  history: LocalQuotaHistory,
  options: BuildPoolOptions
): ProviderPool {
  const { now } = options;
  const weeklyId = WEEKLY_WINDOW_ID[provider];
  let lastReadMs: number | null = null;
  let oldestReadMs: number | null = null;

  const pooled = entries.filter((entry) => !entry.file.disabled);
  const accounts = pooled.map((entry) => {
    const plan = planUnitsFor(provider, planTypeOf(provider, entry));
    const series = seriesFor(history, provider, entry.key, weeklyId);
    const weekly = hasData(entry.quota)
      ? (entry.quota.windows.find((window) => window.id === weeklyId) ?? null)
      : null;
    return buildPoolAccount(
      {
        key: entry.key,
        label: entry.label,
        provider,
        units: plan.units,
        planLabel: plan.planLabel,
        planAssumed: plan.assumed,
        weekly,
        weeklyObservedByMs: weekly ? liveReadBy(provider, entry) : null,
        history: series,
      },
      now
    );
  });

  const routing = predictNewSessionOrder(
    entries.map((entry) => {
      const plan = planUnitsFor(provider, planTypeOf(provider, entry));
      const series = seriesFor(history, provider, entry.key, weeklyId);
      const last = lastPointAtOrBefore(series, now);
      const readBy = liveReadBy(provider, entry);
      // A history reading after the live read replaces its weekly window; the
      // short windows still come from the live read.
      const historyNewer = last !== null && readBy !== null && last.atMs > readBy;
      const observedAtMs = historyNewer
        ? last.atMs
        : (liveReadAt(provider, entry) ?? last?.atMs ?? null);
      const windows = hasData(entry.quota)
        ? historyNewer
          ? [
              ...entry.quota.windows.filter((window) => window.id !== weeklyId),
              { id: weeklyId, usedPercent: last.usedPercent, resetAtMs: last.resetAtMs },
            ]
          : entry.quota.windows
        : null;
      const read = windows
        ? {
            observedAtMs,
            weeklyWindowId: weeklyId,
            windows,
            manualResetExpiriesMs: manualResetExpiries(provider, entry.quota),
          }
        : null;
      if (read && observedAtMs !== null && !entry.file.disabled) {
        lastReadMs = lastReadMs === null ? observedAtMs : Math.max(lastReadMs, observedAtMs);
        oldestReadMs = oldestReadMs === null ? observedAtMs : Math.min(oldestReadMs, observedAtMs);
      }
      return {
        key: entry.key,
        label: entry.label,
        units: plan.units,
        planLabel: plan.planLabel,
        planAssumed: plan.assumed,
        disabled: Boolean(entry.file.disabled),
        priority: typeof entry.file.priority === 'number' ? entry.file.priority : 0,
        read,
      };
    }),
    now
  );

  return {
    provider,
    accountCount: pooled.length,
    summary: summarizePool(accounts, options),
    routing,
    lastReadMs,
    oldestReadMs,
  };
}
