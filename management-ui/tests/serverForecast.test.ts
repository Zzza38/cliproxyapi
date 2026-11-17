import { describe, expect, test } from 'bun:test';
import { emptyHistory } from '../src/features/quota/pool/localHistory';
import { QuotaHistorySync } from '../src/features/quota/pool/historySync';
import {
  HOUR_MS,
  projectPool,
  summarizePool,
  valueAt,
  type PoolAccount,
} from '../src/features/quota/pool/poolModel';
import {
  parseServerForecast,
  type ServerForecast,
} from '../src/features/quota/pool/serverForecast';

const H = HOUR_MS;
const NOW = new Date(2026, 10, 5, 12).getTime();

const account = (overrides: Partial<PoolAccount> = {}): PoolAccount => ({
  key: 'a',
  label: 'a',
  provider: 'codex',
  units: 10,
  planLabel: null,
  planAssumed: false,
  remaining: 10,
  nextResetMs: null,
  periodMs: 168 * H,
  history: [
    { atMs: NOW - 2 * H, value: 10.5 },
    { atMs: NOW, value: 10 },
  ],
  observedFromMs: NOW - 2 * H,
  observedToMs: NOW,
  source: 'live',
  ...overrides,
});

const forecast = (overrides: Partial<ServerForecast> = {}): ServerForecast => ({
  startMs: NOW,
  stepMs: H,
  rates: [1, 3, 0, 2],
  method: 'neural-tokens',
  backtestMae: { average: 0.25, 'neural-spend': 0.35, 'neural-tokens': 0.22 },
  backtestDays: 6,
  samples: 600,
  skill: 0.3,
  generation: 4,
  trainedAtMs: NOW,
  ...overrides,
});

describe('server forecast response', () => {
  test('keeps valid providers and drops malformed ones', () => {
    const parsed = parseServerForecast({
      enabled: true,
      providers: {
        codex: { ...forecast(), backtestMae: { average: 1, bogus: 2 } },
        claude: { ...forecast(), rates: [1, -1] },
        gemini: forecast(),
      },
    });
    expect(parsed?.enabled).toBe(true);
    if (!parsed?.enabled) return;
    expect(Object.keys(parsed.providers)).toEqual(['codex']);
    expect(parsed.providers.codex?.backtestMae).toEqual({ average: 1 });
    expect(parseServerForecast({ enabled: false })).toEqual({ enabled: false });
    expect(parseServerForecast('<html>')).toBeNull();
    expect(parseServerForecast({ providers: {} })).toBeNull();
  });
});

describe('server forecast in the pool', () => {
  const options = {
    now: NOW,
    span: { startMs: NOW - 2 * H, endMs: NOW + 6 * H },
    burnWindowHours: 3,
    futureUsage: 'same' as const,
  };

  test('projects along the server hourly schedule, scaled by future usage', () => {
    const summary = summarizePool([account()], { ...options, serverForecast: forecast() });
    expect(summary.forecast.source).toBe('server');
    expect(summary.forecast.method).toBe('neural-tokens');
    expect(valueAt(summary.projected, NOW + H)).toBeCloseTo(9);
    expect(valueAt(summary.projected, NOW + 2 * H)).toBeCloseTo(6);
    expect(valueAt(summary.projected, NOW + 3 * H)).toBeCloseTo(6);
    // Past the schedule the last hourly rate (2/h) continues.
    expect(valueAt(summary.projected, NOW + 5 * H)).toBeCloseTo(2);

    const doubled = summarizePool([account()], {
      ...options,
      futureUsage: 'double',
      serverForecast: forecast(),
    });
    expect(valueAt(doubled.projected, NOW + H)).toBeCloseTo(8);
  });

  test('a served average reports no network skill', () => {
    const summary = summarizePool([account()], {
      ...options,
      serverForecast: forecast({ method: 'average', rates: [0.5] }),
    });
    expect(summary.forecast.method).toBe('average');
    expect(summary.forecast.skill).toBeNull();
    expect(summary.forecast.next24hPerHour).toBeCloseTo(0.5);
  });

  test('a stale or missing server forecast falls back to the window average', () => {
    for (const serverForecast of [null, forecast({ startMs: NOW - 4 * H })]) {
      const summary = summarizePool([account()], { ...options, serverForecast });
      expect(summary.forecast.source).toBe('window');
      expect(summary.forecast.next24hPerHour).toBeCloseTo(0.25);
      expect(summary.forecast.next24hPerHour).toBe(summary.burnPerHour);
    }
  });

  test('projection handles a schedule that starts before now', () => {
    const result = projectPool(
      [account()],
      { startMs: NOW - H, stepMs: H, rates: [5, 1], fallback: 0 },
      NOW,
      NOW + 3 * H
    );
    const line = result.accounts[0].points;
    expect(valueAt(line, NOW + H)).toBeCloseTo(9);
    expect(valueAt(line, NOW + 3 * H)).toBeCloseTo(9);
  });
});

describe('forecast feed', () => {
  const sync = (fetchForecast?: () => Promise<unknown>) => {
    const timers: (() => void)[] = [];
    const instance = new QuotaHistorySync({
      getRevision: () => 1,
      isConnected: () => true,
      isVisible: () => true,
      fetchHistory: async () => ({ enabled: false }),
      importHistory: async () => ({ imported: 0 }),
      fetchForecast,
      getLocalHistory: () => emptyHistory(),
      now: () => NOW,
      setTimer: (callback) => timers.push(callback),
      clearTimer: () => {},
    });
    instance.acquire();
    return instance;
  };

  test('publishes the parsed forecast in the snapshot', async () => {
    const instance = sync(async () => ({ enabled: true, providers: { claude: forecast() } }));
    await instance.tick();
    expect(instance.getSnapshot().forecast?.claude?.method).toBe('neural-tokens');
  });

  test('an older server without the endpoint leaves the forecast empty', async () => {
    const instance = sync(async () => {
      throw Object.assign(new Error('missing'), { status: 404 });
    });
    await instance.tick();
    expect(instance.getSnapshot().forecast).toBeNull();
  });
});
