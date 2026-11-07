import {
  QUOTA_SORT_MODES,
  QUOTA_TAB_ORDER,
  type QuotaSortMode,
  type QuotaTabId,
} from './constants';
import {
  BURN_WINDOW_HOURS,
  FUTURE_USAGE_OPTIONS,
  POOL_MODES,
  type BurnWindowHours,
  type FutureUsage,
  type PoolMode,
} from './pool/poolModel';

/** Minutes between automatic Claude/Codex quota reads; 0 turns auto-read off. */
export const AUTO_READ_MINUTES = [0, 2, 5, 15] as const;
export type AutoReadMinutes = (typeof AUTO_READ_MINUTES)[number];
export const DEFAULT_AUTO_READ_MINUTES: AutoReadMinutes = 5;

/** Quota page UI preferences, persisted per browser session (sessionStorage). */
export type QuotaUiState = {
  tab?: QuotaTabId;
  sortMode?: QuotaSortMode;
  poolMode?: PoolMode;
  burnWindowHours?: BurnWindowHours;
  futureUsage?: FutureUsage;
  showAccountLines?: boolean;
  autoReadMinutes?: AutoReadMinutes;
};

const QUOTA_UI_STATE_KEY = 'quotaPage.uiState';

const QUOTA_TAB_ID_SET = new Set<string>(['all', ...QUOTA_TAB_ORDER]);
const QUOTA_SORT_MODE_SET = new Set<string>(QUOTA_SORT_MODES);

export const isQuotaTabId = (value: unknown): value is QuotaTabId =>
  typeof value === 'string' && QUOTA_TAB_ID_SET.has(value);

export const isQuotaSortMode = (value: unknown): value is QuotaSortMode =>
  typeof value === 'string' && QUOTA_SORT_MODE_SET.has(value);

const isOneOf = <T>(options: readonly T[], value: unknown): value is T =>
  options.includes(value as T);

export const readQuotaUiState = (): QuotaUiState | null => {
  if (typeof window === 'undefined') return null;
  try {
    const raw = window.sessionStorage.getItem(QUOTA_UI_STATE_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as QuotaUiState;
    if (!parsed || typeof parsed !== 'object') return null;
    return {
      tab: isQuotaTabId(parsed.tab) ? parsed.tab : undefined,
      sortMode: isQuotaSortMode(parsed.sortMode) ? parsed.sortMode : undefined,
      poolMode: isOneOf(POOL_MODES, parsed.poolMode) ? parsed.poolMode : undefined,
      burnWindowHours: isOneOf(BURN_WINDOW_HOURS, parsed.burnWindowHours)
        ? parsed.burnWindowHours
        : undefined,
      futureUsage: isOneOf(FUTURE_USAGE_OPTIONS, parsed.futureUsage)
        ? parsed.futureUsage
        : undefined,
      showAccountLines:
        typeof parsed.showAccountLines === 'boolean' ? parsed.showAccountLines : undefined,
      autoReadMinutes: isOneOf(AUTO_READ_MINUTES, parsed.autoReadMinutes)
        ? parsed.autoReadMinutes
        : undefined,
    };
  } catch {
    return null;
  }
};

/**
 * Merge into whatever is already stored.
 *
 * Callers write one preference at a time — the tab strip knows nothing about
 * the sort control — so a whole-object write would silently drop the other
 * field every time either one changed.
 */
export const writeQuotaUiState = (state: QuotaUiState) => {
  if (typeof window === 'undefined') return;
  try {
    const next = { ...readQuotaUiState(), ...state };
    window.sessionStorage.setItem(QUOTA_UI_STATE_KEY, JSON.stringify(next));
  } catch {
    // ignore
  }
};
