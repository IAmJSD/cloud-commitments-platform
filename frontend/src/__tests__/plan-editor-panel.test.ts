import { mountPlanEditorPanel } from '../plan-editor-panel';

let host: HTMLElement, form: HTMLFormElement, dispose: () => void;
const field = (key: string): HTMLInputElement => host.querySelector(`[data-editor="${key}"]`)!;
const change = (key: string, value: string): void => {
  const input = field(key); input.focus(); input.value = value;
  input.dispatchEvent(new Event('input', { bubbles: true })); input.dispatchEvent(new Event('change', { bubbles: true }));
};
beforeEach(() => {
  jest.useFakeTimers({ now: new Date('2026-10-15T23:59:00Z') });
  document.body.innerHTML = `<div class="modal"><div class="plan-editor-layout"><section hidden></section><form>
    <input id="plan-name" value="Compute"><input name="ramp-schedule" type="radio" value="custom" checked>
    <input id="plan-coverage" value="80"><input id="ramp-step-percent" value="33"><input id="ramp-interval-days" value="7">
    <input id="plan-notify-days" value="3"><input id="plan-term" value="1"><input id="plan-payment" value="monthly"></form></div></div>`;
  form = document.querySelector('form')!; host = document.querySelector('section')!; dispose = mountPlanEditorPanel(host, form, { editableDates: true });
});
afterEach(() => { dispose(); jest.useRealTimers(); });

test('renders real Create intent and follows unedited notification settings', () => {
  expect(host.hidden).toBe(false); expect(host.textContent).toContain('Save Plan does not save date changes');
  expect(host.querySelectorAll('[data-preview-step]')).toHaveLength(3);
  expect(host.querySelector('[data-preview-step="2"]')?.getAttribute('aria-label')).toContain('target 79.2%');
  expect(host.querySelectorAll('svg [role=button]')).toHaveLength(5);
  const notice = form.querySelector<HTMLInputElement>('#plan-notify-days')!; notice.value = '10'; notice.dispatchEvent(new Event('change', { bubbles: true }));
  expect(field('date').value).toBe('2026-10-27');
});
test('preserves focused fields and rejects collisions before paired range correction', () => {
  const input = field('date'), point = host.querySelector('[data-preview-step]');
  change('date', '2026-10-21'); expect(host.querySelector('[data-preview-step]')).toBe(point);
  expect(field('date')).toBe(input); expect(document.activeElement).toBe(input);
  change('start', '2026-11-10'); expect(field('start').value).toBe('2026-11-10');
  expect(field('step').disabled).toBe(true); expect(field('error').textContent).not.toBe('');
  change('end', '2026-11-20'); expect(field('step').disabled).toBe(false);
  expect(host.querySelector('[data-preview-step]')?.getAttribute('aria-label')).toContain('2026-11-10');
});
test('Enter commits only local dates and reset restores the baseline', () => {
  const submit = jest.fn(); form.addEventListener('submit', submit);
  field('date').value = '2026-10-21'; field('date').dispatchEvent(new Event('input', { bubbles: true }));
  const enter = new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true }); field('date').dispatchEvent(enter);
  expect(enter.defaultPrevented).toBe(true); expect(submit).not.toHaveBeenCalled();
  expect(host.querySelector('[data-editor="changes"] summary')?.textContent).toContain('(1)'); field('reset').click();
  expect(host.querySelector('.editor-help summary')?.textContent).toBe('Keyboard and touch help');
  expect(field('date').value).toBe('2026-10-20'); expect(host.querySelector('[data-editor="changes"] summary')?.textContent).toContain('(0)');
});
test.each([['ramp-step-percent', '25', 4], ['plan-notify-days', '10', 3]])('requires explicit rebuild after edited %s changes', (id, value, count) => {
  change('date', '2026-10-21'); const percent = form.querySelector<HTMLInputElement>(`#${id}`)!;
  percent.value = String(value); percent.dispatchEvent(new Event('change', { bubbles: true }));
  expect(field('rebuild').hidden).toBe(false); field('reset').click(); expect(field('error').textContent).toContain('Rebuild');
  field('rebuild').click(); expect(host.querySelectorAll('[data-preview-step]')).toHaveLength(count);
  if (id === 'plan-notify-days') expect(field('date').value).toBe('2026-10-27');
  expect(field('rebuild').hidden).toBe(true); expect(form.querySelector<HTMLInputElement>('#plan-name')?.value).toBe('Compute');
});
test('unmount removes Create state and listeners before another modal context', () => {
  dispose(); expect(host.hidden).toBe(true); expect(host.childElementCount).toBe(0);
  expect(host.closest('.plan-editor-layout')?.classList.contains('has-editor-draft')).toBe(false);
  form.querySelector('#plan-coverage')!.dispatchEvent(new Event('change', { bubbles: true })); expect(host.childElementCount).toBe(0);
});

test.each(['plan-coverage', 'ramp-step-percent', 'plan-notify-days'])('preserves invalid %s reason across local edits and reset', id => {
  const setting = form.querySelector<HTMLInputElement>(`#${id}`)!;
  const original = setting.value; setting.value = ''; setting.dispatchEvent(new Event('input', { bubbles: true }));
  const reason = field('error').textContent; expect(reason).not.toBe('');
  change('date', '2026-10-21'); expect(field('error').textContent).toBe(reason);
  field('reset').click(); expect(field('error').textContent).toBe(reason);
  change('date', '2026-10-21'); setting.value = original; setting.dispatchEvent(new Event('change', { bubbles: true }));
  expect(field('date').value).toBe('2026-10-21'); expect(field('error').textContent).not.toBe(reason);
});


test('actual Create pointer ownership guards submission, cancels on geometry change and restores model', () => {
  const svg = host.querySelector<SVGSVGElement>('svg')!, point = host.querySelector<SVGGElement>('[data-preview-step="1"]')!;
  let captured = false, left = 0;
  Object.assign(svg, { getScreenCTM: () => ({ e: left, inverse: () => ({}) }),
    createSVGPoint: () => ({ x: 0, y: 0, matrixTransform() { return { x: this.x }; } }),
    setPointerCapture: () => { captured = true; }, hasPointerCapture: () => captured, releasePointerCapture: () => { captured = false; } });
  const send = (type: string, target: Element, x: number): void => { target.dispatchEvent(Object.assign(new Event(type, { bubbles: true, cancelable: true }),
    { pointerId: 1, isPrimary: true, button: 0, clientX: x, clientY: 0 })); };
  const x = Number(point.querySelector('circle')!.getAttribute('cx')), day = 544 / 22;
  send('pointerdown', point, x); send('pointermove', svg, x + day);
  expect(field('date').value).toBe('2026-10-28'); expect(form.inert).toBe(true); expect(document.activeElement).toBe(point);
  const submit = new Event('submit', { bubbles: true, cancelable: true }); form.dispatchEvent(submit); expect(submit.defaultPrevented).toBe(true);
  window.dispatchEvent(new Event('scroll')); expect(form.inert).toBe(true);
  left = 1; window.dispatchEvent(new Event('resize'));
  expect(field('date').value).toBe('2026-10-27'); expect(form.inert).toBeFalsy(); expect(captured).toBe(false);
  expect(field('error').textContent).toContain('Graph moved');
  point.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', altKey: true, bubbles: true }));
  expect(field('date').value).toBe('2026-10-28'); expect(form.querySelector<HTMLInputElement>('#plan-name')?.value).toBe('Compute');
  send('pointerdown', point, x); dispose(); expect(form.inert).toBeFalsy(); expect(captured).toBe(false);
});

describe('display-only default (Create dates are not saved yet)', () => {
  beforeEach(() => { dispose(); dispose = mountPlanEditorPanel(host, form); });
  test('shows the graph and dates but disables every date control with a reason', () => {
    expect(host.querySelectorAll('[data-preview-step]')).toHaveLength(3);
    expect(host.querySelectorAll('svg [role=button]')).toHaveLength(0);
    expect(host.querySelector('.preview-hit, [data-preview-boundary]')).toBeNull();
    for (const key of ['step', 'date', 'start', 'end', 'reset']) expect(field(key).disabled).toBe(true);
    expect(field('date').value).toBe('2026-10-20');
    expect(host.textContent).toContain('Custom step dates can be set once saving them is supported');
    expect(host.textContent).not.toContain('does not save date changes');
  });
  test('dragging a point is inert and typed input changes nothing', () => {
    const point = host.querySelector('[data-preview-step="0"]')!, label = point.getAttribute('aria-label');
    const svg = host.querySelector('svg')!;
    point.dispatchEvent(new MouseEvent('pointerdown', { bubbles: true, clientX: 10, clientY: 10 }));
    svg.dispatchEvent(new MouseEvent('pointermove', { bubbles: true, clientX: 200, clientY: 10 }));
    svg.dispatchEvent(new MouseEvent('pointerup', { bubbles: true, clientX: 200, clientY: 10 }));
    expect(host.querySelector('[data-preview-step="0"]')!.getAttribute('aria-label')).toBe(label);
    expect(form.inert).toBeFalsy();
    change('date', '2026-10-30');
    expect(host.querySelector('[data-preview-step="0"]')!.getAttribute('aria-label')).toBe(label);
    expect(host.querySelector('[data-editor="changes"] summary')!.textContent).toBe('Local date changes (0)');
  });
});
