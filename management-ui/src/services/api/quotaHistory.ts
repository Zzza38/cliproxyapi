/**
 * Server-side Claude/Codex weekly quota history (`/v8/management/credentials/quota/history`).
 *
 * GET returns the stored history plus `enabled`; POST imports browser-recorded weekly
 * readings and answers `{ imported }`. Callers pass a client from
 * `apiClient.captureConnection()` so a response that arrives after a connection change
 * cannot emit events (such as a logout on 401) for the new connection.
 */

import { apiClient } from './client';

export const QUOTA_HISTORY_PATH = '/credentials/quota/history';

type HistoryClient = Pick<typeof apiClient, 'get' | 'post'>;

export const quotaHistoryApi = {
  get: (client: HistoryClient = apiClient, signal?: AbortSignal) =>
    client.get<unknown>(QUOTA_HISTORY_PATH, { signal }),
  importLocal: (payload: unknown, client: HistoryClient = apiClient, signal?: AbortSignal) =>
    client.post<unknown>(QUOTA_HISTORY_PATH, payload, { signal }),
};

/** Per-provider token totals; without `from`/`to` the server uses its last 24 hours. */
export const TOKEN_USAGE_PATH = '/observability/usage/tokens';

export const tokenUsageApi = {
  get: (client: HistoryClient = apiClient, signal?: AbortSignal) =>
    client.get<unknown>(TOKEN_USAGE_PATH, { signal }),
};
