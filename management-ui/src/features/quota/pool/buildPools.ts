/**
 * Glue between the quota cache, the credential list and the pool/routing models.
 * React-free so tests can drive it with plain objects.
 */

import type { AuthFileItem, ClaudeQuotaState, CodexQuotaState } from '@/types';
import { resolveCodexPlanType } from '@/utils/quota';
import type { LocalQuotaHistory } from './localHistory';
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
}

export interface ProviderPool {
  provider: PooledProvider;
  accountCount: number;
  summary: PoolSummary;
  routing: RoutingEntry[];
  /** Latest successful read across the provider's accounts in this browser. */
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
        history: series,
      },
      now
    );
  });

  const routing = predictNewSessionOrder(
    entries.map((entry) => {
      const plan = planUnitsFor(provider, planTypeOf(provider, entry));
      const series = seriesFor(history, provider, entry.key, weeklyId);
      const claudeObserved =
        provider === 'claude'
          ? Date.parse((entry.quota as ClaudeQuotaState | undefined)?.observedAt ?? '')
          : NaN;
      const observedAtMs = series
        ? series.points[series.points.length - 1].atMs
        : Number.isFinite(claudeObserved)
          ? claudeObserved
          : null;
      const read = hasData(entry.quota)
        ? {
            observedAtMs,
            weeklyWindowId: weeklyId,
            windows: entry.quota.windows,
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
