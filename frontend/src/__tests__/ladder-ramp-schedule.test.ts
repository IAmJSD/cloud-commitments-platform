/**
 * Ramp schedule model: decoding, validation and JSON output must match the
 * backend (internal/config/validation.go validateLadderRampSchedule, which
 * decodes into cloud-commitments-go pkg/ladder.RampSchedule and calls its
 * Validate).
 */
import {
  DEFAULT_RAMP_STEPS, RAMP_PRESETS, RAMP_SUM_EPSILON, decodeRampSchedule, formatRampJson,
  fractionToPercent, parseRampJson, percentToFraction, rampJsonFor, rampTextProblem,
  rampTotalPercent, validateRampSteps, type RampStep,
} from '../ladder-ramp-schedule';

const steps = (...pairs: Array<[number, number]>): RampStep[] =>
  pairs.map(([after_days, fraction]) => ({ after_days, fraction }));

describe('parseRampJson / decodeRampSchedule (Go json.Unmarshal into RampSchedule)', () => {
  test('a normal schedule decodes to its steps', () => {
    expect(parseRampJson('{"steps":[{"after_days":0,"fraction":0.5},{"after_days":7,"fraction":0.5}]}'))
      .toEqual({ ok: true, base: expect.any(Object), steps: steps([0, 0.5], [7, 0.5]) });
  });

  test('surrounding whitespace is ignored, as the save path trims', () => {
    expect(parseRampJson('  \n{"steps":[]}\n ')).toMatchObject({ ok: true, steps: [] });
  });

  test('invalid JSON reports the parser message', () => {
    const r = parseRampJson('{"steps": [');
    expect(r.ok).toBe(false);
    expect(!r.ok && r.message).toMatch(/^Not valid JSON: /);
  });

  test('keys match case-insensitively, the last matching key winning', () => {
    expect(parseRampJson('{"Steps":[{"AFTER_DAYS":3,"Fraction":1}]}'))
      .toMatchObject({ ok: true, steps: steps([3, 1]) });
    expect(parseRampJson('{"steps":[],"STEPS":[{"fraction":1}]}'))
      .toMatchObject({ ok: true, steps: steps([0, 1]) });
  });

  test('missing or null fields decode to Go zero values', () => {
    expect(parseRampJson('{}')).toMatchObject({ ok: true, steps: [] });
    expect(parseRampJson('null')).toMatchObject({ ok: true, steps: [] });
    expect(parseRampJson('{"steps":null}')).toMatchObject({ ok: true, steps: [] });
    expect(parseRampJson('{"steps":[null,{"fraction":1},{"after_days":null,"fraction":null}]}'))
      .toMatchObject({ ok: true, steps: steps([0, 0], [0, 1], [0, 0]) });
  });

  test('unknown keys are ignored but kept in base', () => {
    const r = parseRampJson('{"note":"x","steps":[{"after_days":0,"fraction":1,"extra":true}]}');
    expect(r).toMatchObject({ ok: true, steps: steps([0, 1]), base: { note: 'x' } });
  });

  test.each([
    ['[]', /JSON object/],
    ['"steps"', /JSON object/],
    ['{"steps":{}}', /"steps" must be an array/],
    ['{"steps":[1]}', /Step 1 must be an object/],
    ['{"steps":[{"after_days":"7","fraction":1}]}', /Step 1: after_days must be a whole number/],
    ['{"steps":[{"after_days":1.5,"fraction":1}]}', /Step 1: after_days must be a whole number/],
    ['{"steps":[{"after_days":true,"fraction":1}]}', /Step 1: after_days must be a whole number/],
    ['{"steps":[{"after_days":0,"fraction":"1"}]}', /Step 1: fraction must be a number/],
  ])('%s is a type error, as Go refuses to decode it', (text, message) => {
    const r = parseRampJson(text);
    expect(r.ok).toBe(false);
    expect(!r.ok && r.message).toMatch(message);
  });

  test('decodeRampSchedule takes an already-parsed value', () => {
    expect(decodeRampSchedule({ steps: [{ after_days: 0, fraction: 1 }] })).toMatchObject({ ok: true, steps: steps([0, 1]) });
  });
});

describe('validateRampSteps (pkg/ladder RampSchedule.Validate)', () => {
  test('valid schedules have no issues', () => {
    expect(validateRampSteps(DEFAULT_RAMP_STEPS)).toEqual([]);
    expect(validateRampSteps(steps([0, 0.5], [7, 0.25], [30, 0.25]))).toEqual([]);
    // The first step may start later than day 0.
    expect(validateRampSteps(steps([5, 1]))).toEqual([]);
    for (const preset of RAMP_PRESETS) expect(validateRampSteps(preset.steps)).toEqual([]);
  });

  test('no steps', () => {
    expect(validateRampSteps([])).toEqual([{ step: null, field: null, message: 'Add at least one step.' }]);
  });

  test('after_days must start at 0 or more (Go starts prev at -1)', () => {
    expect(validateRampSteps(steps([-1, 1]))).toEqual([{ step: 0, field: 'after', message: 'Days must be 0 or more.' }]);
  });

  test('after_days must strictly increase: duplicates and decreases are refused', () => {
    expect(validateRampSteps(steps([0, 0.5], [0, 0.5]))).toEqual([
      { step: 1, field: 'after', message: 'Must be later than step 1 (day 0).' },
    ]);
    expect(validateRampSteps(steps([0, 0.25], [30, 0.25], [14, 0.5]))).toEqual([
      { step: 2, field: 'after', message: 'Must be later than step 2 (day 30).' },
    ]);
  });

  test('each fraction must be in (0, 1]', () => {
    expect(validateRampSteps(steps([0, 0], [7, 1]))).toEqual([
      { step: 0, field: 'share', message: 'Share must be more than 0%.' },
    ]);
    expect(validateRampSteps(steps([0, -0.5], [7, 1.5]))).toEqual([
      { step: 0, field: 'share', message: 'Share must be more than 0%.' },
      { step: 1, field: 'share', message: 'Share can be at most 100%.' },
    ]);
    expect(validateRampSteps(steps([0, 1]))).toEqual([]);
  });

  test('fractions must sum to 1 within 1e-9, summed in order like Go', () => {
    expect(validateRampSteps(steps([0, 0.5], [7, 0.4]))).toEqual([
      { step: null, field: null, message: 'Shares add up to 90%; they must total 100%.' },
    ]);
    expect(validateRampSteps(steps([0, 0.1], [1, 0.2], [2, 0.7]))).toEqual([]);
    expect(validateRampSteps(steps([0, 0.5], [7, 0.5 + RAMP_SUM_EPSILON / 2]))).toEqual([]);
    const over = validateRampSteps(steps([0, 0.5], [7, 0.5 + 2 * RAMP_SUM_EPSILON]));
    expect(over).toHaveLength(1);
    expect(over[0]!.message).not.toContain('100%;');
    // Thirds written out to 17 digits sum to 1.
    expect(validateRampSteps(steps([0, 1 / 3], [7, 1 / 3], [14, 1 / 3]))).toEqual([]);
  });

  test('rows still being typed (NaN) are named', () => {
    expect(validateRampSteps([{ after_days: NaN, fraction: NaN }])).toEqual([
      { step: 0, field: 'after', message: 'Enter the number of days as a whole number.' },
      { step: 0, field: 'share', message: 'Enter a share.' },
    ]);
  });

  test('rampTextProblem gives the first problem the backend would report', () => {
    expect(rampTextProblem('{"steps":[{"after_days":0,"fraction":1}]}')).toBeNull();
    expect(rampTextProblem('{"steps":[{"after_days":0,"fraction":0.5},{"after_days":0,"fraction":0.5}]}'))
      .toBe('Step 2: Must be later than step 1 (day 0).');
    expect(rampTextProblem('{"steps":[]}')).toBe('Add at least one step.');
    expect(rampTextProblem('nope')).toMatch(/^Not valid JSON/);
  });
});

describe('percent conversion', () => {
  test('fractionToPercent drops float noise', () => {
    expect(fractionToPercent(0.07)).toBe('7');
    expect(fractionToPercent(0.5)).toBe('50');
    expect(fractionToPercent(1 / 3)).toBe('33.3333333333');
    expect(fractionToPercent(1e-10)).toBe('1e-8');
  });

  test('percentToFraction gives the nearest double to the decimal typed', () => {
    expect(percentToFraction('33.3')).toBe(0.333);
    expect(33.3 / 100).not.toBe(0.333); // why the decimal shift matters
    expect(percentToFraction('7')).toBe(0.07);
    expect(percentToFraction('100')).toBe(1);
    expect(percentToFraction('1e-5')).toBe(1e-7);
    expect(percentToFraction('')).toBeNaN();
    expect(percentToFraction('abc')).toBeNaN();
  });

  test('rampTotalPercent never shows 100 for a sum the backend refuses', () => {
    expect(rampTotalPercent(steps([0, 0.5], [1, 0.5]))).toBe('100');
    expect(rampTotalPercent(steps([0, 0.5], [1, 0.5 + 1e-8]))).not.toBe('100');
  });
});

describe('rampJsonFor', () => {
  test('writes the same pretty JSON the modal always used', () => {
    expect(rampJsonFor({}, DEFAULT_RAMP_STEPS)).toBe(formatRampJson({ steps: [{ after_days: 0, fraction: 1 }] }));
    expect(rampJsonFor({ steps: [] }, steps([0, 0.5], [7, 0.5])))
      .toBe(JSON.stringify({ steps: [{ after_days: 0, fraction: 0.5 }, { after_days: 7, fraction: 0.5 }] }, null, 2));
  });

  test('keeps other top-level keys in place and collapses case variants of steps', () => {
    const json = rampJsonFor({ a: 1, Steps: [], b: 2, STEPS: [] }, steps([0, 1]));
    expect(Object.keys(JSON.parse(json))).toEqual(['a', 'steps', 'b']);
    expect(JSON.parse(json).steps).toEqual([{ after_days: 0, fraction: 1 }]);
  });

  test('a "__proto__" key survives as a plain key', () => {
    const base = JSON.parse('{"__proto__":{"x":1},"steps":[]}') as Record<string, unknown>;
    expect(JSON.parse(rampJsonFor(base, steps([0, 1])))).toEqual(JSON.parse('{"__proto__":{"x":1},"steps":[{"after_days":0,"fraction":1}]}'));
  });

  test('round-trips exact fractions', () => {
    const exact = steps([0, 1 / 3], [7, 1 / 3], [14, 1 / 3]);
    const back = parseRampJson(rampJsonFor({}, exact));
    expect(back).toMatchObject({ ok: true, steps: exact });
  });
});
