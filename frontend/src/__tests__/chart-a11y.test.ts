/**
 * Issue #744: text alternatives for chart canvases.
 */
import { setChartSummary, renderChartTable, removeChartTable } from '../lib/chart-a11y';

describe('chart-a11y', () => {
  let canvas: HTMLCanvasElement;

  beforeEach(() => {
    document.body.innerHTML = '<section><canvas id="c" role="img" aria-label="static"></canvas><p id="after"></p></section>';
    canvas = document.getElementById('c') as HTMLCanvasElement;
  });

  const data = {
    caption: 'Savings',
    columns: ['Service', 'Current', 'Total'],
    rows: [['compute', '$70', '$210'], ['cache', '$25', '$110']],
  };

  test('setChartSummary replaces the static aria-label', () => {
    setChartSummary(canvas, 'Bar chart of 2 services, $320 in total.');
    expect(canvas.getAttribute('aria-label')).toBe('Bar chart of 2 services, $320 in total.');
  });

  test('renders a collapsed "View as table" disclosure right after the anchor', () => {
    renderChartTable(canvas, 't', data);
    const details = document.getElementById('t') as HTMLDetailsElement;
    expect(details.tagName).toBe('DETAILS');
    expect(details.open).toBe(false);
    expect(canvas.nextElementSibling).toBe(details);
    expect(details.querySelector('summary')?.textContent).toBe('View as table');
    expect(details.querySelector('caption')?.textContent).toBe('Savings');
  });

  test('uses column and row headers so screen readers can announce each cell', () => {
    renderChartTable(canvas, 't', data);
    const table = document.querySelector('#t table') as HTMLTableElement;
    const head = Array.from(table.tHead!.querySelectorAll('th'));
    expect(head.map((th) => th.textContent)).toEqual(['Service', 'Current', 'Total']);
    expect(head.every((th) => th.scope === 'col')).toBe(true);
    const rows = Array.from(table.tBodies[0]!.rows);
    expect(rows.map((r) => Array.from(r.cells).map((c) => c.textContent))).toEqual(data.rows);
    for (const r of rows) {
      expect(r.cells[0]!.tagName).toBe('TH');
      expect((r.cells[0] as HTMLTableCellElement).scope).toBe('row');
      expect(r.cells[1]!.tagName).toBe('TD');
    }
  });

  test('re-rendering replaces the rows and keeps the open state', () => {
    renderChartTable(canvas, 't', data);
    (document.getElementById('t') as HTMLDetailsElement).open = true;
    renderChartTable(canvas, 't', { ...data, rows: [['rds', '$1', '$2']] });
    expect(document.querySelectorAll('#t').length).toBe(1);
    expect(document.querySelectorAll('#t table').length).toBe(1);
    expect(document.querySelectorAll('#t tbody tr').length).toBe(1);
    expect((document.getElementById('t') as HTMLDetailsElement).open).toBe(true);
  });

  test('cell text is never parsed as HTML', () => {
    renderChartTable(canvas, 't', { ...data, rows: [['<img src=x onerror=alert(1)>', '$1', '$2']] });
    expect(document.querySelector('#t img')).toBeNull();
    expect(document.querySelector('#t tbody th')?.textContent).toBe('<img src=x onerror=alert(1)>');
  });

  test('removeChartTable drops the disclosure and tolerates a missing one', () => {
    renderChartTable(canvas, 't', data);
    removeChartTable('t');
    expect(document.getElementById('t')).toBeNull();
    expect(() => removeChartTable('t')).not.toThrow();
  });
});
