import type { ChartConfiguration } from 'chart.js';
import type { LadderEvent, LadderRun } from './api/ladder';
import { formatDate } from './utils';
import { LADDER_COUNTED_NOTE, formatLadderAmount, formatLadderUnits, ladderAmountUnits, ladderCumulative, ladderLabel, type LadderCumulative } from './ladder-timeline-filters';

export const LADDER_UNIT_LABEL = 'On-demand-equivalent $/hr';

const layerTokens: Record<string, string> = {
  'compute-sp': '--cudly-primary', 'ec2-instance-sp': '--cudly-success', 'convertible-ri': '--cudly-warn-fg',
  'azure-reservation': '--cudly-error', 'azure-savings-plan': '--cudly-info',
};

/** Resolve a design token; a missing token is a bug, not a reason to paint a default color. */
function token(name: string): string {
  const value = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  if (!value) throw new Error(`Missing CSS token ${name}`);
  return value;
}

export interface LadderChartSeries { label: string; layer?: string; values: bigint[]; kind: 'base' | 'layer' | 'target' }

export interface LadderChartModel {
  cumulative: LadderCumulative;
  series: LadderChartSeries[];
  /** True when the base is the selected run's existing commitment snapshot; otherwise 0. */
  baseFromRun: boolean;
  target: bigint | null;
}

/** Returns undefined when no tranche is counted, so there is nothing to draw. */
export function ladderChartModel(sorted: readonly LadderEvent[], run: LadderRun | undefined): LadderChartModel | undefined {
  const baseFromRun = run?.existing_usd_hr != null;
  const base = baseFromRun ? ladderAmountUnits(run.existing_usd_hr!) : 0n;
  const target = run?.target_usd_hr != null ? ladderAmountUnits(run.target_usd_hr) : null;
  const cumulative = ladderCumulative(sorted, base);
  if (cumulative.times.length === 0) return undefined;
  const series: LadderChartSeries[] = [];
  if (baseFromRun) series.push({ label: 'Existing commitment', values: cumulative.times.map(() => base), kind: 'base' });
  for (const item of cumulative.layers) series.push({ label: ladderLabel('layer_type', item.layer), layer: item.layer, values: item.units, kind: 'layer' });
  if (target !== null) series.push({ label: 'Target', values: cumulative.times.map(() => target), kind: 'target' });
  return { cumulative, series, baseFromRun, target };
}

const toNumber = (units: bigint): number => Number(formatLadderUnits(units));
const layerPointStyles = ['circle', 'rect', 'triangle', 'rectRot', 'crossRot'] as const;
const day = (stamp: number): string => formatDate(new Date(stamp).toISOString().slice(0, 10));

/** Text alternative for the canvas; the purchase table carries the same figures. */
export function ladderChartSummary(model: LadderChartModel): string {
  const { times, totals } = model.cumulative;
  const first = totals[0]!;
  const last = totals[totals.length - 1]!;
  const scope = model.baseFromRun ? 'including the existing commitment snapshot' : 'planned additions only';
  const target = model.target === null ? '' : ` Target ${formatLadderAmount(formatLadderUnits(model.target))}.`;
  return `Cumulative planned commitment, ${LADDER_UNIT_LABEL.toLowerCase()}, ${scope}: ${formatLadderAmount(formatLadderUnits(first))} on ${day(times[0]!)} rising to ${formatLadderAmount(formatLadderUnits(last))} on ${day(times[times.length - 1]!)}.${target} ${LADDER_COUNTED_NOTE} The purchase table below lists the same figures.`;
}

export function ladderChartConfig(model: LadderChartModel, selectedStamp: number | undefined, handlers: {
  legendClick: (layer: string) => void; pointClick: (series: LadderChartSeries, timeIndex: number) => void;
}): ChartConfiguration<'line'> {
  const { times } = model.cumulative;
  return {
    type: 'line',
    data: {
      datasets: model.series.map((item, index) => {
        const color = item.kind === 'base' ? token('--cudly-text-subtle') : item.kind === 'target' ? token('--cudly-text') : token(layerTokens[item.layer!] ?? '--cudly-text-muted');
        return {
          label: item.label, stepped: 'before', borderColor: color, backgroundColor: color, fill: item.kind !== 'target',
          stack: item.kind === 'target' ? 'target' : 'commitment', borderDash: item.kind === 'target' ? [6, 4] : [],
          pointStyle: item.kind === 'layer' ? layerPointStyles[index % layerPointStyles.length] : 'circle',
          pointRadius: times.map(stamp => item.kind === 'target' || item.kind === 'base' ? 0 : stamp === selectedStamp ? 8 : 5),
          data: times.map((stamp, i) => ({ x: stamp, y: toNumber(item.values[i]!) })),
        };
      }),
    },
    options: {
      responsive: true, maintainAspectRatio: false,
      scales: {
        x: { type: 'linear', title: { display: true, text: 'Scheduled date (UTC)' }, ticks: { callback: value => day(Number(value)) } },
        y: { stacked: true, beginAtZero: true, title: { display: true, text: `${LADDER_UNIT_LABEL}, cumulative` } },
      },
      plugins: {
        legend: {
          title: { display: true, text: LADDER_UNIT_LABEL },
          labels: { usePointStyle: true },
          onClick: (_event, legend) => { const layer = model.series[legend.datasetIndex ?? -1]?.layer; if (layer) handlers.legendClick(layer); },
        },
        tooltip: { callbacks: { label: context => {
          const item = model.series[context.datasetIndex];
          return item ? `${item.label}: ${formatLadderAmount(formatLadderUnits(item.values[context.dataIndex]!))} cumulative, ${LADDER_UNIT_LABEL.toLowerCase()}` : '';
        } } },
      },
      onClick: (_event, points) => {
        const point = points[0];
        const item = point && model.series[point.datasetIndex];
        if (item) handlers.pointClick(item, point.index);
      },
    },
  };
}
