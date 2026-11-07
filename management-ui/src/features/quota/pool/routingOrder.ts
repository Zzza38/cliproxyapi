/**
 * Predicted order for new sessions under `routing.strategy: soonest-reset-first`.
 *
 * Mirrors the server's SoonestResetFirstSelector (sdk/cliproxy/auth/
 * selector_subscription.go) using the quota this dashboard last read:
 *
 * 1. Only enabled accounts in the highest credential priority, and not blocked
 *    by an exhausted weekly or short-term window.
 * 2. Earliest deadline first: the future weekly reset, or an available
 *    manual-reset credit's expiry when that is sooner (Codex only, as on the
 *    server). Credits are read, never redeemed.
 * 3. Then most remaining weekly allowance in base-plan units.
 * 4. Then credential name, for deterministic ties.
 *
 * Accounts without a known deadline go last; the server round-robins them.
 * Existing sessions stay pinned to their account and are not affected.
 *
 * The server probes on its own schedule, so this is an estimate from the
 * browser's reads, not a readout of the router's state.
 */

export interface RoutingWindow {
  id: string;
  usedPercent: number | null;
  resetAtMs?: number | null;
}

export interface RoutingCandidateInput {
  key: string;
  label: string;
  units: number;
  planLabel: string | null;
  planAssumed: boolean;
  disabled: boolean;
  priority: number;
  /** Null when this browser has not read the account's quota yet. */
  read: {
    /** When this browser read it; null when the read predates local recording. */
    observedAtMs: number | null;
    weeklyWindowId: string;
    windows: RoutingWindow[];
    /** Expiry instants of available, unspent manual-reset credits. */
    manualResetExpiriesMs: number[];
  } | null;
}

export type RoutingStatus =
  'ranked' | 'no-deadline' | 'blocked' | 'unread' | 'disabled' | 'lower-priority';

export interface RoutingEntry {
  key: string;
  label: string;
  status: RoutingStatus;
  /** 1-based position among accounts eligible for new sessions; null otherwise. */
  position: number | null;
  deadlineMs: number | null;
  deadlineSource: 'weekly' | 'manual-reset' | null;
  remainingUnits: number | null;
  units: number;
  planLabel: string | null;
  planAssumed: boolean;
  blockedUntilMs: number | null;
  observedAtMs: number | null;
}

const finite = (value: unknown): value is number =>
  typeof value === 'number' && Number.isFinite(value);

function rank(input: RoutingCandidateInput, now: number): RoutingEntry {
  const base: RoutingEntry = {
    key: input.key,
    label: input.label,
    status: 'unread',
    position: null,
    deadlineMs: null,
    deadlineSource: null,
    remainingUnits: null,
    units: input.units,
    planLabel: input.planLabel,
    planAssumed: input.planAssumed,
    blockedUntilMs: null,
    observedAtMs: input.read?.observedAtMs ?? null,
  };
  if (input.disabled) return { ...base, status: 'disabled' };
  if (!input.read) return base;
  // The router ignores allowance snapshots once they are five minutes old.
  if (!finite(input.read.observedAtMs) || now - input.read.observedAtMs >= 5 * 60_000) {
    return base;
  }

  const { windows, weeklyWindowId, manualResetExpiriesMs } = input.read;
  let blockedUntil: number | null = null;
  windows.forEach((window) => {
    if (!finite(window.usedPercent) || window.usedPercent < 100) return;
    if (!finite(window.resetAtMs) || window.resetAtMs <= now) return;
    if (blockedUntil === null || window.resetAtMs > blockedUntil) blockedUntil = window.resetAtMs;
  });

  const weekly = windows.find((window) => window.id === weeklyWindowId);
  const weeklyReset =
    weekly && finite(weekly.usedPercent) && finite(weekly.resetAtMs) && weekly.resetAtMs > now
      ? weekly.resetAtMs
      : null;
  const remainingUnits =
    weeklyReset !== null && weekly && finite(weekly.usedPercent)
      ? (Math.min(100, Math.max(0, 100 - weekly.usedPercent)) / 100) * input.units
      : null;

  let deadlineMs = weeklyReset;
  let deadlineSource: RoutingEntry['deadlineSource'] = weeklyReset !== null ? 'weekly' : null;
  manualResetExpiriesMs.forEach((expiry) => {
    if (expiry > now && (deadlineMs === null || expiry < deadlineMs)) {
      deadlineMs = expiry;
      deadlineSource = 'manual-reset';
    }
  });

  const entry = { ...base, deadlineMs, deadlineSource, remainingUnits };
  if (blockedUntil !== null) return { ...entry, status: 'blocked', blockedUntilMs: blockedUntil };
  return { ...entry, status: deadlineMs === null ? 'no-deadline' : 'ranked' };
}

const compareRanked = (a: RoutingEntry, b: RoutingEntry) => {
  if (a.deadlineMs !== b.deadlineMs) return (a.deadlineMs as number) - (b.deadlineMs as number);
  const remainingA = a.remainingUnits ?? -1;
  const remainingB = b.remainingUnits ?? -1;
  if (remainingA !== remainingB) return remainingB - remainingA;
  return a.key < b.key ? -1 : a.key > b.key ? 1 : 0;
};

const STATUS_ORDER: Record<RoutingStatus, number> = {
  ranked: 0,
  'no-deadline': 1,
  unread: 2,
  blocked: 3,
  'lower-priority': 4,
  disabled: 5,
};

/** Rank every candidate; eligible accounts get positions in the order new sessions try them. */
export function predictNewSessionOrder(
  candidates: RoutingCandidateInput[],
  now: number
): RoutingEntry[] {
  const entries = candidates.map((candidate) => ({ candidate, entry: rank(candidate, now) }));
  const available = entries.filter(
    ({ entry }) => entry.status !== 'disabled' && entry.status !== 'blocked'
  );
  const topPriority = available.length
    ? Math.max(...available.map(({ candidate }) => candidate.priority))
    : 0;
  const result = entries.map(({ candidate, entry }) =>
    entry.status !== 'disabled' && entry.status !== 'blocked' && candidate.priority < topPriority
      ? { ...entry, status: 'lower-priority' as const }
      : entry
  );

  const ranked = result.filter((entry) => entry.status === 'ranked').sort(compareRanked);
  const unranked = result
    .filter((entry) => entry.status !== 'ranked')
    .sort(
      (a, b) =>
        STATUS_ORDER[a.status] - STATUS_ORDER[b.status] ||
        (a.key < b.key ? -1 : a.key > b.key ? 1 : 0)
    );
  ranked.forEach((entry, index) => {
    entry.position = index + 1;
  });
  return [...ranked, ...unranked];
}
