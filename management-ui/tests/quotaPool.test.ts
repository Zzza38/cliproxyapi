import { afterAll, beforeAll, describe, expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import i18n from '../src/i18n/index';
import type { AuthFileItem, ClaudeQuotaState, CodexQuotaState } from '../src/types';
import { planUnitsFor } from '../src/features/quota/pool/planUnits';
import { formatPoolPercent, formatPoolRate } from '../src/features/quota/pool/poolFormat';
import {
  emptyHistory,
  HISTORY_RETENTION_MS,
  parseHistory,
  pruneHistory,
  recordReading,
  seriesFor,
  historyCoverage,
} from '../src/features/quota/pool/localHistory';
import {
  buildPoolAccount,
  burnRate,
  drainSeries,
  historySamples,
  niceCeiling,
  poolDays,
  poolSpan,
  projectAccount,
  summarizePool,
  HOUR_MS,
  type PoolAccount,
} from '../src/features/quota/pool/poolModel';
import {
  predictNewSessionOrder,
  type RoutingCandidateInput,
} from '../src/features/quota/pool/routingOrder';
import { buildProviderPool } from '../src/features/quota/pool/buildPools';
import { QuotaPoolPanel } from '../src/features/quota/components/QuotaPoolPanel';
import { normalizeRoutingStrategy, readRoutingSettings } from '../src/services/api/routing';
import { parseRoutingStrategy } from '../src/hooks/useVisualConfig';
import { ROUTING_STRATEGIES } from '../src/types/routing';

const NOW = new Date(2026, 10, 5, 12).getTime();
const H = HOUR_MS;

const account = (overrides: Partial<PoolAccount>): PoolAccount => ({
  key: 'a.json',
  label: 'a.json',
  provider: 'codex',
  units: 1,
  planLabel: 'Plus',
  planAssumed: false,
  remaining: 0.5,
  nextResetMs: null,
  periodMs: 168 * H,
  history: [],
  source: 'live',
  ...overrides,
});

describe('base-plan accounting policy', () => {
  test('maps Codex and Claude plans to the configured units', () => {
    const codex = (plan: string) => planUnitsFor('codex', plan).units;
    expect(codex('plus')).toBe(1);
    expect(codex('prolite')).toBe(10);
    expect(codex('pro_lite')).toBe(10);
    expect(codex('pro100')).toBe(10);
    expect(codex('pro')).toBe(20);
    expect(codex('pro200')).toBe(20);
    expect(codex('promax')).toBe(50);
    expect(codex('Pro-Max')).toBe(50);
    expect(codex('pro500')).toBe(50);

    const claude = (plan: string) => planUnitsFor('claude', plan).units;
    expect(claude('plan_pro')).toBe(1);
    expect(claude('plan_max5')).toBe(5);
    expect(claude('default_claude_max_5x')).toBe(5);
    expect(claude('plan_max20')).toBe(10);
    expect(claude('default_claude_max_20x')).toBe(10);
  });

  test('unknown plans count as one unit and are flagged, like the router', () => {
    expect(planUnitsFor('codex', 'team')).toEqual({ units: 1, planLabel: 'team', assumed: true });
    expect(planUnitsFor('codex', null)).toEqual({ units: 1, planLabel: null, assumed: true });
    // An undifferentiated Max is not guessed as 5x or 20x.
    expect(planUnitsFor('claude', 'plan_max')).toMatchObject({ units: 1, assumed: true });
  });

  test('pools sum remaining fraction x units, not percentages', () => {
    const span = { startMs: NOW - 3 * 24 * H, endMs: NOW + 11 * 24 * H };
    const build = (key: string, units: number, usedPercent: number) =>
      buildPoolAccount(
        {
          key,
          label: key,
          provider: 'codex',
          units,
          planLabel: null,
          planAssumed: false,
          weekly: { usedPercent, resetAtMs: NOW + 24 * H, periodHours: 168 },
          history: null,
        },
        NOW
      );
    // 25% of a Pro 200 is 5 units; 40% of a Pro 100 is 4 units.
    const pro200 = build('pro200', 20, 75);
    const pro100 = build('pro100', 10, 60);
    expect(pro200.remaining).toBeCloseTo(5, 9);
    expect(pro100.remaining).toBeCloseTo(4, 9);
    const summary = summarizePool([pro200, pro100], {
      now: NOW,
      span,
      burnWindowHours: 3,
      futureUsage: 'same',
    });
    expect(summary.total).toBeCloseTo(9, 9);
    expect(summary.capacity).toBe(30);
  });
});

describe('browser-local history', () => {
  const weekly = (usedPercent: number, resetAtMs = NOW + 48 * H) => ({
    id: 'weekly',
    usedPercent,
    resetAtMs,
    periodHours: 168,
  });

  test('appends real readings, replaces near-duplicates and never rewrites the past', () => {
    let history = recordReading(emptyHistory(), 'codex', 'a.json', [weekly(10)], NOW - 2 * H);
    history = recordReading(history, 'codex', 'a.json', [weekly(20)], NOW - H);
    history = recordReading(history, 'codex', 'a.json', [weekly(21)], NOW - H + 10_000);
    history = recordReading(history, 'codex', 'a.json', [weekly(5)], NOW - 3 * H);
    const series = seriesFor(history, 'codex', 'a.json', 'weekly');
    expect(series?.windowMinutes).toBe(168 * 60);
    expect(series?.points).toEqual([
      { atMs: NOW - 2 * H, usedPercent: 10, resetAtMs: NOW + 48 * H },
      { atMs: NOW - H + 10_000, usedPercent: 21, resetAtMs: NOW + 48 * H },
    ]);
    expect(history.startedAtMs).toBe(NOW - 2 * H);
  });

  test('ignores windows without a numeric reading', () => {
    const history = emptyHistory();
    const next = recordReading(
      history,
      'claude',
      'c.json',
      [{ id: 'seven-day', usedPercent: null, periodHours: 168 }],
      NOW
    );
    expect(next).toBe(history);
  });

  test('prunes old readings and deleted credentials', () => {
    let history = recordReading(
      emptyHistory(),
      'codex',
      'old.json',
      [weekly(10)],
      NOW - HISTORY_RETENTION_MS - H
    );
    history = recordReading(history, 'codex', 'kept.json', [weekly(10)], NOW - H);
    history = recordReading(history, 'codex', 'gone.json', [weekly(10)], NOW - H);
    const pruned = pruneHistory(history, NOW, new Set(['codex:old.json', 'codex:kept.json']));
    expect(Object.keys(pruned.credentials)).toEqual(['codex:kept.json']);
    expect(historyCoverage(pruned, 'codex', 'weekly')).toEqual({ oldestMs: NOW - H, readings: 1 });
  });

  test('drops malformed storage instead of repairing it', () => {
    expect(parseHistory('not json')).toEqual(emptyHistory());
    expect(parseHistory(JSON.stringify({ version: 2, credentials: {} }))).toEqual(emptyHistory());
    const parsed = parseHistory(
      JSON.stringify({
        version: 1,
        startedAtMs: 5,
        credentials: {
          'codex:a': {
            provider: 'codex',
            series: { weekly: { windowMinutes: 10080, readings: [[2, 3, 0], ['x'], [1, 2, 0]] } },
          },
          'xai:b': { provider: 'xai', series: {} },
        },
      })
    );
    expect(Object.keys(parsed.credentials)).toEqual(['codex:a']);
    expect(parsed.credentials['codex:a'].series.weekly.readings).toEqual([
      [1, 2, 0],
      [2, 3, 0],
    ]);
  });
});

describe('pool accounts', () => {
  test('draws a window that rolled between readings as a vertical reset, in units', () => {
    const samples = historySamples(
      {
        windowMinutes: 300,
        points: [
          { atMs: 1 * H, usedPercent: 100, resetAtMs: 2 * H },
          { atMs: 3 * H, usedPercent: 10, resetAtMs: 7 * H },
        ],
      },
      20
    );
    expect(samples).toEqual([
      { atMs: 1 * H, value: 0 },
      { atMs: 2 * H, value: 0 },
      { atMs: 2 * H, value: 20 },
      { atMs: 3 * H, value: 18 },
    ]);
  });

  test('treats a live window whose reset already passed as full, next reset one period on', () => {
    const built = buildPoolAccount(
      {
        key: 'a',
        label: 'a',
        provider: 'claude',
        units: 5,
        planLabel: 'Max 5x',
        planAssumed: false,
        weekly: { usedPercent: 90, resetAtMs: NOW - H, periodHours: 168 },
        history: null,
      },
      NOW
    );
    expect(built.remaining).toBe(5);
    expect(built.nextResetMs).toBe(NOW - H + 168 * H);
  });

  test('uses a recent local reading when no live read is loaded, but not a stale one', () => {
    const series = {
      windowMinutes: 10080,
      points: [
        { atMs: NOW - 20 * 60_000, usedPercent: 50, resetAtMs: NOW + 48 * H },
        { atMs: NOW - 5 * 60_000, usedPercent: 55, resetAtMs: NOW + 48 * H },
      ],
    };
    const input = {
      key: 'k',
      label: 'k',
      provider: 'codex' as const,
      units: 10,
      planLabel: 'Pro 100',
      planAssumed: false,
      weekly: null,
      history: series,
    };
    const built = buildPoolAccount(input, NOW);
    expect(built).toMatchObject({ remaining: 4.5, nextResetMs: NOW + 48 * H, source: 'history' });
    expect(built.history.at(-1)).toEqual({ atMs: NOW, value: 4.5 });

    const stale = buildPoolAccount(input, NOW + 2 * H);
    expect(stale.remaining).toBeNull();
    expect(stale.source).toBe('none');
  });
});

describe('burn and projection', () => {
  test('counts only consumption, ignoring resets, over the covered part of the window', () => {
    const history = [
      { atMs: NOW - 3 * H, value: 8 },
      { atMs: NOW - 2 * H, value: 7 },
      { atMs: NOW - 2 * H, value: 10 },
      { atMs: NOW, value: 9 },
    ];
    expect(burnRate(history, NOW, 3)).toBeCloseTo(2 / 3, 6);
    expect(burnRate(history, NOW, 1)).toBeCloseTo(0.5, 6);
    expect(
      burnRate(
        [
          { atMs: NOW - 60_000, value: 5 },
          { atMs: NOW, value: 4.9 },
        ],
        NOW,
        3
      )
    ).toBeNull();
    expect(burnRate([], NOW, 3)).toBeNull();
  });

  test('spreads measured spend across a gap between readings', () => {
    // Page closed for 4 hours: the 4 units spent in between still count.
    const history = [
      { atMs: NOW - 6 * H, value: 10 },
      { atMs: NOW - 5 * H, value: 9 },
      { atMs: NOW - 1 * H, value: 5 },
      { atMs: NOW, value: 4 },
    ];
    expect(burnRate(history, NOW, 6)).toBeCloseTo(1, 6);
  });

  test('drains to empty, then refills to the plan allowance at each reset', () => {
    const projection = projectAccount(
      account({ units: 20, remaining: 4, nextResetMs: NOW + 10 * H, periodMs: 24 * H }),
      0.8,
      NOW,
      NOW + 36 * H
    );
    expect(projection.emptyAtMs).toBe(NOW + 5 * H);
    expect(projection.points).toContainEqual({ atMs: NOW + 10 * H, value: 0 });
    expect(projection.points).toContainEqual({ atMs: NOW + 10 * H, value: 20 });
    expect(projection.points).toContainEqual({ atMs: NOW + 34 * H, value: 20 });
    const last = projection.points.at(-1);
    expect(last?.atMs).toBe(NOW + 36 * H);
    expect(last?.value).toBeCloseTo(18.4, 6);
  });

  test('starts the recorded pool line only once every account has a real reading', () => {
    const span = { startMs: NOW - 3 * 24 * H, endMs: NOW + 11 * 24 * H };
    const early = account({
      key: 'early',
      units: 10,
      remaining: 3,
      nextResetMs: NOW + 48 * H,
      history: [
        { atMs: NOW - 3 * H, value: 6 },
        { atMs: NOW, value: 3 },
      ],
    });
    const late = account({
      key: 'late',
      units: 1,
      remaining: 0.8,
      nextResetMs: NOW + 48 * H + 30_000,
      history: [
        { atMs: NOW - H, value: 0.8 },
        { atMs: NOW, value: 0.8 },
      ],
    });
    const summary = summarizePool([early, late], {
      now: NOW,
      span,
      burnWindowHours: 3,
      futureUsage: 'same',
    });
    expect(summary.total).toBeCloseTo(3.8, 9);
    expect(summary.capacity).toBe(11);
    expect(summary.recordedStartMs).toBe(NOW - H);
    expect(summary.recorded[0]).toEqual({ atMs: NOW - H, value: 4 + 0.8 });
    expect(summary.burnPerHour).toBeCloseTo(1, 6);
    expect(summary.firstEmpty?.account.key).toBe('early');
    expect(summary.firstEmpty?.atMs).toBe(NOW + 3 * H);
    // Both resets fall within a minute: early refills 10, late refills 0.2.
    expect(summary.nextReset?.atMs).toBe(NOW + 48 * H);
    expect(summary.nextReset?.added).toBeCloseTo(10 + 0.2, 6);

    const none = summarizePool([early, late], {
      now: NOW,
      span,
      burnWindowHours: 3,
      futureUsage: 'none',
    });
    expect(none.firstEmpty).toBeNull();
  });

  test('draws no recorded line while an account has no readings', () => {
    const span = { startMs: NOW - 3 * 24 * H, endMs: NOW + 11 * 24 * H };
    const summary = summarizePool(
      [
        account({
          key: 'a',
          history: [
            { atMs: NOW - H, value: 1 },
            { atMs: NOW, value: 0.5 },
          ],
        }),
        account({ key: 'b', history: [] }),
      ],
      { now: NOW, span, burnWindowHours: 3, futureUsage: 'same' }
    );
    expect(summary.recorded).toEqual([]);
    expect(summary.recordedStartMs).toBeNull();
    expect(summary.projected.length).toBeGreaterThan(0);
  });

  test('derives drain-rate steps from the recorded and projected pool', () => {
    const drain = drainSeries(
      [
        { atMs: 0, value: 10 },
        { atMs: H, value: 9 },
        { atMs: H, value: 19 },
        { atMs: 2 * H, value: 18.5 },
      ],
      [
        { atMs: 2 * H, value: 18.5 },
        { atMs: 4 * H, value: 17.5 },
      ]
    );
    expect(drain.recorded.map((point) => point.value)).toEqual([1, 1, 0.5, 0.5]);
    expect(drain.projected).toEqual([
      { atMs: 2 * H, value: 0.5 },
      { atMs: 4 * H, value: 0.5 },
    ]);
    expect(niceCeiling(7.3)).toBe(10);
    expect(niceCeiling(0)).toBe(1);
  });

  test('spans two weeks starting three days before today, stepping by weeks', () => {
    const span = poolSpan(0, NOW);
    expect(new Date(span.startMs).getDate()).toBe(2);
    expect(poolDays(span)).toHaveLength(14);
    expect(poolSpan(-1, NOW).endMs).toBe(new Date(2026, 10, 9).getTime());
  });
});

describe('new-session order (soonest-reset-first)', () => {
  type CandidateOptions = Partial<RoutingCandidateInput> & {
    weeklyUsed?: number;
    weeklyResetMs?: number | null;
    fiveHourUsed?: number;
    manual?: number[];
  };
  const candidate = (key: string, options: CandidateOptions = {}): RoutingCandidateInput => {
    const {
      weeklyUsed = 50,
      weeklyResetMs = NOW + 72 * H,
      fiveHourUsed = 0,
      manual = [],
      ...overrides
    } = options;
    return {
      key,
      label: key,
      units: 1,
      planLabel: null,
      planAssumed: true,
      disabled: false,
      priority: 0,
      read: {
        observedAtMs: NOW - 60_000,
        weeklyWindowId: 'weekly',
        windows: [
          { id: 'five-hour', usedPercent: fiveHourUsed, resetAtMs: NOW + 2 * H },
          { id: 'weekly', usedPercent: weeklyUsed, resetAtMs: weeklyResetMs },
        ],
        manualResetExpiriesMs: manual,
      },
      ...overrides,
    };
  };

  test('earliest weekly reset wins over more remaining allowance', () => {
    const order = predictNewSessionOrder(
      [
        candidate('later-big', { units: 50, weeklyUsed: 0, weeklyResetMs: NOW + 96 * H }),
        candidate('sooner-small', { units: 1, weeklyUsed: 90, weeklyResetMs: NOW + 24 * H }),
      ],
      NOW
    );
    expect(order.map((entry) => [entry.key, entry.position])).toEqual([
      ['sooner-small', 1],
      ['later-big', 2],
    ]);
    expect(order[0].deadlineSource).toBe('weekly');
  });

  test('an unspent manual-reset credit expiring sooner counts as the deadline', () => {
    const order = predictNewSessionOrder(
      [
        candidate('weekly-soon', { weeklyResetMs: NOW + 24 * H }),
        candidate('credit-sooner', { weeklyResetMs: NOW + 120 * H, manual: [NOW + 6 * H] }),
        candidate('credit-expired', { weeklyResetMs: NOW + 120 * H, manual: [NOW - H] }),
      ],
      NOW
    );
    expect(order.map((entry) => entry.key)).toEqual([
      'credit-sooner',
      'weekly-soon',
      'credit-expired',
    ]);
    expect(order[0]).toMatchObject({ deadlineMs: NOW + 6 * H, deadlineSource: 'manual-reset' });
  });

  test('equal deadlines prefer more remaining base-plan units (docs example)', () => {
    // 25% of a 20-unit plan (5 units) beats 40% of a 10-unit plan (4 units).
    const order = predictNewSessionOrder(
      [
        candidate('pro100-40pct', { units: 10, weeklyUsed: 60 }),
        candidate('pro200-25pct', { units: 20, weeklyUsed: 75 }),
      ],
      NOW
    );
    expect(order[0].key).toBe('pro200-25pct');
    expect(order[0].remainingUnits).toBeCloseTo(5, 9);
    expect(order[1].remainingUnits).toBeCloseTo(4, 9);
  });

  test('five-minute-old readings stop contributing a routing estimate', () => {
    const stale = candidate('stale');
    stale.read!.observedAtMs = NOW - 5 * 60_000;
    const order = predictNewSessionOrder([stale, candidate('fresh')], NOW);
    expect(order[0]).toMatchObject({ key: 'fresh', status: 'ranked', position: 1 });
    expect(order[1]).toMatchObject({ key: 'stale', status: 'unread', position: null });
  });

  test('blocked, disabled, lower-priority and unread accounts get no position', () => {
    const order = predictNewSessionOrder(
      [
        candidate('ok'),
        candidate('short-window-out', { fiveHourUsed: 100, weeklyResetMs: NOW + H }),
        candidate('off', { disabled: true }),
        candidate('backup', { priority: -1 }),
        { ...candidate('fresh'), read: null },
        candidate('no-reset', { weeklyResetMs: null }),
      ],
      NOW
    );
    const byKey = Object.fromEntries(order.map((entry) => [entry.key, entry]));
    expect(byKey.ok).toMatchObject({ status: 'ranked', position: 1 });
    expect(byKey['short-window-out']).toMatchObject({
      status: 'blocked',
      position: null,
      blockedUntilMs: NOW + 2 * H,
    });
    expect(byKey.off.status).toBe('disabled');
    expect(byKey.backup.status).toBe('lower-priority');
    expect(byKey.fresh.status).toBe('unread');
    expect(byKey['no-reset'].status).toBe('no-deadline');
    expect(order[0].key).toBe('ok');
    expect(order[1].key).toBe('no-reset');
  });
});

describe('provider pool glue', () => {
  const file = (name: string, extra: Partial<AuthFileItem> = {}): AuthFileItem =>
    ({ name, type: 'codex', provider: 'codex', ...extra }) as AuthFileItem;
  const codexState = (
    planType: string,
    usedPercent: number,
    resetAtMs: number
  ): CodexQuotaState => ({
    status: 'success',
    planType,
    windows: [
      {
        id: 'five-hour',
        label: '5h',
        usedPercent: 0,
        resetLabel: '',
        resetAtMs: NOW + H,
        periodHours: 5,
      },
      { id: 'weekly', label: '7d', usedPercent, resetLabel: '', resetAtMs, periodHours: 168 },
    ],
    rateLimitResetCredits: [
      {
        id: 'c1',
        status: 'available',
        grantedAt: '',
        expiresAt: new Date(NOW + 3 * H).toISOString(),
      },
    ],
  });

  test('weights accounts by plan and ranks with reset credits', () => {
    let history = emptyHistory();
    const a = codexState('pro', 75, NOW + 48 * H);
    const b: CodexQuotaState = {
      ...codexState('plus', 10, NOW + 24 * H),
      rateLimitResetCredits: [],
    };
    history = recordReading(history, 'codex', 'a.json', a.windows, NOW - 60_000);
    history = recordReading(history, 'codex', 'b.json', b.windows, NOW - 60_000);
    const pool = buildProviderPool(
      'codex',
      [
        { key: 'a.json', label: 'a.json', file: file('a.json'), quota: a },
        { key: 'b.json', label: 'b.json', file: file('b.json'), quota: b },
        {
          key: 'c.json',
          label: 'c.json',
          file: file('c.json', { disabled: true }),
          quota: undefined,
        },
      ],
      history,
      { now: NOW, span: poolSpan(0, NOW), burnWindowHours: 3, futureUsage: 'same' }
    );
    expect(pool.accountCount).toBe(2);
    expect(pool.summary.total).toBeCloseTo(5 + 0.9, 9);
    expect(pool.summary.capacity).toBe(21);
    // a's reset credit expires in 3h, before b's weekly reset in 24h.
    expect(pool.routing.map((entry) => [entry.key, entry.status])).toEqual([
      ['a.json', 'ranked'],
      ['b.json', 'ranked'],
      ['c.json', 'disabled'],
    ]);
    expect(pool.lastReadMs).toBe(NOW - 60_000);
  });

  test('a Claude read made before local recording still ranks with its own timestamp', () => {
    const state: ClaudeQuotaState = {
      status: 'success',
      planType: 'plan_max20',
      observedAt: new Date(NOW - 2 * 60_000).toISOString(),
      windows: [
        {
          id: 'seven-day',
          label: '7d',
          usedPercent: 50,
          resetLabel: '',
          resetAtMs: NOW + 24 * H,
          periodHours: 168,
        },
      ],
    };
    const pool = buildProviderPool(
      'claude',
      [{ key: 'c.json', label: 'c.json', file: file('c.json', { type: 'claude' }), quota: state }],
      emptyHistory(),
      { now: NOW, span: poolSpan(0, NOW), burnWindowHours: 3, futureUsage: 'same' }
    );
    expect(pool.summary.total).toBeCloseTo(5, 9);
    expect(pool.routing[0]).toMatchObject({ status: 'ranked', observedAtMs: NOW - 2 * 60_000 });
  });
});

describe('routing strategy parsing keeps soonest-reset-first', () => {
  test('affinity is enabled even when the stored flag is false', () => {
    expect(
      readRoutingSettings({
        routing: { strategy: 'soonest-reset-first', 'session-affinity': false },
      }).sessionAffinity
    ).toBe(true);
  });
  test('both config readers recognise the strategy instead of coercing it to round-robin', () => {
    expect(ROUTING_STRATEGIES).toContain('soonest-reset-first');
    expect(normalizeRoutingStrategy('soonest-reset-first')).toBe('soonest-reset-first');
    expect(normalizeRoutingStrategy('soonest_reset_first')).toBe('soonest-reset-first');
    expect(parseRoutingStrategy('soonest-reset-first')).toBe('soonest-reset-first');
    expect(normalizeRoutingStrategy('bogus')).toBe('round-robin');
  });
});

describe('pool panel', () => {
  const originalLanguage = i18n.language;
  beforeAll(async () => {
    await i18n.changeLanguage('en');
  });
  afterAll(async () => {
    await i18n.changeLanguage(originalLanguage);
  });

  const span = { startMs: NOW - 3 * 24 * H, endMs: NOW + 11 * 24 * H };
  const pool = (overrides: Partial<PoolAccount>[] = []) => {
    const accounts = [
      account({
        key: 'first.json',
        label: 'first.json',
        units: 20,
        planLabel: 'Pro 200',
        remaining: 5,
        nextResetMs: NOW + 48 * H,
        history: [
          { atMs: NOW - 3 * H, value: 8 },
          { atMs: NOW, value: 5 },
        ],
        ...overrides[0],
      }),
      account({
        key: 'second.json',
        label: 'second.json',
        units: 10,
        planLabel: 'Pro 100',
        remaining: 4,
        nextResetMs: NOW + 72 * H,
        history: [
          { atMs: NOW - 3 * H, value: 4 },
          { atMs: NOW, value: 4 },
        ],
        ...overrides[1],
      }),
    ];
    return {
      provider: 'codex' as const,
      accountCount: 2,
      summary: summarizePool(accounts, { now: NOW, span, burnWindowHours: 3, futureUsage: 'same' }),
      routing: predictNewSessionOrder(
        accounts.map((item) => ({
          key: item.key,
          label: item.label,
          units: item.units,
          planLabel: item.planLabel,
          planAssumed: item.planAssumed,
          disabled: false,
          priority: 0,
          read: {
            observedAtMs: NOW - 60_000,
            weeklyWindowId: 'weekly',
            windows: [
              {
                id: 'weekly',
                usedPercent: 100 - ((item.remaining ?? 0) / item.units) * 100,
                resetAtMs: item.nextResetMs,
              },
            ],
            manualResetExpiriesMs: [],
          },
        })),
        NOW
      ),
      lastReadMs: NOW - 60_000,
      oldestReadMs: NOW - 60_000,
    };
  };
  const props = {
    providerLabel: 'Codex',
    accent: '#6e9ef7',
    pool: pool(),
    mode: 'remaining' as const,
    burnWindowHours: 3,
    futureUsageLabel: 'Same as now',
    span,
    days: poolDays(span),
    now: NOW,
    showAccountLines: false,
    history: { oldestMs: NOW - 3 * H, readings: 4, startedAtMs: NOW - 3 * H },
    routingStrategy: 'soonest-reset-first',
  };

  test('renders percentages against the largest plan with compact labels', () => {
    const markup = renderToStaticMarkup(createElement(QuotaPoolPanel, props));
    expect(markup).toContain('>45%<');
    expect(markup).toContain('/ 150%');
    expect(markup).toContain('100% = Pro 200');
    expect(markup).toContain('2 accounts');
    expect(markup).toContain('5%/h');
    expect(markup).toContain('role="img"');
    expect(markup).toContain('recorded');
    expect(markup).toContain('projected');
    expect(markup).not.toContain('units left');
    expect(markup).not.toContain('The server keeps no quota history');
  });

  test('removes the numbered account-order list and its explanation', () => {
    const markup = renderToStaticMarkup(createElement(QuotaPoolPanel, props));
    expect(markup).not.toContain('New-session order');
    expect(markup).not.toContain('#1');
    expect(markup).not.toContain('Existing sessions stay on their account');
    expect(markup).not.toContain('aria-current="true"');
  });

  test('recalculates the reference when a larger plan is present', () => {
    const current = pool();
    const bigger = { ...current.routing[0], key: 'largest', units: 50, planLabel: 'Pro 500' };
    const markup = renderToStaticMarkup(
      createElement(QuotaPoolPanel, {
        ...props,
        pool: { ...current, routing: [...current.routing, bigger] },
      })
    );
    expect(markup).toContain('>18%<');
    expect(markup).toContain('/ 60%');
    expect(markup).toContain('100% = Pro 500');
  });

  test('leaves unloaded pools concise', () => {
    const unloaded = { remaining: null, source: 'none' as const, history: [] };
    const markup = renderToStaticMarkup(
      createElement(QuotaPoolPanel, {
        ...props,
        pool: pool([unloaded, unloaded]),
        history: { oldestMs: null, readings: 0, startedAtMs: null },
      })
    );
    expect(markup).toContain('Quota not read yet');
    expect(markup).not.toContain('No readings recorded in this browser yet');
    expect(markup).not.toContain('role="img"');
  });
});

describe('highest-plan percentage formatting', () => {
  test('weights lower plans relative to the largest subscription', () => {
    expect(formatPoolPercent(49.5 + 9, 50)).toBe('117%');
    expect(formatPoolPercent(50 + 10, 50)).toBe('120%');
    expect(formatPoolPercent(9, 20)).toBe('45%');
    expect(formatPoolRate(0.02, 50)).toBe('0.04%/h');
  });
});
