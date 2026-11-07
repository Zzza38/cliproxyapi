/**
 * Quota page: provider tabs, pooled Claude/Codex subscription charts, then the
 * per-account card grid.
 *
 * Behavior contracts kept from upstream:
 * - Existing providers stay click-to-load; Devin queries once when first visible, without polling.
 * - cacheGeneration isolates sessions and request ids drop superseded responses (useQuotaBatchLoader).
 * - After the file list changes, each provider's quota cache is pruned (deleted files leave nothing behind).
 * - useHeaderRefresh has a single slot: this page is its only registrant; global refresh = reload the file list.
 *
 * CLIProxyAPI additions:
 * - Pools display weighted weekly percentages relative to the largest plan.
 * - Claude/Codex quota can be read on an interval while the page is visible. It only
 *   reads quota; reset credits are never redeemed automatically.
 * - Recorded history is the server's weekly history merged with this browser's
 *   own reads (pool/useQuotaHistory.ts); browser readings are imported once per connection.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { apiClient, authFilesApi } from '@/services/api';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { IconSearch, IconX } from '@/components/ui/icons';
import { Select } from '@/components/ui/Select';
import { Skeleton } from '@/components/ui/Skeleton';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { useNow } from '@/hooks/useNow';
import { useRevealGroup } from '@/hooks/motion';
import { useAuthStore, useConfigStore, useQuotaStore, useThemeStore } from '@/stores';
import type { AuthFileItem, ResolvedTheme } from '@/types';
import { getQuotaCacheKey, getQuotaDisplayName } from '@/utils/quota/identity';
import { getTypeLabel } from '@/features/authFiles/constants';
import { ProviderTabs } from '@/features/authFiles/components/ProviderTabs';
import { QuotaHeader } from './components/QuotaHeader';
import { QuotaCard } from './components/QuotaCard';
import { QuotaControls } from './components/QuotaControls';
import { QuotaPoolPanel } from './components/QuotaPoolPanel';
import { QuotaTimeline } from './components/QuotaTimeline';
import {
  CARD_ENTRANCE_BUDGET_MS,
  QUOTA_PAGE_SIZE,
  QUOTA_SORT_MODES,
  QUOTA_TAB_ORDER,
  type QuotaSortMode,
  type QuotaTabId,
} from './constants';
import {
  buildTabCounts,
  canRefreshQuotaAfterList,
  classifyQuotaFiles,
  filterEntriesByTab,
  filterEntriesBySearch,
  paginate,
  sortQuotaEntries,
  type QuotaFileEntry,
} from './logic';
import { nextRecoveryMs } from './resetSchedule';
import { QUOTA_ADAPTERS, getQuotaSetter, type QuotaCardState } from './providers';
import type { QuotaProviderType } from './providers/types';
import { useDevinQuotaAutoLoad } from './providers/devin/useDevinQuotaAutoLoad';
import { useQuotaActions } from './hooks/useQuotaActions';
import { useQuotaBatchLoader } from './hooks/useQuotaBatchLoader';
import {
  DEFAULT_AUTO_READ_MINUTES,
  readQuotaUiState,
  writeQuotaUiState,
  type AutoReadMinutes,
} from './uiState';
import { buildProviderPool, type PooledQuotaState } from './pool/buildPools';
import {
  clearQuotaHistory,
  getLastSuccessfulQuota,
  getQuotaObservation,
  pruneQuotaHistory,
} from './pool/historyRecorder';
import { quotaHistorySync, useQuotaHistory } from './pool/useQuotaHistory';
import { emptyTokenTotals } from './pool/tokenUsage';
import { historyCoverage, historyCredentialId, seriesFor } from './pool/localHistory';
import { POOLED_PROVIDERS, isPooledProvider, type PooledProvider } from './pool/planUnits';
import { POOL_ACCENTS, formatDate } from './pool/poolFormat';
import {
  poolDays,
  poolSpan,
  WEEKLY_WINDOW_ID,
  type BurnWindowHours,
  type FutureUsage,
  type PoolMode,
} from './pool/poolModel';
import styles from './QuotaPage.module.scss';

const TAB_IDS: string[] = ['all', ...QUOTA_TAB_ORDER];
const SKELETON_CARD_COUNT = 6;

/**
 * Existing providers display filenames; Devin's card and timeline share an
 * identity-aware display label. Keep the filename fallback stable for memoization.
 */
const displayNameFor = (name: string) => name;

const hasWindows = (state: QuotaCardState | undefined): boolean =>
  Boolean(state && (state as PooledQuotaState).windows?.length > 0);

export function QuotaPage() {
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const connectionStatus = useAuthStore((state) => state.connectionStatus);
  const resolvedTheme: ResolvedTheme = useThemeStore((state) => state.resolvedTheme);
  const routingStrategy = useConfigStore((state) => state.config?.routingStrategy ?? null);

  const [files, setFiles] = useState<AuthFileItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [tab, setTab] = useState<QuotaTabId>(() => readQuotaUiState()?.tab ?? 'all');
  const [sortMode, setSortMode] = useState<QuotaSortMode>(
    () => readQuotaUiState()?.sortMode ?? 'default'
  );
  const [poolMode, setPoolMode] = useState<PoolMode>(
    () => readQuotaUiState()?.poolMode ?? 'remaining'
  );
  const [burnWindowHours, setBurnWindowHours] = useState<BurnWindowHours>(
    () => readQuotaUiState()?.burnWindowHours ?? 3
  );
  const [futureUsage, setFutureUsage] = useState<FutureUsage>(
    () => readQuotaUiState()?.futureUsage ?? 'same'
  );
  const [showAccountLines, setShowAccountLines] = useState(
    () => readQuotaUiState()?.showAccountLines ?? false
  );
  const [autoReadMinutes, setAutoReadMinutes] = useState<AutoReadMinutes>(
    () => readQuotaUiState()?.autoReadMinutes ?? DEFAULT_AUTO_READ_MINUTES
  );
  const [rangeOffset, setRangeOffset] = useState(0);
  const [page, setPage] = useState(1);
  const [search, setSearch] = useState('');
  const searchInputRef = useRef<HTMLInputElement>(null);
  // Header + tabs entrance cascade (title, meta, actions, tabs; 70ms apart).
  const revealRef = useRevealGroup<HTMLDivElement>();

  const disableControls = connectionStatus !== 'connected';

  /* ---------- File list ---------- */

  const sessionGeneration = useQuotaStore((state) => state.cacheGeneration);
  const [filesGeneration, setFilesGeneration] = useState<number | null>(null);
  const [filesRevision, setFilesRevision] = useState<number | null>(null);
  const listRequestRef = useRef(0);
  const loadFiles = useCallback(async () => {
    const requestId = ++listRequestRef.current;
    if (connectionStatus !== 'connected') {
      setFiles([]);
      setFilesGeneration(null);
      setLoading(false);
      return;
    }
    const revision = apiClient.getConnectionRevision();
    const isCurrent = () =>
      requestId === listRequestRef.current &&
      sessionGeneration === useQuotaStore.getState().cacheGeneration &&
      revision === apiClient.getConnectionRevision();
    setLoading(true);
    setError('');
    try {
      const data = await authFilesApi.list();
      if (!isCurrent()) return;
      setFiles(data?.files || []);
      setFilesGeneration(sessionGeneration);
      setFilesRevision(revision);
    } catch (err: unknown) {
      if (!isCurrent()) return;
      const message = err instanceof Error ? err.message : t('notification.refresh_failed');
      setError(message);
    } finally {
      if (isCurrent()) setLoading(false);
    }
  }, [connectionStatus, sessionGeneration, t]);

  useHeaderRefresh(loadFiles);

  useEffect(() => {
    void loadFiles();
    return () => {
      listRequestRef.current += 1;
    };
  }, [loadFiles]);

  /* ---------- Quota cache ----------
   * Read before classification/sorting: "soonest recovery" needs it for the sort key. */

  const antigravityQuota = useQuotaStore((state) => state.antigravityQuota);
  const claudeQuota = useQuotaStore((state) => state.claudeQuota);
  const codexQuota = useQuotaStore((state) => state.codexQuota);
  const devinQuota = useQuotaStore((state) => state.devinQuota);
  const kimiQuota = useQuotaStore((state) => state.kimiQuota);
  const metaQuota = useQuotaStore((state) => state.metaQuota);
  const pluginQuota = useQuotaStore((state) => state.pluginQuota);
  const xaiQuota = useQuotaStore((state) => state.xaiQuota);

  const quotaByType = useMemo<Record<QuotaProviderType, Record<string, QuotaCardState>>>(
    () =>
      ({
        antigravity: antigravityQuota,
        claude: claudeQuota,
        codex: codexQuota,
        devin: devinQuota,
        kimi: kimiQuota,
        meta: metaQuota,
        plugin: pluginQuota,
        xai: xaiQuota,
      }) as unknown as Record<QuotaProviderType, Record<string, QuotaCardState>>,
    [
      antigravityQuota,
      claudeQuota,
      codexQuota,
      devinQuota,
      kimiQuota,
      metaQuota,
      pluginQuota,
      xaiQuota,
    ]
  );

  const getQuota = useCallback(
    (entry: QuotaFileEntry): QuotaCardState | undefined =>
      quotaByType[entry.type][getQuotaCacheKey(entry.file)],
    [quotaByType]
  );

  /* ---------- Classify / filter / sort / paginate ---------- */

  // Subscribe to the minute clock only for "soonest recovery". Without this gate,
  // pageItems would change identity every minute under the default sort and keep
  // re-triggering the "refresh all" loading falling-edge effect below.
  const tick = useNow(sortMode !== 'default');
  const sortNow = sortMode === 'default' ? 0 : tick;

  const entries = useMemo(() => classifyQuotaFiles(files), [files]);
  const tabCounts = useMemo(() => buildTabCounts(entries), [entries]);
  const tabEntries = useMemo(() => filterEntriesByTab(entries, tab), [entries, tab]);
  const filteredEntries = useMemo(
    () => filterEntriesBySearch(tabEntries, search),
    [tabEntries, search]
  );
  const handleSearchChange = useCallback((value: string) => {
    setSearch(value);
    setPage(1);
  }, []);

  const resolveNextRecovery = useCallback(
    (entry: QuotaFileEntry) => nextRecoveryMs(entry.type, getQuota(entry), sortNow),
    [getQuota, sortNow]
  );
  // Sort before paginating, otherwise "soonest recovery" would only hold within a page.
  const sortedEntries = useMemo(
    () => sortQuotaEntries(filteredEntries, sortMode, resolveNextRecovery),
    [filteredEntries, sortMode, resolveNextRecovery]
  );

  const { pageItems, currentPage, totalPages } = useMemo(
    () => paginate(sortedEntries, page, QUOTA_PAGE_SIZE),
    [sortedEntries, page]
  );

  const handleTabChange = useCallback((next: string) => {
    setTab(next as QuotaTabId);
    setPage(1);
    writeQuotaUiState({ tab: next as QuotaTabId });
  }, []);

  const handleSortModeChange = useCallback((next: string) => {
    setSortMode(next as QuotaSortMode);
    setPage(1);
    writeQuotaUiState({ sortMode: next as QuotaSortMode });
  }, []);

  const sortOptions = useMemo(
    () =>
      QUOTA_SORT_MODES.map((mode) => ({ value: mode, label: t(`quota_management.sort_${mode}`) })),
    [t]
  );

  const { loadedCount, attentionCount } = useMemo(() => {
    let loaded = 0;
    let attention = 0;
    entries.forEach((entry) => {
      const status = quotaByType[entry.type][getQuotaCacheKey(entry.file)]?.status;
      if (status === 'success') loaded += 1;
      else if (status === 'error') attention += 1;
    });
    return { loadedCount: loaded, attentionCount: attention };
  }, [entries, quotaByType]);

  // Prune: once the file list settles, each provider cache keeps only existing credentials.
  useEffect(() => {
    if (loading || error || filesGeneration !== sessionGeneration) return;
    const survivorsByType = new Map<QuotaProviderType, Set<string>>(
      QUOTA_TAB_ORDER.map((type) => [type, new Set<string>()])
    );
    entries.forEach((entry) => survivorsByType.get(entry.type)?.add(getQuotaCacheKey(entry.file)));

    QUOTA_TAB_ORDER.forEach((type) => {
      const survivors = survivorsByType.get(type) ?? new Set<string>();
      const setQuota = getQuotaSetter(QUOTA_ADAPTERS[type]);
      setQuota((prev) => {
        const staleKeys = Object.keys(prev).filter((name) => !survivors.has(name));
        if (staleKeys.length === 0) return prev;
        const next = { ...prev };
        staleKeys.forEach((name) => delete next[name]);
        return next;
      });
    });

    // Local quota history follows the same rule: deleted credentials are dropped.
    const historyIds = new Set(
      entries
        .filter((entry) => isPooledProvider(entry.type))
        .map((entry) =>
          historyCredentialId(entry.type as PooledProvider, getQuotaCacheKey(entry.file))
        )
    );
    pruneQuotaHistory(historyIds);
    // The one-time server import only sends readings of credentials that still exist.
    if (filesRevision !== null) quotaHistorySync.setKnownCredentials(filesRevision, historyIds);
  }, [entries, error, filesGeneration, filesRevision, loading, sessionGeneration]);

  /* ---------- Loading and actions ---------- */

  const { batchLoading, loadQuota } = useQuotaBatchLoader();
  const { resettingQuotaName, refreshQuota, resetQuota } = useQuotaActions(disableControls);

  const pendingRefreshRef = useRef<number | null>(null);
  const prevLoadingRef = useRef(loading);

  // Refresh all: reload the file list first, then batch-load the current page once it settles.
  const handleRefreshAll = useCallback(() => {
    if (disableControls) return;
    pendingRefreshRef.current = sessionGeneration;
    void loadFiles();
  }, [disableControls, loadFiles, sessionGeneration]);

  useEffect(() => {
    const wasLoading = prevLoadingRef.current;
    prevLoadingRef.current = loading;

    const requestedSession = pendingRefreshRef.current;
    if (requestedSession === null) return;
    if (requestedSession !== sessionGeneration) {
      pendingRefreshRef.current = null;
      return;
    }
    if (loading || !wasLoading) return;

    pendingRefreshRef.current = null;
    if (
      canRefreshQuotaAfterList(
        requestedSession,
        sessionGeneration,
        filesGeneration,
        Boolean(error),
        disableControls
      )
    ) {
      void loadQuota(pageItems);
    }
  }, [disableControls, error, filesGeneration, loading, loadQuota, pageItems, sessionGeneration]);

  useDevinQuotaAutoLoad(
    pageItems,
    disableControls ||
      loading ||
      batchLoading ||
      Boolean(error) ||
      filesGeneration !== sessionGeneration,
    loadQuota
  );

  const canUseActions = !disableControls && !loading && filesGeneration === sessionGeneration;

  /* ---------- Subscription pools ---------- */

  const { local: localHistory, merged: history, tokens: tokenUsage } = useQuotaHistory();
  const now = useNow();
  const span = useMemo(() => poolSpan(rangeOffset, now), [rangeOffset, now]);
  const days = useMemo(() => poolDays(span), [span]);

  const pooledEntries = useMemo(
    () =>
      entries.filter((entry): entry is QuotaFileEntry & { type: PooledProvider } =>
        isPooledProvider(entry.type)
      ),
    [entries]
  );
  const pooledHistoryIds = useMemo(
    () =>
      new Set(
        pooledEntries.map((entry) => historyCredentialId(entry.type, getQuotaCacheKey(entry.file)))
      ),
    [pooledEntries]
  );
  const visiblePooledProviders = useMemo(
    () =>
      POOLED_PROVIDERS.filter(
        (provider) =>
          (tab === 'all' || tab === provider) &&
          pooledEntries.some((entry) => entry.type === provider)
      ),
    [pooledEntries, tab]
  );

  const pools = useMemo(
    () =>
      visiblePooledProviders.map((provider) => {
        const providerEntries = pooledEntries
          .filter((entry) => entry.type === provider)
          .map((entry) => {
            const key = getQuotaCacheKey(entry.file);
            const current = quotaByType[provider][key];
            // A refresh in flight clears Codex windows; keep the last good read meanwhile.
            const quota = hasWindows(current)
              ? (current as unknown as PooledQuotaState)
              : getLastSuccessfulQuota(provider, key);
            const observation = getQuotaObservation(quota);
            return {
              key,
              label: getQuotaDisplayName(entry.file),
              file: entry.file,
              quota,
              observedAtMs: observation?.atMs ?? null,
              observedByMs: observation?.byMs ?? null,
            };
          });
        return buildProviderPool(provider, providerEntries, history, {
          now,
          span,
          burnWindowHours,
          futureUsage,
        });
      }),
    [
      burnWindowHours,
      futureUsage,
      history,
      now,
      pooledEntries,
      quotaByType,
      span,
      visiblePooledProviders,
    ]
  );

  const handlePoolModeChange = useCallback((next: PoolMode) => {
    setPoolMode(next);
    writeQuotaUiState({ poolMode: next });
  }, []);
  const handleBurnWindowChange = useCallback((next: BurnWindowHours) => {
    setBurnWindowHours(next);
    writeQuotaUiState({ burnWindowHours: next });
  }, []);
  const handleFutureUsageChange = useCallback((next: FutureUsage) => {
    setFutureUsage(next);
    writeQuotaUiState({ futureUsage: next });
  }, []);
  const handleToggleAccountLines = useCallback(() => {
    setShowAccountLines((value) => {
      writeQuotaUiState({ showAccountLines: !value });
      return !value;
    });
  }, []);
  const handleAutoReadChange = useCallback((next: AutoReadMinutes) => {
    setAutoReadMinutes(next);
    writeQuotaUiState({ autoReadMinutes: next });
  }, []);

  // Auto-read: read Claude/Codex quota through the server on an interval while the page
  // is visible. Reads only; resetQuota (credit redemption) is never called from here.
  // Due-ness follows this browser's own reads: server samples do not refresh the cards.
  const historyRef = useRef(localHistory);
  useEffect(() => {
    historyRef.current = localHistory;
  }, [localHistory]);

  const autoReadReady =
    autoReadMinutes > 0 &&
    !disableControls &&
    !loading &&
    !error &&
    filesGeneration === sessionGeneration;

  useEffect(() => {
    if (!autoReadReady) return;
    const targets = pooledEntries.filter((entry) => !entry.file.disabled);
    if (targets.length === 0) return;
    const intervalMs = autoReadMinutes * 60 * 1000;
    const generation = useQuotaStore.getState().cacheGeneration;

    const isDue = () =>
      targets.some((entry) => {
        const series = seriesFor(
          historyRef.current,
          entry.type,
          getQuotaCacheKey(entry.file),
          WEEKLY_WINDOW_ID[entry.type]
        );
        const last = series ? series.points[series.points.length - 1].atMs : null;
        return last === null || Date.now() - last >= intervalMs - 5000;
      });

    const run = async () => {
      if (typeof document !== 'undefined' && document.visibilityState !== 'visible') return;
      if (!isDue()) return;
      // One page at a time keeps upstream concurrency bounded.
      for (let index = 0; index < targets.length; index += QUOTA_PAGE_SIZE) {
        if (useQuotaStore.getState().cacheGeneration !== generation) return;
        await loadQuota(targets.slice(index, index + QUOTA_PAGE_SIZE));
      }
    };

    void run();
    const timer = window.setInterval(() => void run(), intervalMs);
    const onVisible = () => {
      if (document.visibilityState === 'visible') void run();
    };
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, [autoReadMinutes, autoReadReady, loadQuota, pooledEntries]);

  /* ---------- One-time card entrance cascade ----------
   * Flip cardsAnimated right after the first batch renders. Mounted cards captured
   * their delay at mount (useState init in QuotaCard); cards mounted later by tab
   * switches, paging or refreshes get null and do not replay. */

  const [cardsAnimated, setCardsAnimated] = useState(false);
  const enableCardEntrance = !cardsAnimated && !loading && pageItems.length > 0;
  useEffect(() => {
    if (enableCardEntrance) {
      setCardsAnimated(true);
    }
  }, [enableCardEntrance]);
  const cardEntranceDelay = (index: number): number | null => {
    if (!enableCardEntrance) return null;
    if (pageItems.length <= 1) return 0;
    return Math.round((index / (pageItems.length - 1)) * CARD_ENTRANCE_BUDGET_MS);
  };

  /* ---------- Render ---------- */

  const isEmpty = !loading && filteredEntries.length === 0;
  const showPools = !loading && pools.length > 0;

  return (
    <div className={styles.page} ref={revealRef}>
      <QuotaHeader
        totalCount={entries.length}
        loadedCount={loadedCount}
        attentionCount={attentionCount}
        refreshing={loading || batchLoading}
        disableControls={disableControls}
        onRefreshAll={handleRefreshAll}
      />

      <section className={styles.workbench}>
        {/* Provider navigation and the search toolbar sit on separate rows so they do not compete. */}
        <div className={styles.tabsRow} data-reveal>
          <ProviderTabs
            types={TAB_IDS}
            counts={tabCounts}
            active={tab}
            resolvedTheme={resolvedTheme}
            onChange={handleTabChange}
          />
        </div>

        {showPools && (
          <section className={styles.pools} aria-label={t('quota_pool.title')}>
            <div className={styles.poolsHeader}>
              <div className={styles.poolsHeading}>
                <h2 className={styles.poolsTitle}>{t('quota_pool.title')}</h2>
              </div>
              <Button variant="secondary" size="sm" onClick={clearQuotaHistory}>
                {t('quota_pool.clear_history')}
              </Button>
            </div>

            <QuotaControls
              mode={poolMode}
              burnWindowHours={burnWindowHours}
              futureUsage={futureUsage}
              showAccountLines={showAccountLines}
              autoReadMinutes={autoReadMinutes}
              rangeLabel={t('quota_pool.range', {
                from: formatDate(span.startMs, locale),
                to: formatDate(span.endMs - 1, locale),
              })}
              isCurrentRange={rangeOffset === 0}
              onModeChange={handlePoolModeChange}
              onBurnWindowChange={handleBurnWindowChange}
              onFutureUsageChange={handleFutureUsageChange}
              onToggleAccountLines={handleToggleAccountLines}
              onAutoReadChange={handleAutoReadChange}
              onPrevRange={() => setRangeOffset((value) => value - 1)}
              onNextRange={() => setRangeOffset((value) => value + 1)}
              onToday={() => setRangeOffset(0)}
            />

            <div className={styles.poolPanels}>
              {pools.map((pool) => (
                <QuotaPoolPanel
                  key={pool.provider}
                  providerLabel={getTypeLabel(t, pool.provider)}
                  accent={POOL_ACCENTS[pool.provider]}
                  pool={pool}
                  mode={poolMode}
                  burnWindowHours={burnWindowHours}
                  futureUsageLabel={t(`quota_pool.future_${futureUsage}`)}
                  span={span}
                  days={days}
                  now={now}
                  showAccountLines={showAccountLines}
                  history={{
                    ...historyCoverage(
                      history,
                      pool.provider,
                      WEEKLY_WINDOW_ID[pool.provider],
                      pooledHistoryIds
                    ),
                    startedAtMs: history.startedAtMs,
                  }}
                  routingStrategy={routingStrategy}
                  tokens={
                    tokenUsage ? (tokenUsage.providers[pool.provider] ?? emptyTokenTotals()) : null
                  }
                />
              ))}
            </div>
          </section>
        )}

        <div className={styles.toolbar}>
          <div className={styles.search}>
            <IconSearch size={16} className={styles.searchIcon} aria-hidden="true" />
            <input
              ref={searchInputRef}
              className={styles.searchInput}
              type="search"
              value={search}
              onChange={(event) => handleSearchChange(event.target.value)}
              placeholder={t('quota_management.search_placeholder')}
              aria-label={t('quota_management.search_label')}
            />
            {search && (
              <button
                type="button"
                className={styles.clearSearch}
                aria-label={t('quota_management.search_clear')}
                title={t('quota_management.search_clear')}
                onClick={() => {
                  handleSearchChange('');
                  searchInputRef.current?.focus();
                }}
              >
                <IconX size={14} aria-hidden="true" />
              </button>
            )}
          </div>
          <div className={styles.sort}>
            <Select
              value={sortMode}
              options={sortOptions}
              onChange={handleSortModeChange}
              ariaLabel={t('quota_management.sort_label')}
              size="sm"
            />
          </div>
        </div>

        {error && (
          <div className={styles.errorBanner} role="alert">
            {error}
          </div>
        )}

        {loading ? (
          <div className={styles.grid} aria-hidden="true">
            {Array.from({ length: SKELETON_CARD_COUNT }, (_, index) => (
              <Skeleton key={index} height={168} rounded={14} />
            ))}
          </div>
        ) : isEmpty ? (
          <EmptyState
            title={
              search.trim()
                ? t('quota_management.search_empty_title')
                : tab === 'all'
                  ? t('quota_management.empty_title')
                  : t(`${QUOTA_ADAPTERS[tab].i18nPrefix}.empty_title`)
            }
            description={
              search.trim()
                ? t('quota_management.search_empty_desc')
                : tab === 'all'
                  ? t('quota_management.empty_desc')
                  : t(`${QUOTA_ADAPTERS[tab].i18nPrefix}.empty_desc`)
            }
            action={
              search.trim() ? (
                <Button variant="secondary" size="sm" onClick={() => handleSearchChange('')}>
                  {t('quota_management.search_clear')}
                </Button>
              ) : tab === 'all' ? undefined : (
                <Button variant="secondary" size="sm" onClick={() => handleTabChange('all')}>
                  {t('auth_files.filter_all')}
                </Button>
              )
            }
          />
        ) : (
          <div className={styles.grid}>
            {pageItems.map((entry, index) => (
              <QuotaCard
                key={`${entry.type}:${getQuotaCacheKey(entry.file)}`}
                entry={entry}
                quota={getQuota(entry)}
                resolvedTheme={resolvedTheme}
                canRefresh={canUseActions && !entry.file.disabled}
                resetting={resettingQuotaName === getQuotaCacheKey(entry.file)}
                entranceDelayMs={cardEntranceDelay(index)}
                onRefresh={() => void refreshQuota(entry.file, QUOTA_ADAPTERS[entry.type])}
                onReset={() => resetQuota(entry.file, QUOTA_ADAPTERS[entry.type])}
              />
            ))}
          </div>
        )}

        {!loading && filteredEntries.length > QUOTA_PAGE_SIZE && (
          <div className={styles.pagination}>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => setPage(Math.max(1, currentPage - 1))}
              disabled={currentPage <= 1}
            >
              {t('auth_files.pagination_prev')}
            </Button>
            <div className={styles.pageInfo}>
              {t('auth_files.pagination_info', {
                current: currentPage,
                total: totalPages,
                count: filteredEntries.length,
              })}
            </div>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => setPage(Math.min(totalPages, currentPage + 1))}
              disabled={currentPage >= totalPages}
            >
              {t('auth_files.pagination_next')}
            </Button>
          </div>
        )}

        {/* The timeline only compares current-page credentials so many credentials do not create unbounded lanes. */}
        <QuotaTimeline
          entries={pageItems}
          quotaFor={getQuota}
          displayNameFor={displayNameFor}
          resolvedTheme={resolvedTheme}
        />
      </section>
    </div>
  );
}
