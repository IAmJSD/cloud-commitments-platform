/**
 * Ramp schedule model for the per-account ladder config (the visual editor in
 * ladder-ramp-editor.ts and the save path in ladder.ts).
 *
 * The rules mirror the backend exactly, so the editor never blocks a schedule
 * the server would accept and never lets through one it would refuse:
 *
 *  - Decoding follows Go's encoding/json into cloud-commitments-go
 *    pkg/ladder.RampSchedule (internal/config/validation.go
 *    validateLadderRampSchedule): keys match case-insensitively, a missing or
 *    null `steps` is an empty list, a null step is the zero step, a missing or
 *    null `after_days` / `fraction` is 0, `after_days` must be an integer and
 *    `fraction` a number, and unknown keys are ignored.
 *  - Validation is RampSchedule.Validate (pkg/ladder/types.go): at least one
 *    step, after_days strictly ascending starting from 0 or more (the Go loop
 *    starts from prev = -1), each fraction in (0, 1], and the fractions,
 *    summed in order in float64, within 1e-9 of 1.0.
 */

/** One ramp step, in the JSON shape the API stores and the save path sends. */
export interface RampStep {
  after_days: number;
  fraction: number;
}

/** pkg/ladder rampSumEpsilon: the allowed absolute error of the fraction sum. */
export const RAMP_SUM_EPSILON = 1e-9;

/** The schedule a new config starts from (and the modal always has). */
export const DEFAULT_RAMP_STEPS: readonly RampStep[] = [{ after_days: 0, fraction: 1.0 }];

/** Pretty-printed JSON for the textarea, the same format the modal has always used. */
export function formatRampJson(schedule: unknown): string {
  return JSON.stringify(schedule, null, 2);
}

export type RampParse =
  | { ok: true; base: Record<string, unknown>; steps: RampStep[] }
  | { ok: false; message: string };

// Go's decoder matches object keys to struct fields case-insensitively and,
// when several keys match one field, the last one in the input wins.
function goField(obj: Record<string, unknown>, name: string): unknown {
  let value: unknown;
  for (const key of Object.keys(obj)) if (key.toLowerCase() === name) value = obj[key];
  return value;
}

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function decodeStep(raw: unknown, i: number): RampStep | string {
  if (raw === null) return { after_days: 0, fraction: 0 };
  if (!isObject(raw)) return `Step ${i + 1} must be an object like {"after_days": 0, "fraction": 1}.`;
  const after = goField(raw, 'after_days') ?? 0;
  const fraction = goField(raw, 'fraction') ?? 0;
  // Go decodes after_days into an int; Number.isSafeInteger also keeps the
  // value exact in JavaScript (the backend's int64 range is wider).
  if (typeof after !== 'number' || !Number.isSafeInteger(after)) {
    return `Step ${i + 1}: after_days must be a whole number.`;
  }
  if (typeof fraction !== 'number') return `Step ${i + 1}: fraction must be a number.`;
  return { after_days: after, fraction };
}

/**
 * Decode an already-parsed ramp_schedule value the way the backend does.
 * `base` is the object itself, kept so keys the editor does not know about
 * survive a rewrite of the steps.
 */
export function decodeRampSchedule(raw: unknown): RampParse {
  if (raw === null) return { ok: true, base: {}, steps: [] };
  if (!isObject(raw)) return { ok: false, message: 'The schedule must be a JSON object like {"steps": [...]}.' };
  const rawSteps = goField(raw, 'steps') ?? [];
  if (!Array.isArray(rawSteps)) return { ok: false, message: '"steps" must be an array.' };
  const steps: RampStep[] = [];
  for (const [i, item] of rawSteps.entries()) {
    const step = decodeStep(item, i);
    if (typeof step === 'string') return { ok: false, message: step };
    steps.push(step);
  }
  return { ok: true, base: raw, steps };
}

/** Parse the textarea text (trimmed, as the save path does) and decode it. */
export function parseRampJson(text: string): RampParse {
  let raw: unknown;
  try {
    raw = JSON.parse(text.trim());
  } catch (err) {
    return { ok: false, message: `Not valid JSON: ${(err as Error).message}` };
  }
  return decodeRampSchedule(raw);
}

/**
 * The JSON for `steps`, keeping any other top-level keys of `base` in place.
 * Every key that Go would read as `steps` is replaced by one `steps` key.
 */
export function rampJsonFor(base: Record<string, unknown>, steps: readonly RampStep[]): string {
  // No prototype, so a "__proto__" key from JSON.parse stays a plain key.
  const out = Object.create(null) as Record<string, unknown>;
  for (const [key, value] of Object.entries(base)) {
    if (key.toLowerCase() !== 'steps') out[key] = value;
    else if (!('steps' in out)) out['steps'] = null; // reserve the first position
  }
  out['steps'] = steps.map(s => ({ after_days: s.after_days, fraction: s.fraction }));
  return formatRampJson(out);
}

/** A validation problem: on one step's days or share, or on the whole schedule. */
export interface RampIssue {
  step: number | null;
  field: 'after' | 'share' | null;
  message: string;
}

/** Percent text for a fraction, without float noise (0.07 -> "7"). */
export function fractionToPercent(fraction: number): string {
  return String(Number((fraction * 100).toPrecision(12)));
}

/**
 * The fraction for a percent typed by the user, or NaN when it is not a
 * number. Shifting the decimal point in the text ("33.3" -> 0.333) gives the
 * nearest double to the decimal the user meant, which pct / 100 does not.
 */
export function percentToFraction(text: string): number {
  const t = text.trim();
  if (t === '' || !Number.isFinite(Number(t))) return NaN;
  const exp = /^(.*)e([+-]?\d+)$/i.exec(t);
  return exp ? Number(`${exp[1]}e${Number(exp[2]) - 2}`) : Number(`${t}e-2`);
}

function sumOf(steps: readonly RampStep[]): number {
  // Same order and float64 arithmetic as the Go loop, so the same verdict.
  let sum = 0;
  for (const s of steps) sum += s.fraction;
  return sum;
}

export function rampTotalPercent(steps: readonly RampStep[]): string {
  const sum = sumOf(steps);
  const short = fractionToPercent(sum);
  // Never show "100%" for a total the backend refuses.
  return short === '100' && Math.abs(sum - 1) > RAMP_SUM_EPSILON ? String(sum * 100) : short;
}

/** Every problem RampSchedule.Validate would refuse, per step where it can say which. */
export function validateRampSteps(steps: readonly RampStep[]): RampIssue[] {
  if (steps.length === 0) return [{ step: null, field: null, message: 'Add at least one step.' }];
  const issues: RampIssue[] = [];
  steps.forEach((s, i) => {
    const prev = i > 0 ? steps[i - 1]!.after_days : -1;
    if (Number.isNaN(s.after_days)) {
      issues.push({ step: i, field: 'after', message: 'Enter the number of days as a whole number.' });
    } else if (s.after_days < 0) {
      issues.push({ step: i, field: 'after', message: 'Days must be 0 or more.' });
    } else if (s.after_days <= prev) {
      issues.push({ step: i, field: 'after', message: `Must be later than step ${i} (day ${prev}).` });
    }
    const f = s.fraction;
    if (Number.isNaN(f)) {
      issues.push({ step: i, field: 'share', message: 'Enter a share.' });
    } else if (!(f > 0)) {
      issues.push({ step: i, field: 'share', message: 'Share must be more than 0%.' });
    } else if (f > 1) {
      issues.push({ step: i, field: 'share', message: 'Share can be at most 100%.' });
    }
  });
  const sum = sumOf(steps);
  if (!Number.isNaN(sum) && Math.abs(sum - 1) > RAMP_SUM_EPSILON) {
    issues.push({ step: null, field: null, message: `Shares add up to ${rampTotalPercent(steps)}%; they must total 100%.` });
  }
  return issues;
}

/** The first problem with the textarea text, or null when the backend would accept it. */
export function rampTextProblem(text: string): string | null {
  const parsed = parseRampJson(text);
  if (!parsed.ok) return parsed.message;
  const issue = validateRampSteps(parsed.steps)[0];
  if (!issue) return null;
  return issue.step === null ? issue.message : `Step ${issue.step + 1}: ${issue.message}`;
}

/** Quick starting points offered under the step list. */
export const RAMP_PRESETS: ReadonlyArray<{ id: string; label: string; title: string; steps: readonly RampStep[] }> = [
  { id: 'once', label: 'All at once', title: '100% on day 0', steps: [{ after_days: 0, fraction: 1 }] },
  {
    id: 'monthly-2', label: '2 monthly steps', title: '50% on day 0, 50% on day 30',
    steps: [{ after_days: 0, fraction: 0.5 }, { after_days: 30, fraction: 0.5 }],
  },
  {
    id: 'weekly-4', label: '4 weekly steps', title: '25% each on days 0, 7, 14 and 21',
    steps: [0, 7, 14, 21].map(d => ({ after_days: d, fraction: 0.25 })),
  },
];
