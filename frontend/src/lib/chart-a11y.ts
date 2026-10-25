/**
 * Text alternatives for Chart.js canvases (issue #744).
 *
 * A <canvas role="img"> exposes only its aria-label, so a fixed label leaves
 * screen-reader users with no values at all. Each chart therefore gets:
 *   - an aria-label that summarizes what is currently plotted, and
 *   - a collapsed "View as table" disclosure listing every plotted value,
 *     which also helps sighted users who can't tell the series apart.
 *
 * The table is inserted after an anchor element the caller chooses (the
 * canvas itself, or the canvas's dedicated sizing container) so it never sits
 * inside a box Chart.js measures. DOM construction only, no innerHTML.
 */

export interface ChartTableData {
  /** Table caption; normally the chart's heading. */
  caption: string;
  /** Column headers. The first column labels each row. */
  columns: readonly string[];
  /** Cell text, one array per row, already formatted for display. */
  rows: ReadonlyArray<readonly string[]>;
}

/** Replace a canvas's accessible name with a summary of the plotted data. */
export function setChartSummary(canvas: HTMLCanvasElement, summary: string): void {
  canvas.setAttribute('aria-label', summary);
}

/**
 * Render (or re-render) the "View as table" disclosure with the given id
 * immediately after `anchor`. An existing disclosure keeps its open/closed
 * state across data refreshes.
 */
export function renderChartTable(anchor: Element, id: string, data: ChartTableData): void {
  let details = document.getElementById(id) as HTMLDetailsElement | null;
  if (!details) {
    details = document.createElement('details');
    details.id = id;
    details.className = 'chart-table';
    const summary = document.createElement('summary');
    summary.textContent = 'View as table';
    details.appendChild(summary);
    anchor.insertAdjacentElement('afterend', details);
  }
  details.querySelector('.chart-table-scroll')?.remove();

  const scroll = document.createElement('div');
  scroll.className = 'chart-table-scroll';
  const table = document.createElement('table');
  const caption = document.createElement('caption');
  caption.textContent = data.caption;
  table.appendChild(caption);

  const headRow = table.createTHead().insertRow();
  for (const col of data.columns) {
    const th = document.createElement('th');
    th.scope = 'col';
    th.textContent = col;
    headRow.appendChild(th);
  }
  const body = table.createTBody();
  for (const row of data.rows) {
    const tr = body.insertRow();
    row.forEach((cell, i) => {
      const el = document.createElement(i === 0 ? 'th' : 'td');
      if (i === 0) (el as HTMLTableCellElement).scope = 'row';
      el.textContent = cell;
      tr.appendChild(el);
    });
  }
  scroll.appendChild(table);
  details.appendChild(scroll);
}

/** Remove a chart's table, e.g. when the chart is replaced by an empty state. */
export function removeChartTable(id: string): void {
  document.getElementById(id)?.remove();
}
