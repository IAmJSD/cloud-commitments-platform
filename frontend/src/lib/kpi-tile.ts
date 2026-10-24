/**
 * Shared KPI tile builder (issue #743).
 *
 * Home's summary built `.card.kpi-tile` elements by hand while Opportunities
 * wrote plain `.card` innerHTML, so the same kind of figure looked different
 * on the two pages. Both now build their tiles here.
 *
 * Markup (styles in components.css under `.kpi-tile`):
 *   <div class="card kpi-tile" data-kpi="savings">
 *     <h3>Title</h3>
 *     <p class="value kpi-tile-value [savings]">Value</p>
 *     <p class="detail kpi-tile-detail">Detail</p>        (optional)
 *     <svg class="kpi-tile-spark hidden" data-spark-key>  (optional)
 *   </div>
 *
 * The `card` / `value` / `savings` classes stay for existing selectors and
 * tests. Text is set with textContent, never innerHTML.
 */

export interface KpiTileSpec {
  /** Stable key, rendered as data-kpi (and data-spark-key when spark is set). */
  kpi: string;
  title: string;
  value: string;
  /** Render the value in the savings (green) color. */
  valueSavings?: boolean;
  /** Small line under the value; omitted when absent. */
  detail?: string;
  /** Reserve a hidden sparkline slot that a caller fills in later. */
  spark?: boolean;
}

const SVG_NS = 'http://www.w3.org/2000/svg';

export function buildKpiTile(spec: KpiTileSpec): HTMLDivElement {
  const card = document.createElement('div');
  card.classList.add('card', 'kpi-tile');
  card.dataset['kpi'] = spec.kpi;

  const h3 = document.createElement('h3');
  h3.textContent = spec.title;
  card.appendChild(h3);

  const value = document.createElement('p');
  value.classList.add('value', 'kpi-tile-value');
  if (spec.valueSavings) value.classList.add('savings');
  value.textContent = spec.value;
  card.appendChild(value);

  if (spec.detail !== undefined) {
    const detail = document.createElement('p');
    detail.classList.add('detail', 'kpi-tile-detail');
    detail.textContent = spec.detail;
    card.appendChild(detail);
  }

  if (spec.spark) {
    const spark = document.createElementNS(SVG_NS, 'svg');
    spark.classList.add('kpi-tile-spark', 'hidden');
    spark.dataset['sparkKey'] = spec.kpi;
    spark.setAttribute('aria-hidden', 'true');
    card.appendChild(spark);
  }

  return card;
}

/** Replace `container`'s children with one tile per spec. */
export function renderKpiTiles(container: HTMLElement, specs: readonly KpiTileSpec[]): void {
  container.replaceChildren(...specs.map(buildKpiTile));
}
