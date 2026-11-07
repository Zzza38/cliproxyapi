/**
 * Provider quota pools: every credential of a provider pooled into one gauge.
 *
 * Adapted from the pooled quota charts in yazinsai/prox (commit f6a0125628,
 * MIT, a fork of Cli-Proxy-API-Management-Center). Differences:
 *
 * - Values are base-plan units, not percent. Each account contributes
 *   `remaining fraction × plan units` (see planUnits.ts), so a Pro 200 at 25%
 *   (5 units) outweighs a Pro 100 at 40% (4 units). Pools sum units.
 * - The pooled window is the weekly allowance the router ranks on: Claude
 *   `seven-day`, Codex `weekly`.
 * - Recorded history is the server's minute-by-minute weekly history merged
 *   with this browser's own reads (serverHistory.ts, localHistory.ts). The
 *   recorded pool line only starts once every pooled account has at least one
 *   real reading; earlier stretches are left blank rather than filled with guesses.
 *
 * Pure functions, no React and no clock of its own (`now` is always passed in).
 */

import type { HistorySeries } from './localHistory';
import type { PooledProvider } from './planUnits';

export const HOUR_MS = 60 * 60 * 1000;
export const DAY_MS = 24 * HOUR_MS;

export const POOL_MODES = ['remaining', 'drain'] as const;
export type PoolMode = (typeof POOL_MODES)[number];

export const BURN_WINDOW_HOURS = [1, 3, 6] as const;
export type BurnWindowHours = (typeof BURN_WINDOW_HOURS)[number];

export const FUTURE_USAGE_OPTIONS = ['same', 'half', 'double', 'none'] as const;
export type FutureUsage = (typeof FUTURE_USAGE_OPTIONS)[number];

export const FUTURE_USAGE_FACTOR: Record<FutureUsage, number> = {
  same: 1,
  half: 0.5,
  double: 2,
  none: 0,
};

/** Live weekly window id per pooled provider. */
export const WEEKLY_WINDOW_ID: Record<PooledProvider, string> = {
  claude: 'seven-day',
  codex: 'weekly',
};

/** The chart spans two weeks: a few days of record, the rest projection. */
export const POOL_SPAN_DAYS = 14;
export const POOL_DAYS_BEFORE_TODAY = 3;

/** Reset instants within this distance are one reset event. */
const RESET_GROUP_MS = 60 * 1000;
/** Burn needs two real readings at least this far apart. */
export const MIN_BURN_SPAN_MS = 60 * 1000;
/** Without a live read, a stored reading this recent still counts as current. */
export const HISTORY_CURRENT_MAX_AGE_MS = 15 * 60 * 1000;
const EPSILON = 1e-4;
/** Smallest reset jump (units) worth labelling. */
const JUMP_LABEL_MIN = 0.05;

export interface PoolPoint {
  atMs: number;
  value: number;
}

export interface PoolJump {
  atMs: number;
  /** Units restored (positive). */
  added: number;
  /** Pool value right after the jump, for label placement. */
  value: number;
}

export interface LiveWeeklyWindow {
  usedPercent: number | null;
  resetAtMs?: number | null;
  periodHours?: number | null;
}

export interface PoolAccountInput {
  key: string;
  label: string;
  provider: PooledProvider;
  units: number;
  planLabel: string | null;
  planAssumed: boolean;
  /** Weekly window from the latest successful read, if any. */
  weekly: LiveWeeklyWindow | null;
  /**
   * Latest time `weekly` can have been read (its read time, or an upper bound when
   * unknown). A history reading after it is newer and takes precedence. Null when
   * not even a bound is known; the live read then wins as before.
   */
  weeklyObservedByMs?: number | null;
  /** Recorded readings of the same weekly window (browser and server, merged). */
  history: HistorySeries | null;
}

export interface PoolAccount {
  key: string;
  label: string;
  provider: PooledProvider;
  units: number;
  planLabel: string | null;
  planAssumed: boolean;
  /** Remaining weekly allowance in units right now; null when unknown. */
  remaining: number | null;
  nextResetMs: number | null;
  periodMs: number | null;
  /** Recorded remaining units over time; resets appear as two points sharing a time. */
  history: PoolPoint[];
  /** First and last real reading in `history` (it is extended to "now" for drawing). */
  observedFromMs: number | null;
  observedToMs: number | null;
  source: 'live' | 'history' | 'none';
}

const startOfDay = (ms: number): number => {
  const date = new Date(ms);
  date.setHours(0, 0, 0, 0);
  return date.getTime();
};

/** Visible span: today minus a few days, two weeks long, stepped a week at a time. */
export function poolSpan(offsetWeeks: number, now: number): { startMs: number; endMs: number } {
  const start = new Date(startOfDay(now));
  start.setDate(start.getDate() - POOL_DAYS_BEFORE_TODAY + offsetWeeks * 7);
  start.setHours(0, 0, 0, 0);
  const end = new Date(start);
  end.setDate(end.getDate() + POOL_SPAN_DAYS);
  end.setHours(0, 0, 0, 0);
  return { startMs: start.getTime(), endMs: end.getTime() };
}

/** Local midnight boundaries inside the span (DST-safe). */
export function poolDays(span: { startMs: number; endMs: number }): number[] {
  const days: number[] = [];
  const cursor = new Date(span.startMs);
  while (cursor.getTime() < span.endMs && days.length <= 62) {
    days.push(cursor.getTime());
    // Re-anchor to midnight: after a midnight DST jump the day starts at 01:00.
    cursor.setDate(cursor.getDate() + 1);
    cursor.setHours(0, 0, 0, 0);
  }
  return days;
}

/* ------------------------------------------------------------- accounts */

const clampPercent = (value: number) => Math.min(100, Math.max(0, value));
const toUnits = (usedPercent: number, units: number) =>
  ((100 - clampPercent(usedPercent)) / 100) * units;

/** First window end strictly after `now` for a window that ended at `anchorMs`. */
export const nextBoundary = (anchorMs: number, periodMs: number, now: number): number | null => {
  if (!(periodMs > 0)) return null;
  if (anchorMs > now) return anchorMs;
  return anchorMs + (Math.floor((now - anchorMs) / periodMs) + 1) * periodMs;
};

/**
 * Remaining-unit samples from recorded readings. A window that rolled between
 * two readings is drawn as a vertical reset at the old reset instant, not a
 * slope across the gap.
 */
export function historySamples(series: HistorySeries | null, units: number): PoolPoint[] {
  if (!series) return [];
  const out: PoolPoint[] = [];
  series.points.forEach((point, index) => {
    const prev = index > 0 ? series.points[index - 1] : null;
    if (
      prev &&
      prev.resetAtMs !== null &&
      point.resetAtMs !== null &&
      point.resetAtMs - prev.resetAtMs > 2 * RESET_GROUP_MS &&
      prev.resetAtMs > prev.atMs &&
      prev.resetAtMs < point.atMs
    ) {
      out.push({ atMs: prev.resetAtMs, value: toUnits(prev.usedPercent, units) });
      out.push({ atMs: prev.resetAtMs, value: units });
    }
    out.push({ atMs: point.atMs, value: toUnits(point.usedPercent, units) });
  });
  return out;
}

export function buildPoolAccount(input: PoolAccountInput, now: number): PoolAccount {
  const units = input.units > 0 && Number.isFinite(input.units) ? input.units : 1;
  // Readings stamped after `now` (clock skew) wait until they are in the past.
  const points = input.history?.points.filter((point) => point.atMs <= now) ?? [];
  const series: HistorySeries | null =
    input.history && points.length > 0
      ? { windowMinutes: input.history.windowMinutes, points }
      : null;
  const samples = historySamples(series, units);

  const account: PoolAccount = {
    key: input.key,
    label: input.label,
    provider: input.provider,
    units,
    planLabel: input.planLabel,
    planAssumed: input.planAssumed,
    remaining: null,
    nextResetMs: null,
    periodMs: null,
    history: samples,
    observedFromMs: points.length > 0 ? points[0].atMs : null,
    observedToMs: points.length > 0 ? points[points.length - 1].atMs : null,
    source: 'none',
  };

  const live = input.weekly;
  const liveUsed = live && typeof live.usedPercent === 'number' ? live.usedPercent : null;
  const livePeriodMs =
    live && typeof live.periodHours === 'number' && live.periodHours > 0
      ? live.periodHours * HOUR_MS
      : null;
  const liveReset = live && typeof live.resetAtMs === 'number' ? live.resetAtMs : null;
  const lastPoint = series ? series.points[series.points.length - 1] : null;
  const liveBy = typeof input.weeklyObservedByMs === 'number' ? input.weeklyObservedByMs : null;
  // The newer observation wins: a history reading made after the live read, or a
  // recent one when nothing was read live.
  const useHistory =
    lastPoint !== null &&
    (liveUsed === null
      ? now - lastPoint.atMs <= HISTORY_CURRENT_MAX_AGE_MS
      : liveBy !== null && lastPoint.atMs > liveBy);

  if (useHistory && series && lastPoint) {
    const periodMs = series.windowMinutes * 60 * 1000;
    account.periodMs = periodMs;
    account.source = 'history';
    if (lastPoint.resetAtMs === null || lastPoint.resetAtMs > now) {
      account.remaining = toUnits(lastPoint.usedPercent, units);
      account.nextResetMs = lastPoint.resetAtMs;
    } else {
      account.remaining = units;
      account.nextResetMs = nextBoundary(lastPoint.resetAtMs, periodMs, now);
    }
  } else if (liveUsed !== null) {
    account.source = 'live';
    account.periodMs = livePeriodMs;
    if (liveReset === null || liveReset > now) {
      account.remaining = toUnits(liveUsed, units);
      account.nextResetMs = liveReset;
    } else {
      // The reported window already reset; its usage no longer applies.
      account.remaining = units;
      account.nextResetMs = livePeriodMs ? nextBoundary(liveReset, livePeriodMs, now) : null;
    }
  }

  // Close the recorded line at "now" so it meets the projection.
  if (account.remaining !== null && samples.length > 0) {
    const lastSample = samples[samples.length - 1];
    if (
      lastPoint?.resetAtMs &&
      lastPoint.resetAtMs > lastSample.atMs &&
      lastPoint.resetAtMs < now
    ) {
      samples.push({ atMs: lastPoint.resetAtMs, value: lastSample.value });
      samples.push({ atMs: lastPoint.resetAtMs, value: units });
    }
    if (samples[samples.length - 1].atMs < now) {
      samples.push({ atMs: now, value: account.remaining });
    }
  }
  return account;
}

/* ------------------------------------------------------------ polylines */

/**
 * Value of a polyline at `t`. Points sharing a time form a vertical jump:
 * `side` picks the value before ('left') or after ('right') it. Outside the
 * polyline the nearest end value is held.
 */
export function valueAt(points: PoolPoint[], t: number, side: 'left' | 'right' = 'right'): number {
  if (points.length === 0) return 0;
  if (t <= points[0].atMs) {
    if (t < points[0].atMs || side === 'left') return points[0].value;
  }
  const last = points[points.length - 1];
  if (t > last.atMs) return last.value;
  let lo = 0;
  let hi = points.length - 1;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (points[mid].atMs < t) lo = mid + 1;
    else hi = mid;
  }
  // lo = first index with atMs >= t
  if (points[lo].atMs === t) {
    if (side === 'left') return points[lo].value;
    let end = lo;
    while (end + 1 < points.length && points[end + 1].atMs === t) end += 1;
    return points[end].value;
  }
  const before = points[lo - 1];
  const after = points[lo];
  const ratio = (t - before.atMs) / (after.atMs - before.atMs);
  return before.value + (after.value - before.value) * ratio;
}

/** Restrict a polyline to [fromMs, toMs], interpolating the cut ends. */
export function clipPolyline(points: PoolPoint[], fromMs: number, toMs: number): PoolPoint[] {
  if (points.length === 0 || toMs <= fromMs) return [];
  const first = points[0].atMs;
  const last = points[points.length - 1].atMs;
  if (last < fromMs || first > toMs) return [];
  const start = Math.max(fromMs, first);
  const end = Math.min(toMs, last);
  const inner = points.filter((point) => point.atMs > start && point.atMs < end);
  const out: PoolPoint[] = [{ atMs: start, value: valueAt(points, start, 'right') }, ...inner];
  if (end > start) out.push({ atMs: end, value: valueAt(points, end, 'left') });
  return out;
}

/** Sum of several polylines, keeping vertical jumps where any of them jumps. */
export function sumPolylines(lines: PoolPoint[][]): { points: PoolPoint[]; jumps: PoolJump[] } {
  const active = lines.filter((line) => line.length > 0);
  if (active.length === 0) return { points: [], jumps: [] };
  const times = Array.from(new Set(active.flatMap((line) => line.map((p) => p.atMs)))).sort(
    (a, b) => a - b
  );
  const points: PoolPoint[] = [];
  const jumps: PoolJump[] = [];
  times.forEach((t) => {
    const left = active.reduce((sum, line) => sum + valueAt(line, t, 'left'), 0);
    const right = active.reduce((sum, line) => sum + valueAt(line, t, 'right'), 0);
    points.push({ atMs: t, value: left });
    if (Math.abs(right - left) > EPSILON) {
      points.push({ atMs: t, value: right });
      if (right - left >= JUMP_LABEL_MIN)
        jumps.push({ atMs: t, added: right - left, value: right });
    }
  });
  return { points, jumps };
}

/* ---------------------------------------------------------------- burn */

/**
 * Average consumption in units per hour over up to `windowHours` of observed
 * history, ending at the latest real reading (`observed.toMs`). The line drawn
 * from that reading to "now" is not observed and does not count.
 * Only decreases count: a reset restores quota, it does not un-spend it.
 * Spend between two readings is spread evenly over the time between them, so a
 * gap (page closed) contributes its measured total at an average rate.
 * Null (unknown, not zero) without two real readings at least a minute apart,
 * or when the latest reading is older than the window.
 */
export function burnRate(
  history: PoolPoint[],
  now: number,
  windowHours: number,
  observed: { fromMs: number | null; toMs: number | null } = {
    fromMs: history[0]?.atMs ?? null,
    toMs: history[history.length - 1]?.atMs ?? null,
  }
): number | null {
  if (history.length < 2 || observed.fromMs === null || observed.toMs === null) return null;
  const windowMs = windowHours * HOUR_MS;
  const to = Math.min(now, observed.toMs);
  if (to < now - windowMs) return null;
  const from = Math.max(to - windowMs, observed.fromMs, history[0].atMs);
  if (to - from < MIN_BURN_SPAN_MS) return null;
  const segment = clipPolyline(history, from, to);
  let spent = 0;
  for (let i = 1; i < segment.length; i += 1) {
    if (segment[i].atMs > segment[i - 1].atMs) {
      spent += Math.max(0, segment[i - 1].value - segment[i].value);
    }
  }
  return spent / ((to - from) / HOUR_MS);
}

/* ---------------------------------------------------------- projection */

export interface AccountProjection {
  key: string;
  points: PoolPoint[];
  emptyAtMs: number | null;
}

/**
 * Project one account forward from `now` to `endMs`: draining at `burnPerHour`
 * units per hour until empty, refilling to its full allowance at each reset.
 */
export function projectAccount(
  account: PoolAccount,
  burnPerHour: number,
  now: number,
  endMs: number
): AccountProjection {
  const result: AccountProjection = { key: account.key, points: [], emptyAtMs: null };
  if (account.remaining === null || endMs <= now) return result;
  const rate = Math.max(0, burnPerHour) / HOUR_MS;
  let value = account.remaining;
  let t = now;
  let nextReset =
    account.nextResetMs !== null && account.nextResetMs > now ? account.nextResetMs : null;
  const period = account.periodMs && account.periodMs >= HOUR_MS ? account.periodMs : null;
  if (value <= EPSILON) result.emptyAtMs = now;
  result.points.push({ atMs: t, value });

  for (let guard = 0; guard < 2000; guard += 1) {
    const segmentEnd = Math.min(nextReset ?? Infinity, endMs);
    if (rate > 0 && value > 0) {
      const emptyAt = t + value / rate;
      if (emptyAt < segmentEnd) {
        result.points.push({ atMs: emptyAt, value: 0 });
        result.emptyAtMs ??= emptyAt;
        value = 0;
        t = emptyAt;
      }
    }
    value = Math.max(0, value - (segmentEnd - t) * rate);
    t = segmentEnd;
    result.points.push({ atMs: t, value });
    if (t >= endMs || nextReset === null) break;
    value = account.units;
    result.points.push({ atMs: t, value });
    nextReset = period ? nextReset + period : null;
  }
  return result;
}

/** Measure total consumption over one interval covered by every account.
 * Accumulating spend before pooling prevents one account's reset from hiding
 * another account's consumption at the same instant.
 */
export function poolBurnRate(
  accounts: PoolAccount[],
  now: number,
  windowHours: number
): number | null {
  if (
    accounts.length === 0 ||
    accounts.some(
      (account) =>
        account.observedFromMs === null ||
        account.observedToMs === null ||
        account.history.length < 2
    )
  )
    return null;
  const fromMs = Math.max(...accounts.map((account) => account.observedFromMs as number));
  const toMs = Math.min(...accounts.map((account) => account.observedToMs as number));
  const spentLines = accounts.map((account) => {
    let spent = 0;
    return account.history.map((point, index) => {
      const previous = account.history[index - 1];
      if (previous && point.atMs > previous.atMs)
        spent += Math.max(0, previous.value - point.value);
      return { atMs: point.atMs, value: -spent };
    });
  });
  return burnRate(sumPolylines(spentLines).points, now, windowHours, { fromMs, toMs });
}

/** Forecast a single pool-wide demand, moving it to another account when empty.
 * Reset order chooses which balance supplies demand; past per-account usage
 * never determines whether an account will be used in the future.
 */
export function projectPool(
  accounts: PoolAccount[],
  burnPerHour: number,
  now: number,
  endMs: number
): {
  accounts: AccountProjection[];
  resets: PoolResetEvent[];
} {
  const states = accounts.map((account) => ({
    account,
    value: account.remaining ?? 0,
    reset:
      account.nextResetMs !== null && account.nextResetMs > now ? account.nextResetMs : Infinity,
    projection: {
      key: account.key,
      points: [{ atMs: now, value: account.remaining ?? 0 }],
      emptyAtMs: (account.remaining ?? 0) <= EPSILON ? now : null,
    } as AccountProjection,
  }));
  const resets: PoolResetEvent[] = [];
  const rate = Math.max(0, burnPerHour) / HOUR_MS;
  let t = now;
  for (let guard = 0; t < endMs && guard < 20000; guard += 1) {
    const active = states
      .filter((state) => state.value > EPSILON)
      .sort(
        (a, b) =>
          (a.reset === b.reset ? 0 : a.reset < b.reset ? -1 : 1) ||
          b.value - a.value ||
          a.account.key.localeCompare(b.account.key)
      )[0];
    const nextReset = Math.min(...states.map((state) => state.reset));
    const emptyAt = active && rate > 0 ? t + active.value / rate : Infinity;
    const until = Math.min(endMs, nextReset, emptyAt);
    if (active) {
      active.value = Math.max(0, active.value - (until - t) * rate);
      if (active.value <= EPSILON && rate > 0) {
        active.value = 0;
        active.projection.emptyAtMs ??= until;
      }
    }
    t = until;
    for (const state of states) state.projection.points.push({ atMs: t, value: state.value });
    for (const state of states) {
      if (state.reset !== t) continue;
      resets.push({ atMs: t, added: state.account.units - state.value });
      state.value = state.account.units;
      state.projection.points.push({ atMs: t, value: state.value });
      state.reset =
        state.account.periodMs && state.account.periodMs >= HOUR_MS
          ? t + state.account.periodMs
          : Infinity;
    }
  }
  return { accounts: states.map((state) => state.projection), resets };
}

/* ---------------------------------------------------------------- pool */

export interface PoolResetEvent {
  atMs: number;
  added: number;
}

export interface PoolSummary {
  accounts: PoolAccount[];
  /** Accounts whose remaining allowance is known. */
  knownCount: number;
  /** Remaining units across known accounts. */
  total: number | null;
  /** Full weekly allowance of the known accounts, in units. */
  capacity: number;
  /** Pool burn over the selected window, units per hour; null without enough history. */
  burnPerHour: number | null;
  recorded: PoolPoint[];
  recordedJumps: PoolJump[];
  /** Start of the recorded pool line (every known account has a reading from here on). */
  recordedStartMs: number | null;
  projected: PoolPoint[];
  projectedJumps: PoolJump[];
  /** Per-account lines (recorded joined with projected), for the accounts overlay. */
  accountLines: { key: string; label: string; points: PoolPoint[] }[];
  poolEmptyAtMs: number | null;
  firstEmpty: { atMs: number; account: PoolAccount } | null;
  nextReset: PoolResetEvent | null;
}

export interface PoolOptions {
  now: number;
  span: { startMs: number; endMs: number };
  burnWindowHours: number;
  futureUsage: FutureUsage;
}

export function summarizePool(accounts: PoolAccount[], options: PoolOptions): PoolSummary {
  const { now, span, burnWindowHours, futureUsage } = options;
  const known = accounts.filter((account) => account.remaining !== null);
  const factor = FUTURE_USAGE_FACTOR[futureUsage];

  const burnPerHour = poolBurnRate(known, now, burnWindowHours);

  // Recorded: only where every known account has real readings, up to now.
  const recordedEnd = Math.min(now, span.endMs);
  const everyHasHistory = known.length > 0 && known.every((account) => account.history.length > 0);
  const latestFirst = everyHasHistory
    ? Math.max(...known.map((account) => account.history[0].atMs))
    : null;
  const recordedStart =
    latestFirst === null || latestFirst >= now ? null : Math.max(span.startMs, latestFirst);
  const recordedLines = new Map<string, PoolPoint[]>();
  if (recordedStart !== null && recordedEnd > recordedStart) {
    known.forEach((account) => {
      recordedLines.set(account.key, clipPolyline(account.history, recordedStart, recordedEnd));
    });
  }
  const recorded = sumPolylines(Array.from(recordedLines.values()));

  // Projected: from now (or the span start, when looking ahead) to the span end.
  const nextResetAt = Math.min(...known.map((account) => account.nextResetMs ?? Infinity));
  const horizon = Math.max(
    span.endMs,
    now,
    Number.isFinite(nextResetAt) ? nextResetAt + RESET_GROUP_MS : now
  );
  const forecast = projectPool(known, (burnPerHour ?? 0) * factor, now, horizon);
  const projections = forecast.accounts;
  const projectedStart = Math.max(now, span.startMs);
  const projectedLines = projections.map((projection) =>
    clipPolyline(projection.points, projectedStart, span.endMs)
  );
  const projected = sumPolylines(projectedLines);

  let poolEmptyAtMs: number | null = null;
  if (known.length > 0) {
    const full = sumPolylines(projections.map((projection) => projection.points)).points;
    const index = full.findIndex((point) => point.value <= EPSILON);
    if (index === 0) poolEmptyAtMs = now;
    else if (index > 0) {
      const before = full[index - 1];
      const at = full[index];
      poolEmptyAtMs =
        before.value <= at.value
          ? at.atMs
          : before.atMs + ((at.atMs - before.atMs) * before.value) / (before.value - at.value);
    }
  }

  const firstEmpty = projections.reduce<PoolSummary['firstEmpty']>((best, projection, index) => {
    if (projection.emptyAtMs === null) return best;
    if (best && best.atMs <= projection.emptyAtMs) return best;
    return { atMs: projection.emptyAtMs, account: known[index] };
  }, null);

  const nextAt = forecast.resets[0]?.atMs;
  const nextReset: PoolResetEvent | null =
    nextAt === undefined
      ? null
      : {
          atMs: nextAt,
          added: forecast.resets
            .filter((reset) => reset.atMs - nextAt <= RESET_GROUP_MS)
            .reduce((sum, reset) => sum + reset.added, 0),
        };

  return {
    accounts,
    knownCount: known.length,
    total:
      known.length > 0
        ? known.reduce((sum, account) => sum + (account.remaining as number), 0)
        : null,
    capacity: known.reduce((sum, account) => sum + account.units, 0),
    burnPerHour,
    recorded: recorded.points,
    recordedJumps: recorded.jumps,
    recordedStartMs: recorded.points.length > 1 ? recordedStart : null,
    projected: projected.points,
    projectedJumps: projected.jumps,
    accountLines: known.map((account, index) => ({
      key: account.key,
      label: account.label,
      points: [...(recordedLines.get(account.key) ?? []), ...projectedLines[index]],
    })),
    poolEmptyAtMs,
    firstEmpty,
    nextReset,
  };
}

/* --------------------------------------------------------------- drain */

/**
 * Consumption rate over time in units per hour: hourly buckets across the
 * recorded line, then the projected rate as a step line.
 */
export function drainSeries(
  recorded: PoolPoint[],
  projected: PoolPoint[],
  bucketMs = HOUR_MS
): { recorded: PoolPoint[]; projected: PoolPoint[] } {
  const recordedOut: PoolPoint[] = [];
  if (recorded.length > 1) {
    const start = recorded[0].atMs;
    const end = recorded[recorded.length - 1].atMs;
    for (let from = start; from < end; from += bucketMs) {
      const to = Math.min(end, from + bucketMs);
      const segment = clipPolyline(recorded, from, to);
      let spent = 0;
      for (let i = 1; i < segment.length; i += 1) {
        if (segment[i].atMs > segment[i - 1].atMs) {
          spent += Math.max(0, segment[i - 1].value - segment[i].value);
        }
      }
      const rate = spent / ((to - from) / HOUR_MS);
      recordedOut.push({ atMs: from, value: rate }, { atMs: to, value: rate });
    }
  }
  const projectedOut: PoolPoint[] = [];
  for (let i = 1; i < projected.length; i += 1) {
    const a = projected[i - 1];
    const b = projected[i];
    if (b.atMs <= a.atMs) continue;
    const rate = Math.max(0, (a.value - b.value) / ((b.atMs - a.atMs) / HOUR_MS));
    projectedOut.push({ atMs: a.atMs, value: rate }, { atMs: b.atMs, value: rate });
  }
  return { recorded: recordedOut, projected: projectedOut };
}

/** A round axis maximum. */
export function niceCeiling(value: number): number {
  if (!(value > 0)) return 1;
  const magnitude = 10 ** Math.floor(Math.log10(value));
  const step = [1, 2, 2.5, 5, 10].find((candidate) => candidate * magnitude >= value) ?? 10;
  return step * magnitude;
}
