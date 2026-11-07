/**
 * Records every successful Claude/Codex quota read into the browser-local log
 * (the server keeps its own weekly history; see historySync.ts).
 *
 * It watches the shared quota store, so reads started from any page (quota
 * page, auth file cards, auto-refresh) are recorded once. A state object is
 * recorded only the first time it is seen, which keeps a cached state from being
 * re-stamped with a later time when the page remounts.
 */

import { useEffect, useSyncExternalStore } from 'react';
import { useAuthStore, useQuotaStore } from '@/stores';
import type { ClaudeQuotaState, CodexQuotaState } from '@/types';
import {
  emptyHistory,
  loadHistory,
  pruneHistory,
  recordReading,
  saveHistory,
  clearHistory,
  type LocalQuotaHistory,
} from './localHistory';
import type { PooledProvider } from './planUnits';

let started = false;
let scope = '';
let history: LocalQuotaHistory = emptyHistory();
let startedAtMs = 0;
const seen = new WeakSet<object>();
/** When each seen state was read: exact time, or only an upper bound for pre-recorder reads. */
const observations = new WeakMap<object, QuotaObservation>();
/** Latest successful state per credential, so a refresh in flight does not blank the pool. */
const lastSuccess = new Map<string, ClaudeQuotaState | CodexQuotaState>();
const listeners = new Set<() => void>();

const notify = () => listeners.forEach((listener) => listener());

const currentScope = () => useAuthStore.getState().apiBase || '';

const ensureScope = () => {
  const next = currentScope();
  if (next === scope && started) return;
  if (started) lastSuccess.clear();
  scope = next;
  history = pruneHistory(loadHistory(scope), Date.now());
};

export interface QuotaObservation {
  /** Read time when known. */
  atMs: number | null;
  /** Latest time the read can have happened. */
  byMs: number;
}

const claudeObservedAt = (provider: PooledProvider, state: ClaudeQuotaState | CodexQuotaState) => {
  if (provider !== 'claude') return null;
  const parsed = Date.parse((state as ClaudeQuotaState).observedAt ?? '');
  return Number.isFinite(parsed) ? parsed : null;
};

const observedAtMs = (provider: PooledProvider, state: ClaudeQuotaState | CodexQuotaState) =>
  claudeObservedAt(provider, state) ?? Date.now();

const recordMap = (
  provider: PooledProvider,
  map: Record<string, ClaudeQuotaState | CodexQuotaState>
): boolean => {
  let changed = false;
  Object.entries(map).forEach(([key, state]) => {
    if (!state || seen.has(state)) return;
    seen.add(state);
    // Claude keeps the previous windows visible while a refresh is loading or
    // failing; only a fresh success is a new observation.
    if (
      state.status !== 'success' ||
      (provider === 'claude' && (state as ClaudeQuotaState).stale)
    ) {
      return;
    }
    lastSuccess.set(`${provider}:${key}`, state);
    const atMs = observedAtMs(provider, state);
    observations.set(state, { atMs, byMs: atMs });
    const next = recordReading(history, provider, key, state.windows, atMs);
    if (next !== history) {
      history = next;
      changed = true;
    }
  });
  return changed;
};

/** Start recording (idempotent). Existing cached states are marked seen, not recorded. */
export function startQuotaHistoryRecorder(): void {
  if (started) return;
  ensureScope();
  started = true;
  startedAtMs = Date.now();
  const initial = useQuotaStore.getState();
  // Cached states predate the recorder: usable as current data, but their read
  // time is unknown, so they are not written to the history log.
  (
    [
      ['claude', initial.claudeQuota],
      ['codex', initial.codexQuota],
    ] as const
  ).forEach(([provider, map]) =>
    Object.entries(map).forEach(([key, state]) => {
      if (!state) return;
      seen.add(state);
      if (state.status !== 'success') return;
      lastSuccess.set(`${provider}:${key}`, state);
      const atMs = claudeObservedAt(provider, state);
      observations.set(state, { atMs, byMs: atMs ?? startedAtMs });
    })
  );

  useQuotaStore.subscribe((state, previous) => {
    if (state.claudeQuota === previous.claudeQuota && state.codexQuota === previous.codexQuota) {
      return;
    }
    const before = scope;
    ensureScope();
    let changed = before !== scope;
    if (state.claudeQuota !== previous.claudeQuota) {
      changed = recordMap('claude', state.claudeQuota) || changed;
    }
    if (state.codexQuota !== previous.codexQuota) {
      changed = recordMap('codex', state.codexQuota) || changed;
    }
    if (changed) {
      saveHistory(scope, history);
      notify();
    }
  });

  useAuthStore.subscribe(() => {
    const before = scope;
    ensureScope();
    if (before !== scope) notify();
  });
  notify();
}

/** Latest successful quota state seen for a credential in this page session. */
export function getLastSuccessfulQuota(
  provider: PooledProvider,
  key: string
): ClaudeQuotaState | CodexQuotaState | undefined {
  return lastSuccess.get(`${provider}:${key}`);
}

/** When a quota state was read, as far as this page session knows. */
export function getQuotaObservation(state: object | undefined): QuotaObservation | undefined {
  return state ? observations.get(state) : undefined;
}

/** This browser's log for the current connection (used for the one-time server import). */
export function getLocalQuotaHistory(): LocalQuotaHistory {
  const before = scope;
  ensureScope();
  if (before !== scope) notify();
  return history;
}

export function clearQuotaHistory(): void {
  clearHistory(scope);
  history = emptyHistory();
  notify();
}

/** Drop stored credentials that no longer exist on the server. */
export function pruneQuotaHistory(liveCredentialIds: Set<string>): void {
  const next = pruneHistory(history, Date.now(), liveCredentialIds);
  const before = Object.keys(history.credentials).length;
  if (Object.keys(next.credentials).length === before) return;
  history = next;
  saveHistory(scope, history);
  notify();
}

const subscribe = (listener: () => void) => {
  listeners.add(listener);
  return () => listeners.delete(listener);
};
const getSnapshot = () => history;

/** Current local history; starts the recorder on first use. */
export function useLocalQuotaHistory(): LocalQuotaHistory {
  useEffect(() => {
    startQuotaHistoryRecorder();
  }, []);
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot);
}
