/** Locale-aware labels for the quota pool panels (adapted from yazinsai/prox). */

import { DAY_MS, HOUR_MS } from './poolModel';
import type { PooledProvider } from './planUnits';

/** Provider accent for swatches and chart lines. */
export const POOL_ACCENTS: Record<PooledProvider, string> = {
  claude: '#e8795a',
  codex: '#6e9ef7',
};

const cache = new Map<string, Intl.DateTimeFormat>();

/** One full allowance on the largest configured plan is 100%. */
export const formatPoolPercent = (value: number, referenceUnits: number): string => {
  if (!Number.isFinite(value) || !Number.isFinite(referenceUnits) || referenceUnits <= 0)
    return '-';
  return `${Number(((value / referenceUnits) * 100).toFixed(1))}%`;
};

export const formatPoolRate = (value: number, referenceUnits: number): string => {
  if (!Number.isFinite(value) || !Number.isFinite(referenceUnits) || referenceUnits <= 0)
    return '-';
  return `${Number(((value / referenceUnits) * 100).toFixed(2))}%/h`;
};
const dateFormat = (locale: string | undefined, options: Intl.DateTimeFormatOptions) => {
  const key = `${locale ?? ''}|${JSON.stringify(options)}`;
  let format = cache.get(key);
  if (!format) {
    try {
      format = new Intl.DateTimeFormat(locale, options);
    } catch {
      format = new Intl.DateTimeFormat(undefined, options);
    }
    cache.set(key, format);
  }
  return format;
};

/** Base-plan units: one decimal below 10, whole numbers above. */
export const formatUnits = (value: number): string => {
  if (!Number.isFinite(value)) return '-';
  const abs = Math.abs(value);
  if (abs < 10) {
    const rounded = Math.round(value * 10) / 10;
    return Number.isInteger(rounded) ? String(rounded) : rounded.toFixed(1);
  }
  return String(Math.round(value));
};

/** Rates are small; keep two significant decimals below 1 unit/h. */
export const formatUnitRate = (value: number): string => {
  if (!Number.isFinite(value)) return '-';
  if (value > 0 && value < 1) return (Math.round(value * 100) / 100).toFixed(2);
  return formatUnits(value);
};

/** "Sun 4:39 PM" */
export const formatClock = (ms: number, locale?: string) =>
  dateFormat(locale, { weekday: 'short', hour: 'numeric', minute: '2-digit' }).format(ms);

/** "Nov 9" */
export const formatDate = (ms: number, locale?: string) =>
  dateFormat(locale, { month: 'short', day: 'numeric' }).format(ms);

/** "Nov 9, 4:39 PM" */
export const formatDateTime = (ms: number, locale?: string) =>
  dateFormat(locale, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' }).format(
    ms
  );

/** "Fri 23" */
export const formatDayLabel = (ms: number, locale?: string) => {
  const parts = dateFormat(locale, { weekday: 'short', day: 'numeric' }).formatToParts(ms);
  const weekday = parts.find((part) => part.type === 'weekday')?.value ?? '';
  const day = parts.find((part) => part.type === 'day')?.value ?? '';
  return `${weekday} ${day}`.trim();
};

/** Instant within the coming week as weekday + time, further out as a date. */
export const formatWhen = (ms: number, now: number, locale?: string) =>
  Math.abs(ms - now) < 6 * DAY_MS ? formatClock(ms, locale) : formatDate(ms, locale);

const unit = (value: number, name: 'day' | 'hour' | 'minute', locale?: string) => {
  try {
    return new Intl.NumberFormat(locale, {
      style: 'unit',
      unit: name,
      unitDisplay: 'narrow',
    }).format(value);
  } catch {
    return `${value}${name[0]}`;
  }
};

/** Compact duration: "2d 18h", "15h 49m", "12m". */
export const formatDuration = (ms: number, locale?: string) => {
  const minutes = Math.max(0, Math.round(ms / 60000));
  const days = Math.floor(minutes / (24 * 60));
  const hours = Math.floor((minutes % (24 * 60)) / 60);
  const mins = minutes % 60;
  if (days > 0) return `${unit(days, 'day', locale)} ${unit(hours, 'hour', locale)}`;
  if (ms >= HOUR_MS) return `${unit(hours, 'hour', locale)} ${unit(mins, 'minute', locale)}`;
  return unit(mins, 'minute', locale);
};
