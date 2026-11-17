/**
 * Server-trained pool demand forecast (`GET /credentials/quota/forecast`).
 *
 * The server retrains every hour on its quota and token history. It backtests
 * a 24h average against two neural networks (one on quota spend, one on
 * weighted tokens converted to quota units) and serves whichever had the
 * lowest error over recent days. Rates are base-plan units per hour, one per
 * hour from `startMs`.
 */

import type { PooledProvider } from './planUnits';

export const FORECAST_METHODS = ['average', 'neural-spend', 'neural-tokens'] as const;
export type ForecastMethod = (typeof FORECAST_METHODS)[number];

export interface ServerForecast {
  startMs: number;
  stepMs: number;
  rates: number[];
  method: ForecastMethod;
  /** Mean absolute error of next-24h spend per method, units. */
  backtestMae: Partial<Record<ForecastMethod, number>>;
  backtestDays: number;
  samples: number;
  /** Holdout error reduction of the served network vs a 24h average. */
  skill: number | null;
  generation: number;
  trainedAtMs: number;
}

export type ServerForecasts = Partial<Record<PooledProvider, ServerForecast>>;
export type ServerForecastResponse =
  { enabled: false } | { enabled: true; providers: ServerForecasts };

const finite = (value: unknown): value is number =>
  typeof value === 'number' && Number.isFinite(value);

function parseOne(raw: unknown): ServerForecast | null {
  if (!raw || typeof raw !== 'object') return null;
  const body = raw as Record<string, unknown>;
  const method = FORECAST_METHODS.find((candidate) => candidate === body.method);
  if (!method || !finite(body.startMs) || !finite(body.stepMs) || !(body.stepMs > 0)) return null;
  if (!Array.isArray(body.rates) || body.rates.length === 0) return null;
  if (!body.rates.every((rate) => finite(rate) && rate >= 0)) return null;
  const backtestMae: ServerForecast['backtestMae'] = {};
  if (body.backtestMae && typeof body.backtestMae === 'object') {
    for (const candidate of FORECAST_METHODS) {
      const value = (body.backtestMae as Record<string, unknown>)[candidate];
      if (finite(value) && value >= 0) backtestMae[candidate] = value;
    }
  }
  return {
    startMs: body.startMs,
    stepMs: body.stepMs,
    rates: body.rates as number[],
    method,
    backtestMae,
    backtestDays: finite(body.backtestDays) ? body.backtestDays : 0,
    samples: finite(body.samples) ? body.samples : 0,
    skill: finite(body.skill) ? body.skill : null,
    generation: finite(body.generation) ? body.generation : 0,
    trainedAtMs: finite(body.trainedAtMs) ? body.trainedAtMs : 0,
  };
}

/** Validate a response; null when it is not a recognisable forecast answer. */
export function parseServerForecast(payload: unknown): ServerForecastResponse | null {
  if (!payload || typeof payload !== 'object') return null;
  const body = payload as Record<string, unknown>;
  if (body.enabled === false) return { enabled: false };
  if (body.enabled !== true) return null;
  const providers: ServerForecasts = {};
  if (body.providers && typeof body.providers === 'object') {
    for (const provider of ['claude', 'codex'] as const) {
      const parsed = parseOne((body.providers as Record<string, unknown>)[provider]);
      if (parsed) providers[provider] = parsed;
    }
  }
  return { enabled: true, providers };
}
