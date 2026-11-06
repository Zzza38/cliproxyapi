import { expect, test } from 'bun:test';
import {
  createClaudeAccountReader,
  CLAUDE_USAGE_READ_MS,
} from '../src/services/api/claudeAccountReads';
import type { ApiCallRequest, ApiCallResult } from '../src/services/api/apiCall';
const request: ApiCallRequest = {
  authIndex: 'a',
  method: 'GET',
  url: 'https://api.anthropic.com/api/oauth/usage',
};
const ok: ApiCallResult = { statusCode: 200, header: {}, body: {}, bodyText: '{}' };
const limited: ApiCallResult = {
  statusCode: 429,
  header: { 'Retry-After': ['0'] },
  body: {},
  bodyText: '{}',
};

test('one detailed usage read per fifteen minutes, profile per hour, and fresh reset preflight bypasses cache', async () => {
  let now = 0,
    calls = 0;
  const reader = createClaudeAccountReader({
    now: () => now,
    revision: () => 1,
    request: async () => {
      calls++;
      return ok;
    },
  });
  const first = reader.read(request);
  expect(reader.read(request)).toBe(first);
  await first;
  for (let i = 1; i < 900; i++) {
    now = i * 1000;
    await reader.read(request);
  }
  expect(calls).toBe(1);
  now = CLAUDE_USAGE_READ_MS;
  await reader.read(request);
  expect(calls).toBe(2);
  await reader.read(request, true);
  expect(calls).toBe(3);
  const profile = { ...request, url: 'https://api.anthropic.com/api/oauth/profile' };
  await reader.read(profile);
  now += 30 * 60_000;
  await reader.read(profile);
  expect(calls).toBe(4);
  reader.invalidate('a');
  await reader.read(profile);
  expect(calls).toBe(5);
});

test('429 cooldown is shared across usage, profile, and reset-status reads and backs off', async () => {
  let now = 0,
    calls = 0;
  const reader = createClaudeAccountReader({
    now: () => now,
    revision: () => 1,
    request: async () => {
      calls++;
      return limited;
    },
  });
  await reader.read(request);
  for (const minutes of [5, 10, 20, 30, 30]) {
    const started = now;
    now = started + minutes * 60_000 - 1;
    await reader.read({ ...request, url: 'https://api.anthropic.com/api/oauth/profile' }, true);
    const before = calls;
    now++;
    await reader.read(request);
    expect(calls).toBe(before + 1);
  }
  expect(calls).toBe(6);
});

test('positive Retry-After overrides local backoff and connections stay isolated', async () => {
  let now = 0,
    revision = 1,
    calls = 0;
  const reader = createClaudeAccountReader({
    now: () => now,
    revision: () => revision,
    request: async () => {
      calls++;
      return { ...limited, header: { 'retry-after': ['3600'] } };
    },
  });
  await reader.read(request);
  now = 30 * 60_000;
  await reader.read(request, true);
  expect(calls).toBe(1);
  revision++;
  await reader.read(request);
  expect(calls).toBe(2);
});

test('an older successful profile request cannot clear a concurrent 429', async () => {
  let finish!: (r: ApiCallResult) => void;
  let calls = 0;
  const reader = createClaudeAccountReader({
    now: () => 0,
    revision: () => 1,
    request: async (p) => {
      calls++;
      return p.url.endsWith('/profile')
        ? new Promise<ApiCallResult>((r) => {
            finish = r;
          })
        : limited;
    },
  });
  const profile = reader.read({ ...request, url: 'https://api.anthropic.com/api/oauth/profile' });
  await reader.read(request);
  finish(ok);
  await profile;
  await reader.read(request, true);
  expect(calls).toBe(2);
});

test('cached quota keeps its original observation time', async () => {
  let now = 1000;
  const reader = createClaudeAccountReader({
    now: () => now,
    revision: () => 1,
    request: async () => ok,
  });
  expect((await reader.read(request)).observedAtMs).toBe(1000);
  now = 2000;
  expect((await reader.read(request)).observedAtMs).toBe(1000);
});

test('a reset invalidates cached and in-flight reads without caching the old result', async () => {
  const completions: Array<(result: ApiCallResult) => void> = [];
  const reader = createClaudeAccountReader({
    now: () => 0,
    revision: () => 1,
    request: () => new Promise<ApiCallResult>((resolve) => completions.push(resolve)),
  });
  const stale = reader.read(request);
  reader.invalidate('a');
  completions[0](ok);
  await stale;
  const fresh = reader.read(request);
  expect(completions).toHaveLength(2);
  completions[1](ok);
  await fresh;
});
