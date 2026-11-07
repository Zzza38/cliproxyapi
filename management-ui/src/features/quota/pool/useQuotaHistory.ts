/**
 * Quota history for the pool charts: this browser's log merged in memory with
 * the server's weekly history for the current connection, plus server token totals.
 */

import { useEffect, useMemo, useSyncExternalStore } from 'react';
import { apiClient } from '@/services/api/client';
import { quotaHistoryApi, tokenUsageApi } from '@/services/api/quotaHistory';
import { useAuthStore } from '@/stores';
import { getLocalQuotaHistory, useLocalQuotaHistory } from './historyRecorder';
import { QuotaHistorySync } from './historySync';
import type { LocalQuotaHistory } from './localHistory';
import { mergeHistories } from './serverHistory';
import type { TokenUsage } from './tokenUsage';

const isVisible = () => typeof document === 'undefined' || document.visibilityState !== 'hidden';

export const quotaHistorySync = new QuotaHistorySync({
  getRevision: () => apiClient.getConnectionRevision(),
  isConnected: () => useAuthStore.getState().connectionStatus === 'connected',
  isVisible,
  // A captured client pins this connection and drops side effects (such as a
  // 401 logout) once the connection has changed.
  fetchHistory: (signal) => quotaHistoryApi.get(apiClient.captureConnection(), signal),
  importHistory: (payload, signal) =>
    quotaHistoryApi.importLocal(payload, apiClient.captureConnection(), signal),
  fetchTokens: (signal) => tokenUsageApi.get(apiClient.captureConnection(), signal),
  getLocalHistory: getLocalQuotaHistory,
  now: () => Date.now(),
  setTimer: (callback, delayMs) => setTimeout(callback, delayMs),
  clearTimer: (handle) => clearTimeout(handle as ReturnType<typeof setTimeout>),
});

let wired = false;
const wireGlobalListeners = () => {
  if (wired) return;
  wired = true;
  useAuthStore.subscribe((state, previous) => {
    quotaHistorySync.checkConnection();
    if (state.connectionStatus === 'connected' && previous.connectionStatus !== 'connected') {
      quotaHistorySync.wake();
    }
  });
  if (typeof document !== 'undefined') {
    document.addEventListener('visibilitychange', () => {
      if (isVisible()) quotaHistorySync.wake();
    });
  }
};

export interface QuotaHistoryView {
  /** This browser's own reads (drives auto-read timing). */
  local: LocalQuotaHistory;
  /** Local merged with server history; server readings win on equal timestamps. */
  merged: LocalQuotaHistory;
  /** Whether the server store answered as enabled for this connection. */
  serverEnabled: boolean | null;
  /** Server token totals (last 24h) for this connection; null when unavailable. */
  tokens: TokenUsage | null;
}

export function useQuotaHistory(): QuotaHistoryView {
  const local = useLocalQuotaHistory();
  const server = useSyncExternalStore(
    quotaHistorySync.subscribe,
    quotaHistorySync.getSnapshot,
    quotaHistorySync.getSnapshot
  );
  useEffect(() => {
    wireGlobalListeners();
    return quotaHistorySync.acquire();
  }, []);
  const merged = useMemo(() => mergeHistories(local, server.history), [local, server.history]);
  return { local, merged, serverEnabled: server.enabled, tokens: server.tokens };
}
