import { closeModal, openModal } from './modal';
import { escapeHtml } from './utils';
import { suggestedPurchaseStartDate } from './lib/calendar-date';

export interface PlanSchedulePreviewInput {
  name: string;
  preset: string;
  coverage: string;
  stepPercent: string;
  intervalDays: string;
  notificationDays: string;
  term: string;
  payment: string;
}

function integer(value: string, label: string, min: number, max: number): number {
  const parsed = value.trim() === '' ? NaN : Number(value);
  if (!Number.isInteger(parsed) || parsed < min || parsed > max) throw new Error(`${label} must be a whole number between ${min} and ${max}`);
  return parsed;
}

export function openPlanSchedulePreview(input: PlanSchedulePreviewInput, parent: HTMLElement): void {
  const coverage = integer(input.coverage, 'Target coverage', 0, 100);
  const notice = integer(input.notificationDays, 'Notification days', 1, 30);
  // Mirrors presets/shares in internal/config/types.go and custom
  // step counting in PlanRequest.buildCustomRampSchedule, internal/api/types.go.
  const presets: Record<string, readonly [number, number]> = { immediate: [100, 0], 'weekly-25pct': [25, 7], 'monthly-10pct': [10, 30] };
  const preset = Object.prototype.hasOwnProperty.call(presets, input.preset) ? presets[input.preset] : undefined;
  if (!preset && input.preset !== 'custom') throw new Error('Choose a valid ramp schedule before previewing');
  const percent = preset?.[0] ?? integer(input.stepPercent, 'Step percent', 1, 100);
  const interval = preset?.[1] ?? integer(input.intervalDays, 'Interval days', 1, 365);
  const count = Math.floor(100 / percent);
  const start = Date.parse(`${suggestedPurchaseStartDate(notice)}T00:00:00Z`);
  const points = Array.from({ length: count }, (_, index) => ({
    date: new Date(start + index * interval * 86_400_000).toISOString().slice(0, 10),
    target: coverage * Math.min(100, (index + 1) * percent) / 100,
  }));
  const previous = document.getElementById('plan-schedule-preview');
  if (previous) { closeModal(previous); previous.remove(); }
  const modal = document.createElement('div');
  modal.id = 'plan-schedule-preview';
  modal.className = 'modal hidden';
  modal.setAttribute('role', 'dialog');
  modal.setAttribute('aria-modal', 'true');
  modal.setAttribute('aria-labelledby', 'schedule-preview-title');
  const width = Math.max(640, count * 48 + 96);
  const x = (index: number): number => count === 1 ? width / 2 : 48 + index * (width - 96) / (count - 1);
  const y = (target: number): number => 260 - target * 2.2;
  let path = `M 48 ${y(0)}`;
  points.forEach((point, index) => { path += ` H ${x(index)} V ${y(point.target)}`; });
  modal.innerHTML = `
    <div class="modal-content schedule-preview-panel">
      <header><h2 id="schedule-preview-title">Schedule preview</h2><p>${escapeHtml(input.name)}</p></header>
      <div class="schedule-preview-body">
        <p class="schedule-preview-notice">Draft preview. These dates are not saved or scheduled. Saved purchases and history are not shown.</p>
        <div class="schedule-preview-layout">
          <section aria-label="Draft schedule">
            <h3>Planned cumulative target (%)</h3>
            <p class="help-text">Hypothetical dates, not observed utilization. All dates use UTC.</p>
            <div class="schedule-preview-scroll" tabindex="0" aria-label="Scrollable draft calendar">
              <svg viewBox="0 0 ${width} 310" width="${width}" role="group" aria-label="Hypothetical purchase dates">
                ${[0, 20, 40, 60, 80, 100].map(value => `<path class="preview-grid" d="M 48 ${y(value)} H ${width - 24}"/><text x="8" y="${y(value) + 4}">${value}</text>`).join('')}
                <path class="preview-staircase" d="${path}"/>
                ${points.map((point, index) => `<g role="button" tabindex="${index === 0 ? 0 : -1}" data-preview-step="${index}" aria-pressed="${index === 0}" aria-label="Step ${index + 1}, ${point.date}, target ${point.target}%">
                  <circle class="preview-hit" cx="${x(index)}" cy="${y(point.target)}" r="22"/>
                  <circle class="preview-marker" cx="${x(index)}" cy="${y(point.target)}" r="6"/>
                  ${index % Math.ceil(count / 5) === 0 || index === count - 1 ? `<text x="${x(index)}" y="292" text-anchor="middle">${point.date.slice(5)}</text>` : ''}
                </g>`).join('')}
              </svg>
            </div>
            <p class="help-text">○ Draft date · Select a point to inspect it. Arrow keys move between steps.</p>
          </section>
          <aside aria-label="Read-only plan summary">
            <h3>Plan summary</h3>
            <dl><dt>Target</dt><dd>${coverage}%</dd><dt>Steps</dt><dd>${count}</dd><dt>Start</dt><dd>${points[0]!.date}</dd><dt>End</dt><dd>${points[count - 1]!.date}</dd></dl>
            <h3 id="preview-selected-step">Step 1</h3>
            <label>Preview date (UTC)<input id="preview-selected-date" type="date" readonly value="${points[0]!.date}"></label>
            <p id="preview-selected-target" role="status"></p>
            <p class="help-text">Amount unavailable in this draft preview.</p>
            <details><summary>Advanced settings</summary><dl><dt>Term</dt><dd>${escapeHtml(input.term)} years</dd><dt>Payment</dt><dd>${escapeHtml(input.payment)}</dd><dt>Notification window</dt><dd>${notice} days</dd></dl></details>
          </aside>
        </div>
      </div>
      <footer><button type="button" id="close-schedule-preview">Close preview</button></footer>
    </div>`;
  document.body.appendChild(modal);
  const buttons = Array.from(modal.querySelectorAll<SVGGElement>('[data-preview-step]'));
  const select = (index: number, focus: boolean): void => {
    const point = points[index]!;
    buttons.forEach((button, i) => { button.setAttribute('aria-pressed', String(i === index)); button.setAttribute('tabindex', i === index ? '0' : '-1'); });
    modal.querySelector('#preview-selected-step')!.textContent = `Step ${index + 1}`;
    (modal.querySelector('#preview-selected-date') as HTMLInputElement).value = point.date;
    modal.querySelector('#preview-selected-target')!.textContent = `Planned target: ${point.target}%`;
    if (focus) buttons[index]!.focus();
  };
  buttons.forEach((button, index) => {
    button.addEventListener('click', () => select(index, true));
    button.addEventListener('keydown', event => {
      if (['Enter', ' ', 'ArrowLeft', 'ArrowRight'].includes(event.key)) {
        event.preventDefault();
        const next = event.key === 'ArrowLeft' ? Math.max(0, index - 1) : event.key === 'ArrowRight' ? Math.min(count - 1, index + 1) : index;
        select(next, true);
      }
    });
  });
  modal.querySelector('#close-schedule-preview')!.addEventListener('click', () => closeModal(modal));
  const wasInert = parent.inert;
  parent.inert = true;
  select(0, false);
  openModal(modal, { onClose: () => { parent.inert = wasInert; modal.remove(); } });
}
