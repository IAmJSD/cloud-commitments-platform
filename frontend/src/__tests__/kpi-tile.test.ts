/**
 * Issue #743: lib/kpi-tile builds the one KPI tile used by Home and
 * Opportunities.
 */
import { buildKpiTile, renderKpiTiles } from '../lib/kpi-tile';
import { NO_VALUE } from '../utils';

describe('buildKpiTile', () => {
  test('builds the full tile: title, value, detail and hidden spark slot', () => {
    const tile = buildKpiTile({
      kpi: 'savings', title: 'Potential Monthly Savings', value: '$300', valueSavings: true,
      detail: '2 recommendations', spark: true,
    });
    expect(tile.className).toBe('card kpi-tile');
    expect(tile.dataset['kpi']).toBe('savings');
    expect(tile.querySelector('h3')?.textContent).toBe('Potential Monthly Savings');
    const value = tile.querySelector('p.value')!;
    expect(value.className).toBe('value kpi-tile-value savings');
    expect(value.textContent).toBe('$300');
    expect(tile.querySelector('p.detail.kpi-tile-detail')?.textContent).toBe('2 recommendations');
    const spark = tile.querySelector('svg.kpi-tile-spark')!;
    expect(spark.classList.contains('hidden')).toBe(true);
    expect(spark.getAttribute('data-spark-key')).toBe('savings');
    expect(spark.getAttribute('aria-hidden')).toBe('true');
  });

  test('omits the detail line, savings color and spark slot when not asked for', () => {
    const tile = buildKpiTile({ kpi: 'count', title: 'Total Recommendations', value: '19' });
    expect(tile.querySelector('p.value')?.classList.contains('savings')).toBe(false);
    expect(tile.querySelector('.kpi-tile-detail')).toBeNull();
    expect(tile.querySelector('svg')).toBeNull();
    expect(tile.children).toHaveLength(2);
  });

  test('keeps an absent (NO_VALUE) savings figure neutral, not green (issue #739)', () => {
    const tile = buildKpiTile({ kpi: 'savings', title: 'Potential Monthly Savings', value: NO_VALUE, valueSavings: true });
    const value = tile.querySelector('p.value')!;
    expect(value.textContent).toBe(NO_VALUE);
    expect(value.classList.contains('savings')).toBe(false);
  });

  test('an empty-string detail still renders the line', () => {
    const tile = buildKpiTile({ kpi: 'x', title: 'T', value: 'V', detail: '' });
    expect(tile.querySelector('.kpi-tile-detail')).not.toBeNull();
  });

  test('treats title, value and detail as text, never markup', () => {
    const tile = buildKpiTile({ kpi: 'x', title: '<b>t</b>', value: '<img src=x onerror=alert(1)>', detail: '<i>d</i>' });
    expect(tile.querySelector('img, b, i')).toBeNull();
    expect(tile.querySelector('p.value')?.textContent).toBe('<img src=x onerror=alert(1)>');
  });
});

describe('renderKpiTiles', () => {
  test('replaces the container contents with one tile per spec, in order', () => {
    const container = document.createElement('section');
    container.innerHTML = '<p class="error">old</p>';
    renderKpiTiles(container, [
      { kpi: 'a', title: 'A', value: '1' },
      { kpi: 'b', title: 'B', value: '2' },
    ]);
    expect(container.querySelector('.error')).toBeNull();
    expect(Array.from(container.children).map((c) => (c as HTMLElement).dataset['kpi'])).toEqual(['a', 'b']);
  });
});
