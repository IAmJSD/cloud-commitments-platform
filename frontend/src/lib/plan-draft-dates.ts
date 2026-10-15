const DAY = 86_400_000;

export function draftDateDay(date: string): number {
  const time = Date.parse(`${date}T00:00:00Z`);
  if (!/^\d{4}-\d{2}-\d{2}$/.test(date) || date.startsWith('0000') || !Number.isFinite(time)
    || new Date(time).toISOString().slice(0, 10) !== date) throw new Error('Choose a valid UTC calendar date.');
  return time / DAY;
}

export function validateDraftDates(dates: readonly string[]): void {
  const today = Math.floor(Date.now() / DAY);
  let previous = -Infinity;
  for (const date of dates) {
    const day = draftDateDay(date);
    if (day <= today) throw new Error('Draft dates must remain in the future (UTC).');
    if (day <= previous) throw new Error('Keep dates in step order, at least one calendar day apart.');
    previous = day;
  }
}

export function redistributeDraftDates(dates: readonly string[], start: string, end: string): string[] {
  const first = draftDateDay(start), last = dates.length === 1 ? first : draftDateDay(end);
  if (last - first < dates.length - 1) throw new Error('Allow at least one calendar day between every draft step.');
  const result = dates.map((_, index) => new Date((first + (dates.length === 1 ? 0
    : Math.round(index * (last - first) / (dates.length - 1)))) * DAY).toISOString().slice(0, 10));
  return result;
}
