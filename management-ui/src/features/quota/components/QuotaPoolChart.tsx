/**
 * Pool chart: recorded line up to now, projected (dashed) line after it,
 * reset jumps labelled with what they restore. Layout only; the numbers come
 * from pool/poolModel.ts. Adapted from yazinsai/prox (MIT).
 */

import { useEffect, useId, useMemo, useRef, useState } from 'react';
import type { KeyboardEvent, PointerEvent } from 'react';
import { HOUR_MS, valueAt } from '../pool/poolModel';
import type { PoolJump, PoolPoint } from '../pool/poolModel';
import styles from './QuotaPoolPanel.module.scss';

const HEIGHT = 300;
const PLOT_LEFT = 48;
const PLOT_RIGHT_PAD = 10;
const PLOT_TOP = 24;
const PLOT_BOTTOM = 264;
const DEFAULT_WIDTH = 1062;
/** Reset labels closer than this (px) to a previous one are dropped. */
const LABEL_MIN_GAP = 30;

export interface QuotaPoolChartProps {
  span: { startMs: number; endMs: number };
  days: number[];
  now: number;
  yMax: number;
  accent: string;
  recorded: PoolPoint[];
  projected: PoolPoint[];
  jumps: PoolJump[];
  accountLines?: { key: string; points: PoolPoint[] }[];
  emptyMarker?: { atMs: number; label: string } | null;
  formatAxis: (value: number) => string;
  /** Label for a reset jump, e.g. "+20 u". */
  formatJump: (added: number) => string;
  formatDay: (ms: number) => string;
  /** Tooltip text for an instant; `projected` tells which line the value came from. */
  describe: (atMs: number, value: number, projected: boolean) => string;
  nowLabel: string;
  ariaLabel: string;
}

export function QuotaPoolChart(props: QuotaPoolChartProps) {
  const {
    span,
    days,
    now,
    yMax,
    accent,
    recorded,
    projected,
    jumps,
    accountLines = [],
    emptyMarker,
    formatAxis,
    formatJump,
    formatDay,
    describe,
    nowLabel,
    ariaLabel,
  } = props;
  const gradientId = useId().replace(/:/g, '');
  const wrapRef = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(DEFAULT_WIDTH);
  const [cursorMs, setCursorMs] = useState<number | null>(null);

  useEffect(() => {
    const node = wrapRef.current;
    if (!node || typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(([entry]) => {
      const next = Math.round(entry.contentRect.width);
      if (next > 0) setWidth(next);
    });
    observer.observe(node);
    return () => observer.disconnect();
  }, []);

  const plotRight = width - PLOT_RIGHT_PAD;
  const spanMs = span.endMs - span.startMs;
  const x = (ms: number) => PLOT_LEFT + ((ms - span.startMs) / spanMs) * (plotRight - PLOT_LEFT);
  const y = (value: number) =>
    PLOT_BOTTOM - (Math.min(yMax, Math.max(0, value)) / yMax) * (PLOT_BOTTOM - PLOT_TOP);
  const toPoints = (points: PoolPoint[]) =>
    points.map((p) => `${x(p.atMs).toFixed(1)},${y(p.value).toFixed(1)}`).join(' ');

  const nowVisible = now >= span.startMs && now <= span.endMs;
  const combined = useMemo(() => {
    if (recorded.length === 0) return projected;
    if (projected.length === 0) return recorded;
    return [...recorded, ...projected];
  }, [recorded, projected]);

  const area =
    combined.length > 1
      ? `M${x(combined[0].atMs).toFixed(1)},${PLOT_BOTTOM} L${toPoints(combined)} L${x(
          combined[combined.length - 1].atMs
        ).toFixed(1)},${PLOT_BOTTOM} Z`
      : null;

  const labels = useMemo(() => {
    const kept: { left: number; jump: PoolJump }[] = [];
    [...jumps]
      .sort((a, b) => a.atMs - b.atMs)
      .forEach((jump) => {
        const left = PLOT_LEFT + ((jump.atMs - span.startMs) / spanMs) * (plotRight - PLOT_LEFT);
        if (kept.length > 0 && left - kept[kept.length - 1].left < LABEL_MIN_GAP) return;
        kept.push({ left, jump });
      });
    return kept;
  }, [jumps, span.startMs, spanMs, plotRight]);

  const ticks = [yMax, yMax / 2, 0];
  const dayLabelStep = Math.max(1, Math.ceil((days.length * 64) / (plotRight - PLOT_LEFT)));

  /* ---------- cursor ---------- */

  const lineAt = (ms: number): { value: number; projected: boolean } | null => {
    const inRecorded =
      recorded.length > 0 && ms >= recorded[0].atMs && ms <= recorded[recorded.length - 1].atMs;
    if (inRecorded && (projected.length === 0 || ms < projected[0].atMs)) {
      return { value: valueAt(recorded, ms), projected: false };
    }
    if (
      projected.length > 0 &&
      ms >= projected[0].atMs &&
      ms <= projected[projected.length - 1].atMs
    ) {
      return { value: valueAt(projected, ms), projected: true };
    }
    return null;
  };

  const onPointerMove = (event: PointerEvent<SVGSVGElement>) => {
    const rect = event.currentTarget.getBoundingClientRect();
    const px = ((event.clientX - rect.left) / rect.width) * width;
    if (px < PLOT_LEFT || px > plotRight) {
      setCursorMs(null);
      return;
    }
    setCursorMs(span.startMs + ((px - PLOT_LEFT) / (plotRight - PLOT_LEFT)) * spanMs);
  };

  const onKeyDown = (event: KeyboardEvent<SVGSVGElement>) => {
    const step = 3 * HOUR_MS;
    if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') {
      event.preventDefault();
      const base = cursorMs ?? (nowVisible ? now : span.startMs);
      const next = base + (event.key === 'ArrowRight' ? step : -step);
      setCursorMs(Math.min(span.endMs, Math.max(span.startMs, next)));
    } else if (event.key === 'Escape') {
      setCursorMs(null);
    }
  };

  const hover = cursorMs !== null ? lineAt(cursorMs) : null;
  const tooltip =
    hover && cursorMs !== null ? describe(cursorMs, hover.value, hover.projected) : '';
  const tooltipWidth = Math.max(120, tooltip.length * 7 + 28);
  const tooltipLeft =
    cursorMs !== null
      ? Math.min(plotRight - tooltipWidth, Math.max(PLOT_LEFT, x(cursorMs) - tooltipWidth / 2))
      : 0;

  return (
    <div ref={wrapRef} className={styles.chartWrap}>
      <svg
        className={styles.chart}
        width={width}
        height={HEIGHT}
        viewBox={`0 0 ${width} ${HEIGHT}`}
        role="img"
        aria-label={ariaLabel}
        tabIndex={0}
        onPointerMove={onPointerMove}
        onPointerLeave={() => setCursorMs(null)}
        onKeyDown={onKeyDown}
        onBlur={() => setCursorMs(null)}
      >
        <defs>
          <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
            <stop offset="0" style={{ stopColor: accent, stopOpacity: 0.28 }} />
            <stop offset="1" style={{ stopColor: accent, stopOpacity: 0 }} />
          </linearGradient>
        </defs>

        {ticks.map((tick) => (
          <g key={tick}>
            <text x={PLOT_LEFT - 10} y={y(tick) + 4} textAnchor="end" className={styles.axisText}>
              {formatAxis(tick)}
            </text>
            <line
              x1={PLOT_LEFT}
              x2={plotRight}
              y1={y(tick)}
              y2={y(tick)}
              className={styles.gridLine}
            />
          </g>
        ))}

        {days.map((day, index) => {
          const next = days[index + 1] ?? span.endMs;
          return (
            <g key={day}>
              <line
                x1={x(day)}
                x2={x(day)}
                y1={PLOT_TOP}
                y2={PLOT_BOTTOM}
                className={styles.dayLine}
              />
              <text
                x={(x(day) + x(next)) / 2}
                y={288}
                textAnchor="middle"
                className={styles.dayText}
              >
                {index % dayLabelStep === 0 ? formatDay(day) : ''}
              </text>
            </g>
          );
        })}

        {area && <path d={area} fill={`url(#${gradientId})`} />}

        {accountLines.map((line) =>
          line.points.length > 1 ? (
            <polyline
              key={line.key}
              points={toPoints(line.points)}
              className={styles.accountLine}
              style={{ stroke: accent }}
            />
          ) : null
        )}

        {nowVisible && (
          <g>
            <line
              x1={x(now)}
              x2={x(now)}
              y1={PLOT_TOP - 6}
              y2={PLOT_BOTTOM + 10}
              className={styles.nowLine}
            />
            {!hover && (
              <text x={x(now)} y={14} textAnchor="middle" className={styles.nowText}>
                {nowLabel}
              </text>
            )}
          </g>
        )}

        {recorded.length > 1 && (
          <polyline
            points={toPoints(recorded)}
            className={styles.recordedLine}
            style={{ stroke: accent }}
          />
        )}
        {projected.length > 1 && (
          <polyline
            points={toPoints(projected)}
            className={styles.projectedLine}
            style={{ stroke: accent }}
          />
        )}
        {nowVisible && (recorded.length > 0 || projected.length > 0) && (
          <circle
            cx={x(now)}
            cy={y(valueAt(projected.length > 0 ? projected : recorded, now))}
            r={4}
            className={styles.nowDot}
            style={{ fill: accent }}
          />
        )}

        {labels.map(({ left, jump }) => (
          <text
            key={jump.atMs}
            x={left}
            y={Math.max(16, y(jump.value) - 8)}
            textAnchor="middle"
            className={styles.jumpText}
          >
            {formatJump(jump.added)}
          </text>
        ))}

        {emptyMarker && emptyMarker.atMs >= span.startMs && emptyMarker.atMs <= span.endMs && (
          <g>
            <circle cx={x(emptyMarker.atMs)} cy={PLOT_BOTTOM} r={6} className={styles.emptyDot} />
            <text
              x={Math.min(plotRight - 70, Math.max(PLOT_LEFT + 70, x(emptyMarker.atMs)))}
              y={PLOT_BOTTOM - 14}
              textAnchor="middle"
              className={styles.emptyText}
            >
              {emptyMarker.label}
            </text>
          </g>
        )}

        {hover && cursorMs !== null && (
          <g pointerEvents="none">
            <line
              x1={x(cursorMs)}
              x2={x(cursorMs)}
              y1={PLOT_TOP}
              y2={PLOT_BOTTOM}
              className={styles.cursorLine}
              style={{ stroke: accent }}
            />
            <circle cx={x(cursorMs)} cy={y(hover.value)} r={4.5} style={{ fill: accent }} />
            <rect
              x={tooltipLeft}
              y={4}
              width={tooltipWidth}
              height={28}
              rx={6}
              className={styles.tooltipBox}
            />
            <text
              x={tooltipLeft + tooltipWidth / 2}
              y={23}
              textAnchor="middle"
              className={styles.tooltipText}
            >
              {tooltip}
            </text>
          </g>
        )}
      </svg>
      <p className={styles.srOnly} aria-live="polite">
        {tooltip}
      </p>
    </div>
  );
}
