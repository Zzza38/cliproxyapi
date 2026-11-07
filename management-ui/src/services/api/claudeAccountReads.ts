import { apiClient } from './client';
import { apiCallApi, type ApiCallRequest, type ApiCallResult } from './apiCall';

export const CLAUDE_USAGE_READ_MS = 15 * 60_000;
export const CLAUDE_PROFILE_READ_MS = 60 * 60_000;
export type ClaudeAccountReadResult = ApiCallResult & { observedAtMs?: number };

const INITIAL_BACKOFF_MS = 5 * 60_000;
const MAX_BACKOFF_MS = 30 * 60_000;

/** Share read-only account lookups across cards, while keeping reset preflight fresh. */
export function createClaudeAccountReader(
  deps = {
    now: () => Date.now(),
    revision: () => apiClient.getConnectionRevision(),
    request: (payload: ApiCallRequest) => apiCallApi.request(payload),
  }
) {
  let revision = deps.revision();
  const epochs = new Map<string, number>();
  const cache = new Map<string, { until: number; result: ClaudeAccountReadResult }>();
  const pending = new Map<string, Promise<ClaudeAccountReadResult>>();
  const throttles = new Map<
    string,
    { until: number; failures: number; result: ClaudeAccountReadResult }
  >();
  const sync = () => {
    if (revision !== deps.revision()) {
      revision = deps.revision();
      epochs.clear();
      cache.clear();
      pending.clear();
      throttles.clear();
    }
    return revision;
  };
  return {
    invalidate(authIndex: string) {
      sync();
      epochs.set(authIndex, (epochs.get(authIndex) ?? 0) + 1);
      for (const key of pending.keys()) if (key.startsWith(`${authIndex}\n`)) pending.delete(key);
      for (const key of cache.keys()) if (key.startsWith(`${authIndex}\n`)) cache.delete(key);
    },
    read(payload: ApiCallRequest, fresh = false): Promise<ClaudeAccountReadResult> {
      const session = sync();
      const account = payload.authIndex ?? '';
      const key = `${account}\n${payload.url}`;
      const epoch = epochs.get(account) ?? 0;
      const now = deps.now();
      const throttle = throttles.get(account);
      if (throttle && throttle.until > now) return Promise.resolve(throttle.result);
      const cached = cache.get(key);
      if (!fresh && cached && cached.until > now) return Promise.resolve(cached.result);
      const existing = pending.get(key);
      if (existing && !fresh) return existing;
      const request = deps
        .request(payload)
        .then((raw) => {
          const result: ClaudeAccountReadResult = { ...raw, observedAtMs: now };
          if (session !== deps.revision()) throw new Error('connection changed');
          if (epoch !== (epochs.get(account) ?? 0)) return result;
          if (result.statusCode === 429) {
            const failures = (throttles.get(account)?.failures ?? 0) + 1;
            const retryValue = Object.entries(result.header).find(
              ([name]) => name.toLowerCase() === 'retry-after'
            )?.[1]?.[0];
            const seconds = Number(retryValue);
            const retryMs = Number.isFinite(seconds)
              ? Math.max(0, seconds * 1000)
              : Math.max(0, Date.parse(retryValue ?? '') - deps.now());
            const backoff = Math.min(
              MAX_BACKOFF_MS,
              INITIAL_BACKOFF_MS * 2 ** Math.min(failures - 1, 8)
            );
            throttles.set(account, {
              until: deps.now() + Math.max(backoff, retryMs || 0),
              failures,
              result,
            });
          } else if (result.statusCode >= 200 && result.statusCode < 300) {
            // An older profile request must not erase a newer usage throttle.
            if ((throttles.get(account)?.until ?? 0) <= now) throttles.delete(account);
            const ttl = new URL(payload.url).pathname.endsWith('/profile')
              ? CLAUDE_PROFILE_READ_MS
              : CLAUDE_USAGE_READ_MS;
            cache.set(key, { until: deps.now() + ttl, result });
          }
          return result;
        })
        .finally(() => {
          if (pending.get(key) === request) pending.delete(key);
        });
      pending.set(key, request);
      return request;
    },
  };
}

export const claudeAccountReader = createClaudeAccountReader();
