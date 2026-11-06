/**
 * Keeps the server's weekly quota history and 24h token totals in memory for the
 * current connection.
 *
 * - While at least one consumer is mounted and the page is visible, both are
 *   fetched about once a minute, each on its own schedule.
 * - Once per connection, compatible weekly browser readings are imported (POST)
 *   before the next history GET, so the merged server copy includes them. The
 *   local log is kept as is.
 * - Every request is tied to `apiClient.getConnectionRevision()`: a login,
 *   logout or base/key change (including switching back, A→B→A) aborts the
 *   in-flight request, drops everything held and starts over. Late responses
 *   of an older revision are ignored.
 * - A missing endpoint (older server) stops that request for the connection.
 *   Other failures and `enabled: false` back off quietly; nothing is logged or shown.
 *
 * React-free; dependencies are injected so tests drive it with fakes.
 */

import type { LocalQuotaHistory } from './localHistory';
import { buildImportPayload, parseImportResponse, parseServerHistory } from './serverHistory';
import { parseTokenUsage, type TokenUsage } from './tokenUsage';

export const POLL_INTERVAL_MS = 60_000;
export const MAX_BACKOFF_MS = 15 * 60_000;
const MAX_IMPORT_ATTEMPTS = 3;

export interface HistorySyncDeps {
  getRevision(): number;
  /** Whether the current connection is logged in and usable. */
  isConnected(): boolean;
  isVisible(): boolean;
  fetchHistory(signal: AbortSignal): Promise<unknown>;
  importHistory(payload: LocalQuotaHistory, signal: AbortSignal): Promise<unknown>;
  /** Token totals; omit to skip them. */
  fetchTokens?(signal: AbortSignal): Promise<unknown>;
  /** This browser's local log for the current connection. */
  getLocalHistory(): LocalQuotaHistory;
  now(): number;
  setTimer(callback: () => void, delayMs: number): unknown;
  clearTimer(handle: unknown): void;
}

export interface ServerHistorySnapshot {
  revision: number;
  /** True / false once the server answered; null before that or when unavailable. */
  enabled: boolean | null;
  history: LocalQuotaHistory | null;
  /** Token totals when the server store answered as enabled; null otherwise. */
  tokens: TokenUsage | null;
}

const statusOf = (error: unknown): number | undefined =>
  (error as { status?: number } | null)?.status;

/** The server has no such route (older build): stop asking on this connection. */
const isMissingEndpoint = (error: unknown) => {
  const status = statusOf(error);
  return status === 404 || status === 405 || status === 501;
};

/** The server rejected the import itself; retrying the same payload will not help. */
const isRejectedImport = (error: unknown) => {
  const status = statusOf(error);
  return status !== undefined && status >= 400 && status < 500 && status !== 408 && status !== 429;
};

/** Schedule state of one polled endpoint. */
interface Feed {
  stopped: boolean;
  failures: number;
  dueMs: number;
}

const freshFeed = (): Feed => ({ stopped: false, failures: 0, dueMs: Number.NEGATIVE_INFINITY });

const backoff = (failures: number) =>
  failures === 0 ? POLL_INTERVAL_MS : Math.min(MAX_BACKOFF_MS, POLL_INTERVAL_MS * 2 ** failures);

const emptySnapshot = (revision: number): ServerHistorySnapshot => ({
  revision,
  enabled: null,
  history: null,
  tokens: null,
});

export class QuotaHistorySync {
  private revision = Number.NaN;
  private snapshot: ServerHistorySnapshot = emptySnapshot(Number.NaN);
  private readonly emptySnapshots = new Map<number, ServerHistorySnapshot>();
  private known: { revision: number; ids: Set<string> } | null = null;
  private imported = false;
  private importAttempts = 0;
  private history: Feed = freshFeed();
  private tokens: Feed = freshFeed();
  private controller: AbortController | null = null;
  private timer: unknown = null;
  private consumers = 0;
  private readonly listeners = new Set<() => void>();

  constructor(private readonly deps: HistorySyncDeps) {
    if (!deps.fetchTokens) this.tokens.stopped = true;
  }

  /** Start polling for a mounted consumer; the returned function releases it. */
  acquire(): () => void {
    this.consumers += 1;
    this.checkConnection();
    if (this.consumers === 1) this.kick();
    let released = false;
    return () => {
      if (released) return;
      released = true;
      this.consumers -= 1;
      if (this.consumers === 0) {
        this.cancelTimer();
        this.abort();
      }
    };
  }

  /** Credentials that currently exist on the connection identified by `revision`. */
  setKnownCredentials(revision: number, ids: Set<string>): void {
    if (revision !== this.deps.getRevision()) return;
    this.checkConnection();
    const first = !this.known || this.known.revision !== revision;
    this.known = { revision, ids };
    // The first import waits for the credential list; start it as soon as it arrives.
    if (first && this.importPending() && !this.controller && this.consumers > 0) this.kick();
  }

  /** Call when auth state changes. */
  checkConnection(): void {
    const revision = this.deps.getRevision();
    if (revision === this.revision) return;
    this.abort();
    this.revision = revision;
    this.known = null;
    this.imported = false;
    this.importAttempts = 0;
    this.history = freshFeed();
    this.tokens = freshFeed();
    if (!this.deps.fetchTokens) this.tokens.stopped = true;
    this.snapshot = emptySnapshot(revision);
    this.notify();
    if (this.consumers > 0) this.kick();
  }

  /** Page visible again or connection ready: fetch whatever is due now. */
  wake(): void {
    this.checkConnection();
    if (this.consumers === 0 || this.controller || this.allStopped()) return;
    this.kick();
  }

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  };

  /** Server data for the current connection only; stable between changes. */
  getSnapshot = (): ServerHistorySnapshot => {
    const revision = this.deps.getRevision();
    if (this.snapshot.revision === revision) return this.snapshot;
    let empty = this.emptySnapshots.get(revision);
    if (!empty) {
      this.emptySnapshots.clear();
      empty = emptySnapshot(revision);
      this.emptySnapshots.set(revision, empty);
    }
    return empty;
  };

  /* ------------------------------------------------------------ internals */

  private notify() {
    this.listeners.forEach((listener) => listener());
  }

  private cancelTimer() {
    if (this.timer !== null) this.deps.clearTimer(this.timer);
    this.timer = null;
  }

  private abort() {
    this.controller?.abort();
    this.controller = null;
  }

  private kick() {
    this.schedule(0);
  }

  private allStopped() {
    return this.history.stopped && this.tokens.stopped;
  }

  private schedule(delayMs: number) {
    this.cancelTimer();
    if (this.consumers === 0 || this.allStopped()) return;
    this.timer = this.deps.setTimer(
      () => {
        this.timer = null;
        void this.tick();
      },
      Math.max(0, delayMs)
    );
  }

  private importPending() {
    return (
      !this.history.stopped &&
      !this.imported &&
      this.importAttempts === 0 &&
      this.known?.revision === this.revision &&
      this.snapshot.enabled !== false
    );
  }

  private nextDelay() {
    // The credential list arrived during a fetch: import right away.
    if (this.importPending()) return 0;
    const dues = [this.history, this.tokens]
      .filter((feed) => !feed.stopped)
      .map((feed) => feed.dueMs);
    return Math.min(...dues) - this.deps.now();
  }

  private update(patch: Partial<Omit<ServerHistorySnapshot, 'revision'>>) {
    this.snapshot = { ...this.snapshot, ...patch, revision: this.revision };
    this.notify();
  }

  private settle(feed: Feed, failed: boolean) {
    feed.failures = failed ? feed.failures + 1 : 0;
    feed.dueMs = this.deps.now() + backoff(feed.failures);
  }

  /** One import (when due) and one fetch of each due endpoint. Exposed for tests. */
  async tick(): Promise<void> {
    this.checkConnection();
    if (this.consumers === 0 || this.controller || this.allStopped()) return;
    if (!this.deps.isConnected() || !this.deps.isVisible()) {
      this.schedule(POLL_INTERVAL_MS);
      return;
    }
    const revision = this.revision;
    const controller = new AbortController();
    this.controller = controller;
    const current = () =>
      this.controller === controller &&
      !controller.signal.aborted &&
      revision === this.revision &&
      revision === this.deps.getRevision();

    try {
      const now = this.deps.now();
      if (!this.history.stopped && (this.history.dueMs <= now || this.importPending())) {
        await this.syncHistory(controller.signal, current);
        if (!current()) return;
      }
      if (!this.tokens.stopped && this.tokens.dueMs <= this.deps.now()) {
        await this.syncTokens(controller.signal, current);
      }
    } finally {
      if (this.controller === controller) {
        this.controller = null;
        if (revision !== this.deps.getRevision()) this.checkConnection();
        else this.schedule(this.nextDelay());
      }
    }
  }

  private async syncHistory(signal: AbortSignal, current: () => boolean) {
    const revision = this.revision;
    if (!this.imported && this.snapshot.enabled !== false && this.known?.revision === revision) {
      const payload = buildImportPayload(
        this.deps.getLocalHistory(),
        this.known.ids,
        this.deps.now()
      );
      if (payload === null) {
        this.imported = true;
      } else {
        this.importAttempts += 1;
        try {
          const response = await this.deps.importHistory(payload, signal);
          if (!current()) return;
          this.imported = parseImportResponse(response) !== null;
          if (!this.imported && this.importAttempts >= MAX_IMPORT_ATTEMPTS) this.imported = true;
        } catch (error) {
          if (!current()) return;
          if (isMissingEndpoint(error)) {
            this.stopHistory();
            return;
          }
          if (isRejectedImport(error) || this.importAttempts >= MAX_IMPORT_ATTEMPTS) {
            this.imported = true;
          }
        }
      }
    }

    let response: unknown;
    try {
      response = await this.deps.fetchHistory(signal);
    } catch (error) {
      if (!current()) return;
      if (isMissingEndpoint(error)) this.stopHistory();
      else this.settle(this.history, true);
      return;
    }
    if (!current()) return;
    const parsed = parseServerHistory(response, this.deps.now());
    if (!parsed) {
      // Not a quota-history response (for example an HTML fallback page).
      this.stopHistory();
    } else if (!parsed.enabled) {
      this.settle(this.history, true);
      this.update({ enabled: false, history: null });
    } else {
      this.settle(this.history, false);
      this.update({ enabled: true, history: parsed.history });
    }
  }

  private stopHistory() {
    this.history.stopped = true;
    if (this.snapshot.history !== null || this.snapshot.enabled !== null) {
      this.update({ enabled: null, history: null });
    }
  }

  private async syncTokens(signal: AbortSignal, current: () => boolean) {
    const fetchTokens = this.deps.fetchTokens;
    if (!fetchTokens) return;
    let response: unknown;
    try {
      response = await fetchTokens(signal);
    } catch (error) {
      if (!current()) return;
      if (isMissingEndpoint(error)) this.stopTokens();
      else this.settle(this.tokens, true);
      return;
    }
    if (!current()) return;
    const parsed = parseTokenUsage(response);
    if (!parsed) {
      this.stopTokens();
    } else if (!parsed.enabled) {
      this.settle(this.tokens, true);
      if (this.snapshot.tokens !== null) this.update({ tokens: null });
    } else {
      this.settle(this.tokens, false);
      this.update({ tokens: parsed.usage });
    }
  }

  private stopTokens() {
    this.tokens.stopped = true;
    if (this.snapshot.tokens !== null) this.update({ tokens: null });
  }
}
