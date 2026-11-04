import { escapeHtml } from './utils';
import { draftDateDay } from './lib/plan-draft-dates';

export interface SchedulePlotPoint { step: number; date: string; target: number }
export interface SchedulePlotFrame { width: number; startDay: number; span: number }
export function schedulePlotFrame(dates: readonly string[]): SchedulePlotFrame {
  if (dates.length === 0) throw new Error('No dated steps to plot.');
  const days = dates.map(draftDateDay), first = Math.min(...days), last = Math.max(...days);
  return { width: Math.max(640, dates.length * 48 + 96), startDay: first - 3, span: last - first + 8 };
}
export function schedulePlotX(date: string, frame: SchedulePlotFrame): number {
  return 48 + (draftDateDay(date) - frame.startDay) / frame.span * (frame.width - 96);
}
const y = (target: number): number => 260 - target * 2.2;
function staircase(points: readonly SchedulePlotPoint[], frame: SchedulePlotFrame): string {
  let path = points[0]?.step === 1 ? `M 48 ${y(0)}` : '';
  points.forEach((point, index) => {
    const left = schedulePlotX(point.date, frame);
    path += point.step === (points[index - 1]?.step ?? 0) + 1
      ? ` H ${left} V ${y(point.target)}` : ` M ${left} ${y(point.target)}`;
  });
  return path;
}

export function renderSchedulePlot(points: readonly SchedulePlotPoint[], frame: SchedulePlotFrame, selected: number, interactive: boolean): string {
  const { width } = frame, count = points.length, x = (index: number): number => schedulePlotX(points[index]!.date, frame);
  const path = staircase(points, frame);
  // The read-only plot has no Start/End handle row, so it ends below the date labels.
  const height = interactive ? 370 : 310;
  return `<svg viewBox="0 0 ${width} ${height}" width="${width}" role="${interactive ? "group" : "img"}" aria-label="Hypothetical purchase dates">
                ${[0, 20, 40, 60, 80, 100].map(value => `<path class="preview-grid" d="M 48 ${y(value)} H ${width - 24}"/><text x="40" y="${y(value) + 4}" text-anchor="end">${value}%</text>`).join('')}
                <path class="preview-staircase" d="${path}"/>
                ${points.map((point, index) => `<g ${interactive ? `role="button" tabindex="${point.step - 1 === selected ? 0 : -1}"` : ""} data-preview-step="${point.step - 1}" ${point.step - 1 === selected ? 'data-selected' : ''} ${interactive ? `aria-pressed="${point.step - 1 === selected}"` : ""} aria-label="Step ${point.step}, ${escapeHtml(point.date)}, target ${point.target}%">
${interactive ? `                  <foreignObject class="preview-hit" x="${x(index) - 22}" y="${y(point.target) - 22}" width="44" height="44"><div xmlns="http://www.w3.org/1999/xhtml" class="preview-touch-target"></div></foreignObject>` : ""}
                  <circle class="preview-marker" cx="${x(index)}" cy="${y(point.target)}" r="6"/>
                  ${index % Math.ceil(count / 5) === 0 || index === count - 1 ? `<text x="${x(index)}" y="292" text-anchor="middle">${escapeHtml(point.date.slice(5))}</text>` : ''}
                </g>`).join('')}
                ${interactive && count > 1 ? `<path class="preview-interval" d="M ${x(0)} 336 H ${x(count - 1)}"/>
                  ${(['start', 'end'] as const).map((boundary, i) => {
                    const left = x(i === 0 ? 0 : count - 1);
                    return `<g data-preview-boundary="${boundary}" role="button" tabindex="0">
                      <foreignObject class="preview-handle-hit" x="${left - 22}" y="314" width="44" height="44"><div xmlns="http://www.w3.org/1999/xhtml" class="preview-touch-target"></div></foreignObject>
                      <rect class="preview-handle-marker" x="${left - 6}" y="330" width="12" height="12"/>
                      <text x="${left}" y="365" text-anchor="middle">${i === 0 ? 'Start' : 'End'}</text></g>`;
                  }).join('')}` : ''}
              </svg>`;
}

export function updateSchedulePlot(svg: SVGSVGElement, points: readonly SchedulePlotPoint[], frame: SchedulePlotFrame, selected: number, interactive: boolean): void {
  const buttons = svg.querySelectorAll<SVGGElement>('[data-preview-step]');
  buttons.forEach((button, index) => {
    const point = points[index]!, left = schedulePlotX(point.date, frame);
    button.querySelector('.preview-hit')?.setAttribute('x', String(left - 22));
    button.querySelector('.preview-hit')?.setAttribute('y', String(y(point.target) - 22));
    button.querySelector('.preview-marker')!.setAttribute('cx', String(left));
    button.querySelector('.preview-marker')!.setAttribute('cy', String(y(point.target)));
    const tick = button.querySelector('text');
    if (tick) { tick.setAttribute('x', String(left)); tick.textContent = point.date.slice(5); }
    button.dataset.previewStep = String(point.step - 1);
    button.setAttribute('aria-label', `Step ${point.step}, ${point.date}, target ${point.target}%`);
    button.toggleAttribute('data-selected', point.step - 1 === selected);
    if (interactive) { button.setAttribute('aria-pressed', String(point.step - 1 === selected)); button.setAttribute('tabindex', point.step - 1 === selected ? '0' : '-1'); }
  });
  svg.querySelector('.preview-staircase')!.setAttribute('d', staircase(points, frame));
  svg.querySelectorAll<SVGGElement>('[data-preview-boundary]').forEach(handle => {
    const boundary = handle.dataset.previewBoundary!, index = boundary === 'start' ? 0 : points.length - 1;
    const left = schedulePlotX(points[index]!.date, frame);
    handle.querySelector('.preview-handle-hit')!.setAttribute('x', String(left - 22));
    handle.querySelector('.preview-handle-marker')!.setAttribute('x', String(left - 6));
    handle.querySelector('text')!.setAttribute('x', String(left));
    handle.setAttribute('aria-label', `Interval ${boundary}, ${points[index]!.date}, UTC. Arrows move a day; Enter edits date.`);
  });
  svg.querySelector('.preview-interval')?.setAttribute('d', `M ${schedulePlotX(points[0]!.date, frame)} 336 H ${schedulePlotX(points[points.length - 1]!.date, frame)}`);
}
