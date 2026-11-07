/**
 * Weekly subscription percentages, normalized to the largest plan in the pool.
 * Recorded and projected usage share the same reference capacity.
 *
 * Adapted from yazinsai/prox (MIT). History is the server's weekly record merged with
 * this browser's reads; see pool/useQuotaHistory.ts. Token totals sit in their own row.
 */

import { useMemo } from 'react';
import type { CSSProperties } from 'react';
import { useTranslation } from 'react-i18next';
import { drainSeries, niceCeiling, type PoolMode } from '../pool/poolModel';
import type { ProviderPool } from '../pool/buildPools';
import { formatTokenCount, type TokenTotals } from '../pool/tokenUsage';
import {
  formatDate,
  formatDayLabel,
  formatDuration,
  formatPoolPercent,
  formatPoolRate,
  formatWhen,
} from '../pool/poolFormat';
import { QuotaPoolChart } from './QuotaPoolChart';
import styles from './QuotaPoolPanel.module.scss';

export interface QuotaPoolPanelProps {
  providerLabel: string;
  accent: string;
  pool: ProviderPool;
  mode: PoolMode;
  burnWindowHours: number;
  futureUsageLabel: string;
  span: { startMs: number; endMs: number };
  days: number[];
  now: number;
  showAccountLines: boolean;
  history: { oldestMs: number | null; readings: number; startedAtMs: number | null };
  /** Configured routing.strategy when known. */
  routingStrategy: string | null;
  /** Server token totals for the last 24h; null hides the row. */
  tokens?: TokenTotals | null;
}

export function QuotaPoolPanel(props: QuotaPoolPanelProps) {
  const {
    providerLabel,
    accent,
    pool,
    mode,
    burnWindowHours,
    span,
    days,
    now,
    showAccountLines,
    tokens,
  } = props;
  const { summary } = pool;
  const { t, i18n } = useTranslation();
  const locale = i18n.resolvedLanguage;
  const isDrain = mode === 'drain';
  const referenceUnits = Math.max(
    1,
    ...summary.accounts.map((account) => account.units),
    ...pool.routing.map((entry) => entry.units)
  );
  const referencePlan =
    pool.routing.find((entry) => entry.units === referenceUnits)?.planLabel ??
    summary.accounts.find((account) => account.units === referenceUnits)?.planLabel;
  const percent = (value: number) => formatPoolPercent(value, referenceUnits);

  const drain = useMemo(
    () => (isDrain ? drainSeries(summary.recorded, summary.projected) : null),
    [isDrain, summary.recorded, summary.projected]
  );
  const recorded = drain ? drain.recorded : summary.recorded;
  const projected = drain ? drain.projected : summary.projected;
  const yMax = drain
    ? (niceCeiling(
        (Math.max(0, ...recorded.map((p) => p.value), ...projected.map((p) => p.value)) * 100) /
          referenceUnits
      ) *
        referenceUnits) /
      100
    : Math.max(referenceUnits, summary.capacity);

  const hasData = summary.total !== null;
  const rate = (value: number) => formatPoolRate(value, referenceUnits);

  const poolEmptyValue =
    summary.poolEmptyAtMs === null
      ? t('quota_pool.empty_never', { date: formatDate(span.endMs - 1, locale) })
      : summary.poolEmptyAtMs <= now
        ? t('quota_pool.empty_now')
        : formatWhen(summary.poolEmptyAtMs, now, locale);

  const stats = hasData
    ? [
        { key: 'pool', label: t('quota_pool.pool_empty'), value: poolEmptyValue },
        {
          key: 'first',
          label: t('quota_pool.first_empty'),
          value: summary.firstEmpty ? (
            <>
              <span className={styles.identity} title={summary.firstEmpty.account.label}>
                {summary.firstEmpty.account.label}
              </span>
              {summary.firstEmpty.atMs <= now
                ? t('quota_pool.empty_now')
                : t('quota_pool.in_duration', {
                    duration: formatDuration(summary.firstEmpty.atMs - now, locale),
                  })}
            </>
          ) : (
            t('quota_pool.none')
          ),
        },
        {
          key: 'reset',
          label: t('quota_pool.next_reset'),
          value: summary.nextReset
            ? `+${percent(summary.nextReset.added)} · ${formatWhen(summary.nextReset.atMs, now, locale)}`
            : t('quota_pool.none'),
        },
        {
          key: 'burn',
          label: t('quota_pool.burn', { hours: burnWindowHours }),
          value: summary.burnPerHour === null ? '-' : rate(summary.burnPerHour),
        },
      ]
    : [];

  const describe = (atMs: number, value: number, isProjected: boolean) => {
    const kind = t(isProjected ? 'quota_pool.kind_projected' : 'quota_pool.kind_recorded');
    const when = formatWhen(atMs, now, locale);
    return `${when} · ${isDrain ? rate(value) : percent(value)} · ${kind}`;
  };

  return (
    <section className={styles.panel} style={{ '--pool-accent': accent } as CSSProperties}>
      <div className={styles.top}>
        <div className={styles.titleRow}>
          <span className={styles.swatch} aria-hidden="true" />
          <h2 className={styles.title}>{providerLabel}</h2>
          <span className={styles.count}>
            {t('quota_pool.accounts', { count: pool.accountCount })}
          </span>
        </div>

        <div className={styles.figures}>
          <div className={styles.headline}>
            <span className={hasData ? styles.total : `${styles.total} ${styles.totalEmpty}`}>
              {hasData ? percent(summary.total as number) : '-'}
            </span>
            <span className={styles.capacity}>
              {hasData ? `/ ${percent(summary.capacity)}` : t('quota_pool.not_loaded')}
            </span>
          </div>
          {stats.map((stat) => (
            <div key={stat.key} className={styles.stat}>
              <span className={styles.statLabel}>{stat.label}</span>
              <span className={styles.statValue}>{stat.value}</span>
            </div>
          ))}
        </div>

        {tokens && (
          <div className={styles.tokens} aria-label={t('quota_pool.tokens_24h')}>
            <span className={styles.tokensLabel}>{t('quota_pool.tokens_24h')}</span>
            {(
              [
                ['total', tokens.totalTokens],
                ['input', tokens.inputTokens],
                ['output', tokens.outputTokens],
                ['cache_read', tokens.cacheReadTokens],
                ['cache_write', tokens.cacheWriteTokens],
              ] as const
            ).map(([key, value]) => (
              <span key={key} className={styles.tokenItem}>
                {t(`quota_pool.tokens_${key}`)}
                <span className={styles.tokenValue} title={value.toLocaleString(locale)}>
                  {formatTokenCount(value, locale)}
                </span>
              </span>
            ))}
          </div>
        )}
      </div>

      {hasData && (
        <QuotaPoolChart
          span={span}
          days={days}
          now={now}
          yMax={yMax}
          accent={accent}
          recorded={recorded}
          projected={projected}
          jumps={isDrain ? [] : [...summary.recordedJumps, ...summary.projectedJumps]}
          accountLines={showAccountLines && !isDrain ? summary.accountLines : []}
          emptyMarker={
            !isDrain && summary.poolEmptyAtMs !== null && summary.poolEmptyAtMs > now
              ? {
                  atMs: summary.poolEmptyAtMs,
                  label: t('quota_pool.empty_marker', {
                    when: formatWhen(summary.poolEmptyAtMs, now, locale),
                  }),
                }
              : null
          }
          formatAxis={(value) => (isDrain ? rate(value) : percent(value))}
          formatJump={(added) => `+${percent(added)}`}
          formatDay={(ms) => formatDayLabel(ms, locale)}
          describe={describe}
          nowLabel={t('quota_pool.now')}
          ariaLabel={`${providerLabel}: ${percent(summary.total as number)} / ${percent(summary.capacity)}`}
        />
      )}

      {hasData && (
        <div className={styles.legend}>
          <span className={styles.legendItem}>
            <span className={styles.legendSolid} aria-hidden="true" />
            {t('quota_pool.kind_recorded')}
          </span>
          <span className={styles.legendItem}>
            <svg width="18" height="2" viewBox="0 0 18 2" aria-hidden="true">
              <line x1="0" x2="18" y1="1" y2="1" className={styles.legendDash} />
            </svg>
            {t('quota_pool.kind_projected')}
          </span>
          <span className={styles.legendNote}>
            {referencePlan ? `100% = ${referencePlan}` : '100% = largest plan'}
          </span>
        </div>
      )}
    </section>
  );
}
