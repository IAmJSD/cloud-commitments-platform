import { draftDateDay, redistributeDraftDates } from './plan-draft-dates';

export interface EditorSlot {
  step_number: number;
  scheduled_date: string | null;
  cumulative_target_percent: number;
  execution_id: string | null;
  logical_completed: boolean;
  root_state: 'missing' | 'unique' | 'ambiguous';
  capability: 'fixed' | 'replace_pending' | 'approved_toggle_required' | 'needs_creation' | 'denied';
  editable: boolean;
  requires_approved_edits: boolean;
}
export type DraftContext =
  | { kind: 'create'; totalSteps: number; shares: readonly number[]; dates: readonly (string | null)[] }
  | { kind: 'edit'; planId: string; revision: string; totalSteps: number; slots: readonly EditorSlot[] };
export interface EditorDraft {
  readonly context: DraftContext;
  readonly dates: readonly (string | null)[];
  readonly selected: number;
  readonly approvedEdits: boolean;
}
export interface DraftDateChange {
  step: number;
  before: string | null;
  after: string | null;
  executionId: string | null;
  requiresReapproval: boolean;
}
const capabilities = ['fixed', 'replace_pending', 'approved_toggle_required', 'needs_creation', 'denied'];

export function createEditorDraft(input: DraftContext): EditorDraft {
  const count = input.totalSteps;
  if (!Number.isInteger(count) || count < 1 || count > 100) throw new Error('Unsupported step count.');
  let context: DraftContext;
  if (input.kind === 'create') {
    if (input.shares.length !== count || input.dates.length !== count) throw new Error('Incomplete local schedule.');
    context = Object.freeze({ kind: 'create', totalSteps: input.totalSteps, shares: Object.freeze([...input.shares]), dates: Object.freeze([...input.dates]) });
  } else {
    if (input.slots.length !== count) throw new Error('Incomplete schedule projection.');
    if (!input.planId || !input.revision) throw new Error('Missing plan revision.');
    if (input.slots.some((slot, i) => slot.step_number !== i + 1 || !capabilities.includes(slot.capability)
      || !['missing', 'unique', 'ambiguous'].includes(slot.root_state))) throw new Error('Unsupported schedule projection.');
    context = Object.freeze({ kind: 'edit', planId: input.planId, revision: input.revision, totalSteps: input.totalSteps, slots: Object.freeze(input.slots.map(slot => Object.freeze({ ...slot }))) });
  }
  const shares = context.kind === 'create' ? context.shares : context.slots.map(slot => slot.cumulative_target_percent);
  if (shares.some((share, i) => !Number.isFinite(share) || share < 0 || share > 100
    || i > 0 && share < shares[i - 1]!)) throw new Error('Unsupported ramp shares.');
  const dates = context.kind === 'create' ? [...context.dates] : context.slots.map(slot => slot.scheduled_date);
  dates.forEach(date => { if (date !== null) draftDateDay(date); });
  return Object.freeze({ context, dates: Object.freeze(dates), selected: 0, approvedEdits: false });
}

export function canDraftStep(state: EditorDraft, index: number): boolean {
  if (!Number.isInteger(index) || index < 0 || index >= state.dates.length) return false;
  if (state.context.kind === 'create') return true;
  const slot = state.context.slots[index]!;
  if (slot.logical_completed || slot.root_state === 'ambiguous') return false;
  if (slot.capability === 'needs_creation') return slot.root_state === 'missing' && slot.execution_id === null
    && slot.scheduled_date === null && !slot.editable && !slot.requires_approved_edits;
  if (slot.root_state !== 'unique' || !slot.execution_id || slot.scheduled_date === null) return false;
  return slot.capability === 'replace_pending' && slot.editable && !slot.requires_approved_edits
    || slot.capability === 'approved_toggle_required' && !slot.editable && slot.requires_approved_edits && state.approvedEdits;
}

export function selectDraftStep(state: EditorDraft, index: number): EditorDraft {
  if (!Number.isInteger(index) || index < 0 || index >= state.dates.length) throw new Error('Choose an existing step.');
  return Object.freeze({ ...state, selected: index });
}
export function setApprovedDraftEdits(state: EditorDraft, enabled: boolean): EditorDraft {
  return Object.freeze({ ...state, approvedEdits: state.context.kind === 'edit' && enabled });
}
export function draftBlock(state: EditorDraft): readonly [number, number] | null {
  if (!canDraftStep(state, state.selected)) return null;
  let first = state.selected, last = first;
  while (canDraftStep(state, first - 1)) first--;
  while (canDraftStep(state, last + 1)) last++;
  return [first, last];
}
function baselineDates(state: EditorDraft): readonly (string | null)[] {
  return state.context.kind === 'create' ? state.context.dates : state.context.slots.map(slot => slot.scheduled_date);
}
function validateProposal(state: EditorDraft, dates: readonly (string | null)[], changed: readonly number[]): void {
  const today = Math.floor(Date.now() / 86_400_000);
  for (const index of changed) {
    if (!canDraftStep(state, index)) throw new Error('This step is locked.');
    const date = dates[index];
    if (!date || draftDateDay(date) <= today) throw new Error('Choose a future UTC date.');
    const block = draftBlock({ ...state, selected: index })!;
    for (const [other, neighbor] of dates.entries()) {
      if (other === index) continue;
      if (neighbor === null) {
        if (other === block[0] - 1 || other === block[1] + 1) throw new Error('A fixed date is unavailable.');
      } else if ((draftDateDay(neighbor) - draftDateDay(date)) * Math.sign(other - index) < Math.abs(other - index)) {
        throw new Error('Keep dates in step order, at least one UTC day apart.');
      }
    }
  }
}
export function setDraftDate(state: EditorDraft, index: number, date: string): EditorDraft {
  if (!canDraftStep(state, index)) throw new Error('This step is locked.');
  const dates = [...state.dates];
  dates[index] = date;
  validateProposal(state, dates, [index]);
  return Object.freeze({ ...state, dates: Object.freeze(dates) });
}
export function redistributeEditorBlock(state: EditorDraft, start: string, end: string): EditorDraft {
  const block = draftBlock(state);
  if (!block) throw new Error('Choose an editable block.');
  const [first, last] = block;
  const dates = state.dates.slice(first, last + 1);
  if (dates.some(date => date === null)) throw new Error('Enter every block date before redistributing.');
  const replacement = redistributeDraftDates(dates as string[], start, end);
  const next = [...state.dates];
  next.splice(first, replacement.length, ...replacement);
  validateProposal(state, next, replacement.map((_, i) => first + i));
  return Object.freeze({ ...state, dates: Object.freeze(next) });
}
export function resetEditorDraft(state: EditorDraft): EditorDraft {
  return Object.freeze({ ...state, dates: Object.freeze([...baselineDates(state)]), approvedEdits: false });
}
export function draftDateChanges(state: EditorDraft): DraftDateChange[] {
  const before = baselineDates(state);
  return state.dates.flatMap((after, i) => after === before[i] ? [] : [{
    step: i + 1, before: before[i]!, after,
    executionId: state.context.kind === 'edit' ? state.context.slots[i]!.execution_id : null,
    requiresReapproval: state.context.kind === 'edit' && state.context.slots[i]!.requires_approved_edits,
  }]);
}
export function canReviewEditorDraft(state: EditorDraft): boolean {
  const changed = state.context.kind === 'create'
    ? state.dates.map((_, i) => i) : draftDateChanges(state).map(change => change.step - 1);
  if (state.dates.some((date, i) => date === null && canDraftStep(state, i))) return false;
  try { validateProposal(state, state.dates, changed); return true; } catch { return false; }
}
