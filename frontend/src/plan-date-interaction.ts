import { draftDateDay, validateDraftDates } from './lib/plan-draft-dates';
import { schedulePlotFrame, schedulePlotX, SchedulePlotFrame } from './plan-schedule-plot';

type Boundary = 'start' | 'end';
export interface PlanDateIntent { index: number; boundary: Boundary | null; date: string; before: readonly string[] }
export interface PlanDateInteraction {
  read(): { dates: readonly string[]; blocked: boolean };
  select(index: number): void;
  gesture(frame: SchedulePlotFrame | null): void;
  apply(intent: PlanDateIntent): void;
  restore(before: readonly string[]): void;
  focusDate(boundary: Boundary): void;
  error(message: string): void;
}

export function wirePlanDateInteraction(svg: SVGSVGElement, binding: PlanDateInteraction): () => void {
  const modal = svg.closest<HTMLElement>('.modal')!;
  let gesture: { id: number; index: number; boundary: Boundary | null; before: string[]; startX: number;
    anchorX: number; day: number; frame: SchedulePlotFrame; matrix: DOMMatrix; inverse: DOMMatrix } | null = null;
  const dateAt = (day: number): string => new Date(day * 86_400_000).toISOString().slice(0, 10);
  const owner = (event: Event): SVGGElement | null => event.target instanceof Element
    ? event.target.closest<SVGGElement>('[data-preview-step], [data-preview-boundary]') : null;
  const select = (index: number, focus: boolean): void => {
    binding.select(index);
    if (focus) svg.querySelector<SVGGElement>(`[data-preview-step="${index}"]`)!.focus();
  };
  const stop = (cancel: boolean): void => {
    const current = gesture;
    if (!current) return;
    gesture = null;
    try { if (cancel) binding.restore(current.before); }
    finally {
      try { binding.gesture(null); }
      finally { if (svg.hasPointerCapture(current.id)) svg.releasePointerCapture(current.id); }
    }
    if (cancel) {
      binding.error('');
      try { validateDraftDates(binding.read().dates); }
      catch { binding.error('Draft dates are no longer future dates. Close and reopen for fresh dates.'); }
    }
  };
  const attempt = (operation: () => void, prefix = ''): void => {
    try { operation(); }
    catch (reason) {
      try { stop(true); }
      finally { binding.error(prefix + (reason instanceof Error ? reason.message : String(reason))); }
    }
  };
  const pointerX = (event: PointerEvent, inverse: DOMMatrix): number => {
    const point = svg.createSVGPoint(); point.x = event.clientX; point.y = event.clientY;
    const left = point.matrixTransform(inverse).x;
    if (!Number.isFinite(left)) throw new Error('Drag coordinates unavailable. Use date inputs.');
    return left;
  };
  const move = (event: PointerEvent): void => {
    const current = gesture;
    if (!current || current.id !== event.pointerId) return;
    const displacement = pointerX(event, current.inverse) - current.startX, left = current.anchorX + displacement;
    if (left < 48 || left > current.frame.width - 48) { binding.error('Stay inside the plot; use date inputs for farther dates.'); return; }
    binding.apply({ index: current.index, boundary: current.boundary, before: current.before,
      date: dateAt(current.day + Math.round(displacement / (current.frame.width - 96) * current.frame.span)) });
  };
  const down = (event: PointerEvent): void => {
    if (gesture || binding.read().blocked || !event.isPrimary || event.button !== 0) return;
    const target = owner(event);
    if (!target) return;
    attempt(() => {
      const { dates } = binding.read(), boundary = target.dataset.previewBoundary as Boundary | undefined;
      const index = boundary === 'end' ? dates.length - 1 : boundary === 'start' ? 0 : Number(target.dataset.previewStep);
      if (!Number.isInteger(index) || index < 0 || index >= dates.length) return;
      if (!boundary) select(index, false);
      target.focus({ preventScroll: true });
      const matrix = svg.getScreenCTM();
      if (!matrix) throw new Error('Drag coordinates unavailable. Use date inputs.');
      const inverse = matrix.inverse(), frame = schedulePlotFrame(dates);
      gesture = { id: event.pointerId, index, boundary: boundary ?? null, before: [...dates], frame, matrix, inverse,
        startX: pointerX(event, inverse), anchorX: schedulePlotX(dates[index]!, frame), day: draftDateDay(dates[index]!) };
      svg.setPointerCapture(event.pointerId); binding.gesture(frame); event.preventDefault();
    }, 'Cannot start date drag. Use date inputs. ');
  };
  const pointerMove = (event: PointerEvent): void => attempt(() => move(event));
  const up = (event: PointerEvent): void => {
    if (gesture?.id !== event.pointerId) return;
    attempt(() => {
      move(event);
      try { validateDraftDates(binding.read().dates); }
      catch { throw new Error('Draft dates are no longer future dates. Close and reopen for fresh dates.'); }
      stop(false);
    });
  };
  const cancel = (event: Event): void => { if (gesture?.id === (event as PointerEvent).pointerId) attempt(() => stop(true)); };
  const click = (event: MouseEvent): void => {
    const target = owner(event);
    if (!gesture && !binding.read().blocked && target?.dataset.previewStep !== undefined) attempt(() => select(Number(target.dataset.previewStep), true));
  };
  const keydown = (event: KeyboardEvent): void => {
    const target = owner(event);
    if (!target || event.ctrlKey || event.metaKey || !['Enter', ' ', 'ArrowLeft', 'ArrowRight'].includes(event.key)) return;
    event.preventDefault();
    if (gesture || binding.read().blocked) return;
    attempt(() => {
      const { dates } = binding.read(), boundary = target.dataset.previewBoundary as Boundary | undefined;
      const index = boundary === 'end' ? dates.length - 1 : boundary === 'start' ? 0 : Number(target.dataset.previewStep);
      if (boundary && ['Enter', ' '].includes(event.key)) { binding.focusDate(boundary); return; }
      if (boundary || event.altKey && ['ArrowLeft', 'ArrowRight'].includes(event.key)) {
        binding.apply({ index, boundary: boundary ?? null, before: [...dates],
          date: dateAt(draftDateDay(dates[index]!) + (event.key === 'ArrowRight' ? 1 : -1)) });
      } else select(event.key === 'ArrowLeft' ? Math.max(0, index - 1)
        : event.key === 'ArrowRight' ? Math.min(dates.length - 1, index + 1) : index, true);
    });
  };
  const modalKey = (event: KeyboardEvent): void => {
    if (!gesture || event.ctrlKey || event.metaKey) return;
    if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); attempt(() => stop(true)); }
    else if (['Tab', 'Enter', ' ', 'ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown', 'PageUp', 'PageDown', 'Home', 'End'].includes(event.key)) {
      event.preventDefault(); event.stopPropagation();
    }
  };
  const geometry = (): void => {
    if (!gesture) return;
    const matrix = svg.getScreenCTM(), previous = gesture.matrix;
    if (!matrix || (['a', 'b', 'c', 'd', 'e', 'f'] as const).some(key => matrix[key] !== previous[key])) {
      attempt(() => { stop(true); binding.error('Graph moved. Date drag canceled; use date inputs or start again.'); });
    }
  };
  svg.addEventListener('pointerdown', down); svg.addEventListener('pointermove', pointerMove); svg.addEventListener('pointerup', up);
  svg.addEventListener('pointercancel', cancel); svg.addEventListener('lostpointercapture', cancel);
  svg.addEventListener('click', click); svg.addEventListener('keydown', keydown); modal.addEventListener('keydown', modalKey, true);
  window.addEventListener('scroll', geometry, true); window.addEventListener('resize', geometry);
  return () => {
    try { stop(true); }
    finally {
      svg.removeEventListener('pointerdown', down); svg.removeEventListener('pointermove', pointerMove); svg.removeEventListener('pointerup', up);
      svg.removeEventListener('pointercancel', cancel); svg.removeEventListener('lostpointercapture', cancel);
      svg.removeEventListener('click', click); svg.removeEventListener('keydown', keydown); modal.removeEventListener('keydown', modalKey, true);
      window.removeEventListener('scroll', geometry, true); window.removeEventListener('resize', geometry);
    }
  };
}
