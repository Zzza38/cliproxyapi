/**
 * Pool chart controls: mode, burn window, future usage, account lines, auto-read
 * interval and the week navigator. Adapted from yazinsai/prox (MIT); the
 * provider tabs stay the upstream ProviderTabs.
 */

import { useTranslation } from 'react-i18next';
import { IconChevronLeft } from '@/components/ui/icons';
import { Select } from '@/components/ui/Select';
import {
  BURN_WINDOW_HOURS,
  FUTURE_USAGE_OPTIONS,
  POOL_MODES,
  type BurnWindowHours,
  type FutureUsage,
  type PoolMode,
} from '../pool/poolModel';
import { AUTO_READ_MINUTES, type AutoReadMinutes } from '../uiState';
import styles from './QuotaControls.module.scss';

interface SegmentedProps<T extends string | number> {
  value: T;
  options: readonly T[];
  label: (value: T) => string;
  ariaLabel: string;
  onChange: (value: T) => void;
}

function Segmented<T extends string | number>(props: SegmentedProps<T>) {
  const { value, options, label, ariaLabel, onChange } = props;
  return (
    <div className={styles.segmented} role="group" aria-label={ariaLabel}>
      {options.map((option) => (
        <button
          key={option}
          type="button"
          className={styles.segment}
          aria-pressed={option === value}
          onClick={() => onChange(option)}
        >
          {label(option)}
        </button>
      ))}
    </div>
  );
}

export interface QuotaControlsProps {
  mode: PoolMode;
  burnWindowHours: BurnWindowHours;
  futureUsage: FutureUsage;
  showAccountLines: boolean;
  autoReadMinutes: AutoReadMinutes;
  rangeLabel: string;
  isCurrentRange: boolean;
  onModeChange: (mode: PoolMode) => void;
  onBurnWindowChange: (hours: BurnWindowHours) => void;
  onFutureUsageChange: (usage: FutureUsage) => void;
  onToggleAccountLines: () => void;
  onAutoReadChange: (minutes: AutoReadMinutes) => void;
  onPrevRange: () => void;
  onNextRange: () => void;
  onToday: () => void;
}

export function QuotaControls(props: QuotaControlsProps) {
  const { t } = useTranslation();
  const futureLabelId = 'quota-pool-future-label';
  const autoLabelId = 'quota-pool-auto-label';

  return (
    <div className={styles.controls}>
      <Segmented
        value={props.mode}
        options={POOL_MODES}
        label={(mode) => t(`quota_pool.mode_${mode}`)}
        ariaLabel={t('quota_pool.mode_label')}
        onChange={props.onModeChange}
      />

      <div className={styles.field}>
        <span className={styles.fieldLabel} aria-hidden="true">
          {t('quota_pool.burn_window')}
        </span>
        <Segmented
          value={props.burnWindowHours}
          options={BURN_WINDOW_HOURS}
          label={(hours) => t('quota_pool.burn_hours', { hours })}
          ariaLabel={t('quota_pool.burn_window')}
          onChange={props.onBurnWindowChange}
        />
      </div>

      <div className={styles.field}>
        <span id={futureLabelId} className={styles.fieldLabel}>
          {t('quota_pool.future_usage')}
        </span>
        <div className={styles.select}>
          <Select
            value={props.futureUsage}
            options={FUTURE_USAGE_OPTIONS.map((usage) => ({
              value: usage,
              label: t(`quota_pool.future_${usage}`),
            }))}
            onChange={(value) => props.onFutureUsageChange(value as FutureUsage)}
            ariaLabelledBy={futureLabelId}
            size="sm"
          />
        </div>
      </div>

      <button
        type="button"
        className={styles.toggle}
        aria-pressed={props.showAccountLines}
        onClick={props.onToggleAccountLines}
      >
        {t('quota_pool.account_lines')}
      </button>

      <div className={styles.field} title={t('quota_pool.auto_read_hint')}>
        <span id={autoLabelId} className={styles.fieldLabel}>
          {t('quota_pool.auto_read')}
        </span>
        <div className={styles.select}>
          <Select
            value={String(props.autoReadMinutes)}
            options={AUTO_READ_MINUTES.map((minutes) => ({
              value: String(minutes),
              label:
                minutes === 0
                  ? t('quota_pool.auto_read_off')
                  : t('quota_pool.auto_read_every', { minutes }),
            }))}
            onChange={(value) => props.onAutoReadChange(Number(value) as AutoReadMinutes)}
            ariaLabelledBy={autoLabelId}
            size="sm"
          />
        </div>
      </div>

      <div className={styles.range}>
        <span className={styles.rangeLabel}>{props.rangeLabel}</span>
        <div className={styles.rangeNav}>
          <button
            type="button"
            className={styles.iconButton}
            onClick={props.onPrevRange}
            aria-label={t('quota_pool.range_prev')}
          >
            <IconChevronLeft size={16} aria-hidden="true" />
          </button>
          <button
            type="button"
            className={styles.todayButton}
            onClick={props.onToday}
            disabled={props.isCurrentRange}
          >
            {t('quota_pool.today')}
          </button>
          <button
            type="button"
            className={styles.iconButton}
            onClick={props.onNextRange}
            aria-label={t('quota_pool.range_next')}
          >
            <IconChevronLeft size={16} className={styles.flip} aria-hidden="true" />
          </button>
        </div>
      </div>
    </div>
  );
}
