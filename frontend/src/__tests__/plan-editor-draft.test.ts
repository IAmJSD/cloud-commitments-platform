import {
  canDraftStep, canReviewEditorDraft, createEditorDraft, draftBlock, draftDateChanges,
  EditorSlot, redistributeEditorBlock, resetEditorDraft, selectDraftStep, setApprovedDraftEdits, setDraftDate,
} from '../lib/plan-editor-draft';

const slot = (step: number, date: string | null, override: Partial<EditorSlot> = {}): EditorSlot => ({
  step_number: step, scheduled_date: date, cumulative_target_percent: step * 33,
  execution_id: `root-${step}`, logical_completed: false, root_state: 'unique',
  capability: 'replace_pending', editable: true, requires_approved_edits: false, ...override,
});
const edit = (slots: EditorSlot[]) => createEditorDraft({ kind: 'edit', planId: 'plan', revision: 'opaque-revision', totalSteps: slots.length, slots });
const local = (dates: (string | null)[], shares = dates.map((_, i) => (i + 1) * 100 / dates.length)) =>
  createEditorDraft({ kind: 'create', totalSteps: dates.length, shares, dates });

describe('controlled editor dates', () => {
  beforeEach(() => { jest.useFakeTimers(); jest.setSystemTime(new Date('2026-10-15T12:00:00Z')); });
  afterEach(() => jest.useRealTimers());

  test('actual Create has local intent without persisted identity or authority', () => {
    const state = local([null]);
    const next = setDraftDate(state, 0, '2026-10-20');
    expect(next.dates).toEqual(['2026-10-20']);
    expect(state.dates).toEqual([null]);
    expect(next.context).not.toHaveProperty('revision');
    expect(next.context).not.toHaveProperty('capability');
    expect(canReviewEditorDraft(next)).toBe(true);
  });
  test('all Create proposals need fresh future/order checks even unchanged or reset', () => {
    const past = local(['2026-10-14']);
    expect(canReviewEditorDraft(past)).toBe(false);
    const today = local(['2026-10-16']);
    expect(canReviewEditorDraft(today)).toBe(true);
    jest.setSystemTime(new Date('2026-10-16T00:00:00Z'));
    expect(canReviewEditorDraft(today)).toBe(false);
    expect(canReviewEditorDraft(resetEditorDraft(today))).toBe(false);
    expect(canReviewEditorDraft(local(['2026-10-20', '2026-10-19']))).toBe(false);
  });
  test('Create missing dates are explicit proposals and cannot be redistributed as placeholders', () => {
    const state = local([null, null]);
    expect(canReviewEditorDraft(state)).toBe(false);
    expect(() => redistributeEditorBlock(state, '2026-10-20', '2026-10-24')).toThrow('every block date');
    const first = setDraftDate(state, 0, '2026-10-20');
    expect(() => setDraftDate(first, 1, '2026-10-20')).toThrow('order');
    expect(setDraftDate(first, 1, '2026-10-21').dates).toEqual(['2026-10-20', '2026-10-21']);
  });
  test.each(['2026-02-30', '0000-01-01', '2026-1-01', '2026-10-15', '2026-10-14'])(
    'rejects invalid or nonfuture proposed date %s atomically', date => {
      const state = local(['2026-10-20']);
      expect(() => setDraftDate(state, 0, date)).toThrow();
      expect(state.dates).toEqual(['2026-10-20']);
    });
  test('valid leap dates and operation-time midnight are handled in UTC', () => {
    const state = local(['2028-02-29']);
    expect(canReviewEditorDraft(state)).toBe(true);
    jest.setSystemTime(new Date('2028-02-29T00:00:00Z'));
    expect(() => setDraftDate(state, 0, '2028-02-29')).toThrow('future');
    expect(setDraftDate(state, 0, '2028-03-01').dates).toEqual(['2028-03-01']);
  });
  test.each([
    { capability: 'fixed' }, { capability: 'denied' }, { root_state: 'ambiguous' },
    { logical_completed: true }, { editable: false }, { execution_id: null }, { requires_approved_edits: true },
  ] as Partial<EditorSlot>[])('does not unlock an inconsistent or fixed existing slot %j', override => {
    const state = edit([slot(1, '2026-10-20', override)]);
    expect(canDraftStep(setApprovedDraftEdits(state, true), 0)).toBe(false);
    expect(() => setDraftDate(state, 0, '2026-10-21')).toThrow('locked');
  });
  test('missing needs_creation is intentional despite editable false and no existing ID', () => {
    const missing = slot(1, null, { execution_id: null, root_state: 'missing', capability: 'needs_creation', editable: false });
    const state = edit([missing]);
    expect(canDraftStep(state, 0)).toBe(true);
    expect(canReviewEditorDraft(state)).toBe(false);
    expect(draftDateChanges(setDraftDate(state, 0, '2026-10-20'))).toEqual([
      { step: 1, before: null, after: '2026-10-20', executionId: null, requiresReapproval: false },
    ]);
    expect(canDraftStep(edit([{ ...missing, editable: true }]), 0)).toBe(false);
  });
  test('approved toggle retains dirty proposals while disabling Review and reset clears consent', () => {
    const state = edit([slot(1, '2026-10-20', {
      capability: 'approved_toggle_required', editable: false, requires_approved_edits: true,
    })]);
    expect(canDraftStep(state, 0)).toBe(false);
    const changed = setDraftDate(setApprovedDraftEdits(state, true), 0, '2026-10-22');
    expect(draftDateChanges(changed)[0]!.requiresReapproval).toBe(true);
    const locked = setApprovedDraftEdits(changed, false);
    expect(locked.dates).toEqual(['2026-10-22']);
    expect(canReviewEditorDraft(locked)).toBe(false);
    expect(canReviewEditorDraft(setApprovedDraftEdits(locked, true))).toBe(true);
    expect(resetEditorDraft(changed).approvedEdits).toBe(false);
    expect(resetEditorDraft(changed).dates).toEqual(['2026-10-20']);
    expect(createEditorDraft(changed.context).approvedEdits).toBe(false);
  });
  test('completed historical prefix stays fixed and does not invalidate unchanged past dates', () => {
    const state = edit([slot(1, '2026-09-01', { logical_completed: true, capability: 'fixed', editable: false }),
      slot(2, '2026-10-20'), slot(3, '2026-10-24')]);
    expect(draftBlock(state)).toBeNull();
    expect(draftBlock(selectDraftStep(state, 1))).toEqual([1, 2]);
    const next = redistributeEditorBlock(selectDraftStep(state, 1), '2026-10-21', '2026-10-23');
    expect(next.dates).toEqual(['2026-09-01', '2026-10-21', '2026-10-23']);
    expect(canReviewEditorDraft(next)).toBe(true);
    expect(next.context).toEqual(state.context);
  });
  test('fixed neighbors and ordinal distances across missing dates constrain changes', () => {
    const missing = slot(2, null, { execution_id: null, root_state: 'missing', capability: 'needs_creation', editable: false });
    const state = edit([slot(1, '2026-10-20'), missing, slot(3, '2026-10-24', { capability: 'fixed', editable: false })]);
    expect(() => setDraftDate(state, 0, '2026-10-23')).toThrow('order');
    expect(setDraftDate(state, 0, '2026-10-22').dates).toEqual(['2026-10-22', null, '2026-10-24']);
    const unknown = edit([slot(1, null, { capability: 'fixed', editable: false }), slot(2, '2026-10-24')]);
    expect(() => setDraftDate(unknown, 1, '2026-10-25')).toThrow('unavailable');
  });
  test('legacy33 shares and exact identities survive range edits without normalization', () => {
    const state = edit([slot(1, '2026-10-20'), slot(2, '2026-10-24'), slot(3, '2026-10-28')]);
    const next = redistributeEditorBlock(state, '2026-10-21', '2026-10-29');
    expect(next.dates).toEqual(['2026-10-21', '2026-10-25', '2026-10-29']);
    expect(next.context).toEqual(state.context);
    expect(state.context.kind === 'edit' && state.context.slots[2]!.cumulative_target_percent).toBe(99);
  });
  test('one and one hundred local steps reuse bounded redistribution', () => {
    expect(redistributeEditorBlock(local(['2026-10-20']), '2026-10-21', 'ignored').dates).toEqual(['2026-10-21']);
    const dates = Array.from({ length: 100 }, (_, i) => new Date(Date.UTC(2026, 9, 20 + i)).toISOString().slice(0, 10));
    const state = local(dates);
    expect(redistributeEditorBlock(state, dates[0]!, dates[99]!).dates).toEqual(dates);
    expect(() => redistributeEditorBlock(state, dates[0]!, dates[98]!)).toThrow('at least one');
  });
  test('inputs and returned changes cannot alias the baseline and context replacement drops old consent', () => {
    const rows = [slot(1, '2026-10-20')];
    const state = edit(rows);
    rows[0]!.scheduled_date = '2026-10-30';
    expect(state.dates).toEqual(['2026-10-20']);
    const next = setDraftDate(state, 0, '2026-10-21');
    const changes = draftDateChanges(next); changes[0]!.after = '2026-10-31';
    expect(draftDateChanges(next)[0]!.after).toBe('2026-10-21');
    expect(local([null]).approvedEdits).toBe(false);
    expect(() => selectDraftStep(state, 1)).toThrow('existing');
  });
  test('refuses unsupported counts, shares, ordinals, dates and revision without coercion', () => {
    expect(() => local([])).toThrow('count');
    for (const totalSteps of [1, 3]) expect(() => createEditorDraft({ kind: 'edit', planId: 'plan', revision: 'revision', totalSteps, slots: [slot(1, '2026-10-20'), slot(2, '2026-10-21')] })).toThrow('Incomplete');
    expect(() => local(['2026-10-20'], [Infinity])).toThrow('shares');
    expect(() => local(['2026-10-20'], [101])).toThrow('shares');
    expect(() => local(['2026-02-30'])).toThrow('valid');
    expect(() => edit([slot(2, '2026-10-20')])).toThrow('projection');
    expect(() => createEditorDraft({ kind: 'edit', planId: 'plan', revision: '', totalSteps: 1, slots: [slot(1, '2026-10-20')] })).toThrow('revision');
  });
  test('keeps completed slots locked even when replacement capability is present', () => {
    const state = edit([slot(1, '2026-10-20', { logical_completed: true })]);
    expect(canDraftStep(state, 0)).toBe(false);
  });
  test('refuses crossing a known fixed neighbor', () => {
    const state = edit([slot(1, '2026-10-20'), slot(2, '2026-10-22', { capability: 'fixed', editable: false })]);
    expect(() => setDraftDate(state, 0, '2026-10-23')).toThrow('order');
  });
  test('typed context reuse cannot carry persisted identity into Create or local fields into Edit', () => {
    const persisted = { kind: 'edit' as const, planId: 'persisted', revision: 'revision', totalSteps: 1, slots: [slot(1, '2026-10-20')] };
    const reused = { ...persisted, kind: 'create' as const, shares: [100], dates: [null] };
    const fresh = createEditorDraft(reused);
    expect(fresh.context).toEqual({ kind: 'create', totalSteps: 1, shares: [100], dates: [null] });
    const returning = createEditorDraft({ ...reused, kind: 'edit', slots: persisted.slots });
    expect(returning.context).toEqual(persisted);
    expect(returning.approvedEdits).toBe(false);
  });
});
