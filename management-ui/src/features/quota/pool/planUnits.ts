/**
 * Base-plan accounting policy for pooled subscription quota.
 *
 * One unit is the weekly allowance of the provider's base plan. An account's
 * remaining allowance is `remaining fraction × units`, and pools add units, not
 * percentages: 25% of a Pro 200 (20 units) is 5 units, more than 40% of a
 * Pro 100 (4 units).
 *
 * The table mirrors `SubscriptionCapacity` in the server
 * (internal/runtime/executor/helps/subscription_quota.go) so the dashboard ranks
 * accounts the same way the soonest-reset-first router does. Unknown plans count
 * as one unit there too.
 *
 * React-free so tests can import it directly.
 */

import type { QuotaProviderType } from '../providers/types';

export interface PlanUnits {
  /** Weekly allowance in base-plan units. */
  units: number;
  /** Short plan name for display, or null when the plan is unknown. */
  planLabel: string | null;
  /** True when the plan was not recognised and the one-unit default applies. */
  assumed: boolean;
}

const squash = (value: string | null | undefined): string =>
  (value ?? '').toLowerCase().replace(/[\s_-]/g, '');

const CODEX_UNITS: Record<string, { units: number; label: string }> = {
  plus: { units: 1, label: 'Plus' },
  prolite: { units: 10, label: 'Pro 100' },
  pro100: { units: 10, label: 'Pro 100' },
  pro: { units: 20, label: 'Pro 200' },
  pro200: { units: 20, label: 'Pro 200' },
  promax: { units: 50, label: 'Pro 500' },
  pro500: { units: 50, label: 'Pro 500' },
};

// The dashboard reads Claude tiers from the profile's rate_limit_tier, mapped to
// plan_max5 / plan_max20 by providers/claude/data.ts. The raw tier strings are
// accepted as well so values copied from credential metadata also resolve.
const CLAUDE_UNITS: Record<string, { units: number; label: string }> = {
  planpro: { units: 1, label: 'Pro' },
  pro: { units: 1, label: 'Pro' },
  planmax5: { units: 5, label: 'Max 5x' },
  max5x: { units: 5, label: 'Max 5x' },
  defaultclaudemax5x: { units: 5, label: 'Max 5x' },
  planmax20: { units: 10, label: 'Ultra' },
  max20x: { units: 10, label: 'Ultra' },
  max10x: { units: 10, label: 'Ultra' },
  defaultclaudemax20x: { units: 10, label: 'Ultra' },
};

export function planUnitsFor(
  provider: QuotaProviderType,
  planType: string | null | undefined
): PlanUnits {
  const key = squash(planType);
  const table = provider === 'codex' ? CODEX_UNITS : provider === 'claude' ? CLAUDE_UNITS : null;
  const hit = table && key ? table[key] : undefined;
  if (hit) return { units: hit.units, planLabel: hit.label, assumed: false };
  // An undifferentiated Claude "Max" (only the account flag is known) stays at
  // one unit, exactly like the router, instead of guessing 5x or 20x.
  const fallbackLabel =
    provider === 'claude' && key === 'planmax' ? 'Max (tier unknown)' : planType?.trim() || null;
  return { units: 1, planLabel: fallbackLabel, assumed: true };
}

/** Providers whose pools and routing order the dashboard models. */
export const POOLED_PROVIDERS = ['claude', 'codex'] as const satisfies readonly QuotaProviderType[];
export type PooledProvider = (typeof POOLED_PROVIDERS)[number];

export const isPooledProvider = (type: string): type is PooledProvider =>
  (POOLED_PROVIDERS as readonly string[]).includes(type);
