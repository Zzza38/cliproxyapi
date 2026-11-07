import { afterEach, describe, expect, spyOn, test } from 'bun:test';
import type { AuthFileItem, CodexQuotaState } from '../src/types';
import { apiClient } from '../src/services/api/client';
import {
  QUOTA_HISTORY_PATH,
  quotaHistoryApi,
  TOKEN_USAGE_PATH,
  tokenUsageApi,
} from '../src/services/api/quotaHistory';
import { parseTokenUsage } from '../src/features/quota/pool/tokenUsage';
import {
  emptyHistory,
  MAX_READINGS_PER_SERIES,
  recordReading,
  seriesFor,
  type LocalQuotaHistory,
  type StoredReading,
} from '../src/features/quota/pool/localHistory';
import {
  buildImportPayload,
  IMPORT_MAX_ROWS,
  mergeHistories,
  parseServerHistory,
} from '../src/features/quota/pool/serverHistory';
import {
  MAX_BACKOFF_MS,
  POLL_INTERVAL_MS,
  QuotaHistorySync,
  type HistorySyncDeps,
} from '../src/features/quota/pool/historySync';
import {
  buildPoolAccount,
  burnRate,
  HOUR_MS,
  poolSpan,
  summarizePool,
} from '../src/features/quota/pool/poolModel';
import { buildProviderPool } from '../src/features/quota/pool/buildPools';

const NOW = new Date(2026, 10, 5, 12).getTime();
const H = HOUR_MS;
const MIN = 60_000;
const WEEK_MIN = 10080;

const weekly = (readings: StoredReading[], windowMinutes = WEEK_MIN) => ({
  windowMinutes,
  readings,
});

const serverPayload = (
  credentials: LocalQuotaHistory['credentials'],
  startedAtMs: number | null = null
) => ({ enabled: true, version: 1, startedAtMs, credentials });

/** One reading per minute for `count` minutes ending at `endMs`. */
const minuteReadings = (count: number, endMs: number, used = 10): StoredReading[] =>
  Array.from({ length: count }, (_, index) => [
    endMs - (count - 1 - index) * MIN,
    used,
    endMs + 24 * H,
  ]);

describe('server history response', () => {
  test('enabled:false and unrecognised payloads are told apart', () => {
    expect(parseServerHistory({ enabled: false }, NOW)).toEqual({ enabled: false });
    expect(parseServerHistory('<html>', NOW)).toBeNull();
    expect(parseServerHistory({ version: 1, credentials: {} }, NOW)).toBeNull();
    expect(parseServerHistory({ enabled: true, version: 2, credentials: {} }, NOW)).toBeNull();
  });

  test('keeps only weekly series of correctly prefixed credentials and drops junk', () => {
    const parsed = parseServerHistory(
      serverPayload({
        'codex:a.json': {
          provider: 'codex',
          series: {
            weekly: weekly([
              [NOW - 2 * MIN, 120, NOW + H],
              [NOW - 3 * MIN, 10, 0],
              ['bad', 1, 2] as unknown as StoredReading,
            ]),
            'five-hour': weekly([[NOW, 1, 0]], 300),
          },
        },
        'claude:b.json': {
          provider: 'codex',
          series: { weekly: weekly([[NOW, 1, 0]]) },
        },
        'claude:c.json': {
          provider: 'claude',
          series: { 'seven-day': weekly([[NOW - 29 * 24 * H, 1, 0]]) },
        },
      }),
      NOW
    );
    expect(parsed?.enabled).toBe(true);
    if (!parsed?.enabled) return;
    expect(Object.keys(parsed.history.credentials)).toEqual(['codex:a.json']);
    expect(parsed.history.credentials['codex:a.json'].series).toEqual({
      weekly: weekly([
        [NOW - 3 * MIN, 10, 0],
        [NOW - 2 * MIN, 100, NOW + H],
      ]),
    });
  });

  test('28 days of minute samples survive parsing and merging untruncated', () => {
    const readings = minuteReadings(40_320, NOW);
    const parsed = parseServerHistory(
      serverPayload({
        'codex:a.json': { provider: 'codex', series: { weekly: weekly(readings) } },
      }),
      NOW
    );
    if (!parsed?.enabled) throw new Error('expected enabled history');
    const merged = mergeHistories(emptyHistory(), parsed.history);
    const series = seriesFor(merged, 'codex', 'a.json', 'weekly');
    expect(series?.points.length).toBe(40_320);
    expect(series!.points.length).toBeGreaterThan(MAX_READINGS_PER_SERIES);
    expect(series?.points[0].atMs).toBe(NOW - 40_319 * MIN);
  });
});

describe('merging browser and server history', () => {
  test('server wins equal timestamps; browser-only readings and windows stay', () => {
    let local = emptyHistory();
    local = recordReading(
      local,
      'codex',
      'a.json',
      [
        { id: 'weekly', usedPercent: 40, resetAtMs: NOW + H, periodHours: 168 },
        { id: 'five-hour', usedPercent: 5, resetAtMs: NOW + H, periodHours: 5 },
      ],
      NOW - 10 * MIN
    );
    local = recordReading(
      local,
      'codex',
      'a.json',
      [{ id: 'weekly', usedPercent: 45, resetAtMs: NOW + H, periodHours: 168 }],
      NOW - 2 * MIN
    );
    const server: LocalQuotaHistory = {
      version: 1,
      startedAtMs: NOW - 3 * H,
      credentials: {
        'codex:a.json': {
          provider: 'codex',
          series: {
            weekly: weekly([
              [NOW - 3 * H, 20, NOW + H],
              [NOW - 10 * MIN, 41, NOW + H],
              [NOW - MIN, 46, NOW + H],
            ]),
          },
        },
        'codex:z.json': { provider: 'codex', series: { weekly: weekly([[NOW, 1, 0]]) } },
      },
    };
    const merged = mergeHistories(local, server);
    expect(merged.credentials['codex:a.json'].series.weekly.readings).toEqual([
      [NOW - 3 * H, 20, NOW + H],
      [NOW - 10 * MIN, 41, NOW + H],
      [NOW - 2 * MIN, 45, NOW + H],
      [NOW - MIN, 46, NOW + H],
    ]);
    expect(merged.credentials['codex:a.json'].series['five-hour']).toBe(
      local.credentials['codex:a.json'].series['five-hour']
    );
    expect(merged.credentials['codex:z.json']).toBe(server.credentials['codex:z.json']);
    expect(merged.startedAtMs).toBe(NOW - 3 * H);
    // Neither input is modified.
    expect(local.credentials['codex:a.json'].series.weekly.readings).toHaveLength(2);
    expect(mergeHistories(local, null)).toBe(local);
  });
});

describe('browser history import payload', () => {
  test('sends weekly readings of live credentials only and leaves the log alone', () => {
    let local = emptyHistory();
    local = recordReading(
      local,
      'codex',
      'a.json',
      [
        { id: 'weekly', usedPercent: 40, resetAtMs: NOW + H, periodHours: 168 },
        { id: 'five-hour', usedPercent: 5, resetAtMs: NOW + H, periodHours: 5 },
      ],
      NOW - 10 * MIN
    );
    local = recordReading(
      local,
      'claude',
      'c.json',
      [{ id: 'seven-day', usedPercent: 10, resetAtMs: NOW + H, periodHours: 168 }],
      NOW - 5 * MIN
    );
    local = recordReading(
      local,
      'codex',
      'deleted.json',
      [{ id: 'weekly', usedPercent: 1, resetAtMs: NOW + H, periodHours: 168 }],
      NOW - 5 * MIN
    );
    const before = JSON.stringify(local);
    const payload = buildImportPayload(local, new Set(['codex:a.json', 'claude:c.json']), NOW);
    expect(payload).toEqual({
      version: 1,
      startedAtMs: NOW - 10 * MIN,
      credentials: {
        'codex:a.json': {
          provider: 'codex',
          series: { weekly: weekly([[NOW - 10 * MIN, 40, NOW + H]]) },
        },
        'claude:c.json': {
          provider: 'claude',
          series: { 'seven-day': weekly([[NOW - 5 * MIN, 10, NOW + H]]) },
        },
      },
    });
    expect(JSON.stringify(local)).toBe(before);
    expect(buildImportPayload(local, new Set(), NOW)).toBeNull();
  });

  test('keeps the newest rows within the row limit, in time order', () => {
    // Three credentials x 40,000 minutes = 120,000 rows, over the 100,000 limit.
    const credentials: LocalQuotaHistory['credentials'] = {};
    ['a', 'b', 'c'].forEach((name) => {
      credentials[`codex:${name}.json`] = {
        provider: 'codex',
        series: { weekly: weekly(minuteReadings(40_000, NOW)) },
      };
    });
    const local: LocalQuotaHistory = { version: 1, startedAtMs: null, credentials };
    const payload = buildImportPayload(
      local,
      new Set(['codex:a.json', 'codex:b.json', 'codex:c.json']),
      NOW
    );
    const sent = Object.values(payload?.credentials ?? {}).map(
      (credential) => credential.series.weekly.readings
    );
    expect(sent.reduce((sum, readings) => sum + readings.length, 0)).toBe(IMPORT_MAX_ROWS);
    sent.forEach((readings) => {
      expect(readings.at(-1)?.[0]).toBe(NOW);
      expect(readings.every((reading, i) => i === 0 || reading[0] > readings[i - 1][0])).toBe(true);
    });
    expect(JSON.stringify(payload).length).toBeLessThan(8 * 1024 * 1024);
    expect(local.credentials['codex:a.json'].series.weekly.readings).toHaveLength(40_000);
  });
});

/* ------------------------------------------------------------- sync loop */

interface Pending<T> {
  resolve(value: T): void;
  reject(error: unknown): void;
  signal: AbortSignal;
  payload?: LocalQuotaHistory;
}

function harness(local: LocalQuotaHistory = emptyHistory(), withTokens = false) {
  let revision = 1;
  let clock = NOW;
  const timers = new Map<number, { callback: () => void; at: number }>();
  let nextTimer = 1;
  const gets: Pending<unknown>[] = [];
  const posts: Pending<unknown>[] = [];
  const tokenGets: Pending<unknown>[] = [];
  const state = { connected: true, visible: true, local };
  const deps: HistorySyncDeps = {
    getRevision: () => revision,
    isConnected: () => state.connected,
    isVisible: () => state.visible,
    fetchHistory: (signal) =>
      new Promise((resolve, reject) => gets.push({ resolve, reject, signal })),
    importHistory: (payload, signal) =>
      new Promise((resolve, reject) => posts.push({ resolve, reject, signal, payload })),
    fetchTokens: withTokens
      ? (signal) => new Promise((resolve, reject) => tokenGets.push({ resolve, reject, signal }))
      : undefined,
    getLocalHistory: () => state.local,
    now: () => clock,
    setTimer: (callback, delayMs) => {
      const id = nextTimer++;
      timers.set(id, { callback, at: clock + delayMs });
      return id;
    },
    clearTimer: (handle) => {
      timers.delete(handle as number);
    },
  };
  const sync = new QuotaHistorySync(deps);
  const flush = async () => {
    for (let i = 0; i < 10; i += 1) await Promise.resolve();
  };
  /** Advance the fake clock, firing due timers. */
  const advance = async (ms: number) => {
    const target = clock + ms;
    for (;;) {
      const due = [...timers.entries()]
        .filter(([, timer]) => timer.at <= target)
        .sort((a, b) => a[1].at - b[1].at)[0];
      if (!due) break;
      timers.delete(due[0]);
      clock = Math.max(clock, due[1].at);
      due[1].callback();
      await flush();
    }
    clock = target;
  };
  return {
    sync,
    gets,
    posts,
    tokenGets,
    state,
    timers,
    advance,
    flush,
    setRevision: (next: number) => {
      revision = next;
    },
  };
}

const enabledResponse = (used: number) =>
  serverPayload({
    'codex:a.json': {
      provider: 'codex',
      series: { weekly: weekly([[NOW - MIN, used, NOW + 24 * H]]) },
    },
  });

const localWithReading = () =>
  recordReading(
    emptyHistory(),
    'codex',
    'a.json',
    [{ id: 'weekly', usedPercent: 30, resetAtMs: NOW + 24 * H, periodHours: 168 }],
    NOW - 30 * MIN
  );

describe('server history sync', () => {
  test('imports once per connection, then fetches and polls once a minute', async () => {
    const h = harness(localWithReading());
    const release = h.sync.acquire();
    h.sync.setKnownCredentials(1, new Set(['codex:a.json']));
    await h.advance(0);
    expect(h.posts).toHaveLength(1);
    expect(h.gets).toHaveLength(0);
    expect(h.posts[0].payload?.credentials['codex:a.json']).toBeDefined();
    h.posts[0].resolve({ imported: 1 });
    await h.flush();
    expect(h.gets).toHaveLength(1);
    h.gets[0].resolve(enabledResponse(31));
    await h.flush();
    expect(h.sync.getSnapshot().enabled).toBe(true);
    expect(h.sync.getSnapshot().history?.credentials['codex:a.json']).toBeDefined();

    // A second consumer does not start a second loop.
    const releaseSecond = h.sync.acquire();
    await h.advance(POLL_INTERVAL_MS - 1);
    expect(h.gets).toHaveLength(1);
    await h.advance(1);
    expect(h.gets).toHaveLength(2);
    expect(h.posts).toHaveLength(1);
    h.gets[1].resolve(enabledResponse(32));
    await h.flush();
    await h.advance(POLL_INTERVAL_MS);
    expect(h.gets).toHaveLength(3);
    expect(h.posts).toHaveLength(1);

    releaseSecond();
    release();
    expect(h.gets[2].signal.aborted).toBe(true);
    expect(h.timers.size).toBe(0);
  });

  test('a connection change aborts, drops late responses and re-imports even after A→B→A', async () => {
    const h = harness(localWithReading());
    h.sync.acquire();
    h.sync.setKnownCredentials(1, new Set(['codex:a.json']));
    await h.advance(0);
    h.posts[0].resolve({ imported: 1 });
    await h.flush();
    h.gets[0].resolve(enabledResponse(31));
    await h.flush();
    expect(h.sync.getSnapshot().history).not.toBeNull();

    // Switch away: the held history disappears for the new revision at once.
    h.setRevision(2);
    expect(h.sync.getSnapshot().history).toBeNull();
    h.sync.checkConnection();
    await h.advance(0);
    // No credential list for revision 2 yet: fetch only, no import.
    expect(h.posts).toHaveLength(1);
    expect(h.gets).toHaveLength(2);
    const staleGet = h.gets[1];

    // Back to the first server (new revision 3) while revision 2's request is pending.
    h.setRevision(3);
    h.sync.checkConnection();
    expect(staleGet.signal.aborted).toBe(true);
    staleGet.resolve(enabledResponse(99));
    await h.flush();
    expect(h.sync.getSnapshot().history).toBeNull();

    // Old-revision credential lists are ignored; the current one triggers a fresh import.
    h.sync.setKnownCredentials(2, new Set(['codex:a.json']));
    h.sync.setKnownCredentials(3, new Set(['codex:a.json']));
    await h.advance(0);
    expect(h.posts.length).toBe(2);
    expect(h.gets.length).toBe(2);
    h.posts[1].resolve({ imported: 0 });
    await h.flush();
    h.gets.at(-1)?.resolve(enabledResponse(41));
    await h.flush();
    expect(
      h.sync.getSnapshot().history?.credentials['codex:a.json'].series.weekly.readings[0][1]
    ).toBe(41);
  });

  test('a credential list arriving during the first fetch imports right after it', async () => {
    const h = harness(localWithReading());
    h.sync.acquire();
    await h.advance(0);
    expect(h.gets).toHaveLength(1);
    h.sync.setKnownCredentials(1, new Set(['codex:a.json']));
    expect(h.posts).toHaveLength(0);
    h.gets[0].resolve(enabledResponse(31));
    await h.flush();
    await h.advance(0);
    expect(h.posts).toHaveLength(1);
  });

  test('a rejected import is not retried; the fetch still runs', async () => {
    const h = harness(localWithReading());
    h.sync.acquire();
    h.sync.setKnownCredentials(1, new Set(['codex:a.json']));
    await h.advance(0);
    h.posts[0].reject(Object.assign(new Error('too large'), { status: 413 }));
    await h.flush();
    expect(h.gets).toHaveLength(1);
    h.gets[0].resolve(enabledResponse(31));
    await h.flush();
    await h.advance(3 * POLL_INTERVAL_MS);
    expect(h.posts).toHaveLength(1);
    expect(h.sync.getSnapshot().enabled).toBe(true);
  });

  test('a missing endpoint stops requests quietly for that connection', async () => {
    const h = harness();
    h.sync.acquire();
    await h.advance(0);
    h.gets[0].reject(Object.assign(new Error('not found'), { status: 404 }));
    await h.flush();
    await h.advance(60 * POLL_INTERVAL_MS);
    expect(h.gets).toHaveLength(1);
    expect(h.sync.getSnapshot()).toMatchObject({ enabled: null, history: null });

    h.setRevision(2);
    h.sync.checkConnection();
    await h.advance(0);
    expect(h.gets).toHaveLength(2);
  });

  test('enabled:false keeps the local fallback, skips imports and backs off', async () => {
    const h = harness(localWithReading());
    h.sync.acquire();
    await h.advance(0);
    h.gets[0].resolve({ enabled: false });
    await h.flush();
    expect(h.sync.getSnapshot()).toMatchObject({ enabled: false, history: null });
    h.sync.setKnownCredentials(1, new Set(['codex:a.json']));
    await h.advance(POLL_INTERVAL_MS);
    expect(h.gets).toHaveLength(1);
    await h.advance(POLL_INTERVAL_MS);
    expect(h.gets).toHaveLength(2);
    expect(h.posts).toHaveLength(0);
    h.gets[1].reject(new Error('network'));
    await h.flush();
    await h.advance(MAX_BACKOFF_MS);
    expect(h.gets.length).toBe(3);
    expect(h.posts).toHaveLength(0);
  });

  test('does not request while hidden or disconnected', async () => {
    const h = harness();
    h.state.visible = false;
    h.sync.acquire();
    await h.advance(5 * POLL_INTERVAL_MS);
    expect(h.gets).toHaveLength(0);
    h.state.visible = true;
    h.sync.wake();
    await h.advance(0);
    expect(h.gets).toHaveLength(1);
  });
});

const tokenResponse = (total: number) => ({
  enabled: true,
  fromMs: NOW - 24 * H,
  toMs: NOW,
  providers: {
    codex: {
      requests: 3,
      inputTokens: total - 10,
      outputTokens: 10,
      cacheReadTokens: 5,
      cacheWriteTokens: 1,
      totalTokens: total,
    },
  },
});

describe('token totals', () => {
  test('parses totals as sent and rejects unknown shapes', () => {
    expect(parseTokenUsage({ enabled: false })).toEqual({ enabled: false });
    expect(parseTokenUsage({ enabled: true, providers: {} })).toBeNull();
    expect(parseTokenUsage(null)).toBeNull();
    expect(
      parseTokenUsage({
        enabled: true,
        fromMs: 1,
        toMs: 2,
        providers: { claude: { inputTokens: 7, totalTokens: 9, outputTokens: -1 }, bad: 3 },
      })
    ).toEqual({
      enabled: true,
      usage: {
        fromMs: 1,
        toMs: 2,
        providers: {
          claude: {
            requests: 0,
            inputTokens: 7,
            outputTokens: 0,
            cacheReadTokens: 0,
            cacheWriteTokens: 0,
            totalTokens: 9,
          },
        },
      },
    });
  });

  test('poll with history once a minute and drop on connection change', async () => {
    const h = harness(emptyHistory(), true);
    h.sync.acquire();
    await h.advance(0);
    h.gets[0].resolve(enabledResponse(31));
    await h.flush();
    expect(h.tokenGets).toHaveLength(1);
    h.tokenGets[0].resolve(tokenResponse(100));
    await h.flush();
    expect(h.sync.getSnapshot().tokens?.providers.codex.totalTokens).toBe(100);
    await h.advance(POLL_INTERVAL_MS);
    expect(h.gets).toHaveLength(2);
    h.gets[1].resolve(enabledResponse(32));
    await h.flush();
    expect(h.tokenGets).toHaveLength(2);
    const stale = h.tokenGets[1];

    h.setRevision(2);
    expect(h.sync.getSnapshot().tokens).toBeNull();
    h.sync.checkConnection();
    expect(stale.signal.aborted).toBe(true);
    stale.resolve(tokenResponse(999));
    await h.flush();
    expect(h.sync.getSnapshot().tokens).toBeNull();
  });

  test('a missing token endpoint stops only token requests', async () => {
    const h = harness(emptyHistory(), true);
    h.sync.acquire();
    await h.advance(0);
    h.gets[0].resolve(enabledResponse(31));
    await h.flush();
    h.tokenGets[0].reject(Object.assign(new Error('not found'), { status: 404 }));
    await h.flush();
    for (let i = 0; i < 3; i += 1) {
      await h.advance(POLL_INTERVAL_MS);
      h.gets.at(-1)?.resolve(enabledResponse(32));
      await h.flush();
    }
    expect(h.gets).toHaveLength(4);
    expect(h.tokenGets).toHaveLength(1);
    expect(h.sync.getSnapshot().tokens).toBeNull();
    expect(h.sync.getSnapshot().enabled).toBe(true);
  });

  test('a missing history endpoint keeps token polling going', async () => {
    const h = harness(emptyHistory(), true);
    h.sync.acquire();
    await h.advance(0);
    h.gets[0].reject(Object.assign(new Error('not found'), { status: 404 }));
    await h.flush();
    h.tokenGets[0].resolve(tokenResponse(5));
    await h.flush();
    await h.advance(POLL_INTERVAL_MS);
    expect(h.gets).toHaveLength(1);
    expect(h.tokenGets).toHaveLength(2);
  });
});

describe('API paths', () => {
  const spies: Array<{ mockRestore(): void }> = [];
  afterEach(() => spies.splice(0).forEach((spy) => spy.mockRestore()));

  test('uses the v8 credentials quota history route', async () => {
    const get = spyOn(apiClient, 'get').mockResolvedValue({ enabled: false } as never);
    const post = spyOn(apiClient, 'post').mockResolvedValue({ imported: 2 } as never);
    spies.push(get, post);
    expect(QUOTA_HISTORY_PATH).toBe('/credentials/quota/history');
    await quotaHistoryApi.get();
    await quotaHistoryApi.importLocal(emptyHistory());
    expect(get.mock.calls[0][0]).toBe('/credentials/quota/history');
    expect(post.mock.calls[0][0]).toBe('/credentials/quota/history');
    expect(post.mock.calls[0][1]).toEqual(emptyHistory());
    expect(TOKEN_USAGE_PATH).toBe('/observability/usage/tokens');
    await tokenUsageApi.get();
    // No from/to: the server's own clock picks the last 24 hours.
    expect(get.mock.calls[1]).toEqual(['/observability/usage/tokens', { signal: undefined }]);
  });
});

/* ------------------------------------------------------------- burn rate */

const weeklySeries = (points: [number, number][]) => ({
  windowMinutes: WEEK_MIN,
  points: points.map(([atMs, usedPercent]) => ({ atMs, usedPercent, resetAtMs: NOW + 48 * H })),
});

const accountInput = (
  history: ReturnType<typeof weeklySeries> | null,
  weeklyUsed: number | null = null
) => ({
  key: 'a',
  label: 'a',
  provider: 'codex' as const,
  units: 10,
  planLabel: 'Pro 100',
  planAssumed: false,
  weekly:
    weeklyUsed === null
      ? null
      : { usedPercent: weeklyUsed, resetAtMs: NOW + 48 * H, periodHours: 168 },
  history,
});

const options = {
  now: NOW,
  span: poolSpan(0, NOW),
  burnWindowHours: 3,
  futureUsage: 'same' as const,
};

describe('burn from real samples', () => {
  test('two server samples a minute apart give a rate right away', () => {
    const built = buildPoolAccount(
      accountInput(
        weeklySeries([
          [NOW - 2 * MIN, 10],
          [NOW - MIN, 11],
        ])
      ),
      NOW
    );
    // 1% of 10 units in one minute = 6 units per hour.
    expect(summarizePool([built], options).burnPerHour).toBeCloseTo(6, 6);
  });

  test('a single old reading stays unknown, not zero', () => {
    const built = buildPoolAccount(accountInput(weeklySeries([[NOW - 2 * H, 10]]), 20), NOW);
    expect(built.remaining).toBeCloseTo(8, 9);
    expect(summarizePool([built], options).burnPerHour).toBeNull();
  });

  test('the rate covers the observed interval, not the line extended to now', () => {
    // Two readings 20 minutes apart, then a live read 30 minutes later shows more spend.
    const built = buildPoolAccount(
      {
        ...accountInput(
          weeklySeries([
            [NOW - 50 * MIN, 10],
            [NOW - 30 * MIN, 12],
          ]),
          40
        ),
        weeklyObservedByMs: NOW - 31 * MIN,
      },
      NOW
    );
    expect(built.history.at(-1)).toEqual({ atMs: NOW, value: expect.any(Number) });
    // 2% of 10 units over 20 minutes = 0.6 units per hour; the drawn line to now does not count.
    expect(summarizePool([built], options).burnPerHour).toBeCloseTo(0.6, 6);
  });

  test('readings that ended before the burn window give no rate', () => {
    const history = [
      { atMs: NOW - 5 * H, value: 10 },
      { atMs: NOW - 4 * H, value: 9 },
      { atMs: NOW, value: 9 },
    ];
    expect(burnRate(history, NOW, 3, { fromMs: NOW - 5 * H, toMs: NOW - 4 * H })).toBeNull();
    expect(burnRate(history, NOW, 6, { fromMs: NOW - 5 * H, toMs: NOW - 4 * H })).toBeCloseTo(1, 6);
  });

  test('a weekly reset between samples restores quota without counting as negative spend', () => {
    const built = buildPoolAccount(
      {
        ...accountInput(null),
        history: {
          windowMinutes: WEEK_MIN,
          points: [
            { atMs: NOW - 40 * MIN, usedPercent: 90, resetAtMs: NOW - 30 * MIN },
            { atMs: NOW - 20 * MIN, usedPercent: 1, resetAtMs: NOW + 168 * H - 30 * MIN },
            { atMs: NOW - 10 * MIN, usedPercent: 2, resetAtMs: NOW + 168 * H - 30 * MIN },
          ],
        },
      },
      NOW
    );
    expect(built.history).toContainEqual({ atMs: NOW - 30 * MIN, value: 10 });
    // Spend: 0 before the reset, 0.1 + 0.1 after it, over 30 minutes.
    expect(summarizePool([built], options).burnPerHour).toBeCloseTo(0.4, 6);
  });
});

/* ------------------------------------------------------------- precedence */

describe('newer server samples beat an older cached read', () => {
  const file = (name: string) => ({ name, type: 'codex', provider: 'codex' }) as AuthFileItem;
  const codexState = (usedPercent: number): CodexQuotaState => ({
    status: 'success',
    planType: 'pro',
    windows: [
      {
        id: 'five-hour',
        label: '5h',
        usedPercent: 100,
        resetLabel: '',
        resetAtMs: NOW + H,
        periodHours: 5,
      },
      {
        id: 'weekly',
        label: '7d',
        usedPercent,
        resetLabel: '',
        resetAtMs: NOW + 48 * H,
        periodHours: 168,
      },
    ],
    rateLimitResetCredits: [],
  });
  const historyWith = (atMs: number, used: number): LocalQuotaHistory => ({
    version: 1,
    startedAtMs: atMs,
    credentials: {
      'codex:a.json': {
        provider: 'codex',
        series: { weekly: weekly([[atMs, used, NOW + 48 * H]]) },
      },
    },
  });
  const build = (
    history: LocalQuotaHistory,
    observed: { observedAtMs?: number | null; observedByMs?: number | null }
  ) =>
    buildProviderPool(
      'codex',
      [
        {
          key: 'a.json',
          label: 'a.json',
          file: file('a.json'),
          quota: codexState(50),
          ...observed,
        },
      ],
      history,
      options
    );

  test('a server reading after the browser read sets the current allowance', () => {
    const pool = build(historyWith(NOW - MIN, 80), { observedAtMs: NOW - 10 * MIN });
    // Pro = 20 units; 20% of it remains.
    expect(pool.summary.total).toBeCloseTo(4, 9);
    expect(pool.summary.accounts[0].source).toBe('history');
    expect(pool.lastReadMs).toBe(NOW - MIN);
    // The routing estimate keeps the live short window and uses the newer weekly reading.
    expect(pool.routing[0]).toMatchObject({ status: 'blocked', remainingUnits: 4 });
  });

  test('an older server reading does not replace a newer browser read', () => {
    const pool = build(historyWith(NOW - 10 * MIN, 80), { observedAtMs: NOW - MIN });
    expect(pool.summary.total).toBeCloseTo(10, 9);
    expect(pool.summary.accounts[0].source).toBe('live');
  });

  test('a read of unknown time loses only to readings after its upper bound', () => {
    expect(
      build(historyWith(NOW - MIN, 80), { observedByMs: NOW - 5 * MIN }).summary.total
    ).toBeCloseTo(4, 9);
    expect(
      build(historyWith(NOW - 6 * MIN, 80), { observedByMs: NOW - 5 * MIN }).summary.total
    ).toBeCloseTo(10, 9);
    // With no bound at all the live read wins, as before.
    expect(build(historyWith(NOW - MIN, 80), {}).summary.total).toBeCloseTo(10, 9);
  });

  test('an old newer-than-live sample still beats an even older live read', () => {
    const pool = build(historyWith(NOW - 2 * H, 80), { observedAtMs: NOW - 3 * H });
    expect(pool.summary.total).toBeCloseTo(4, 9);
  });
});
