/**
 * Keep keyboard focus across an innerHTML rebuild (issue #191).
 *
 * The Opportunities table is rebuilt from scratch on every selection,
 * expand/collapse and sort change. Rebuilding drops the focused element, so
 * a keyboard user who pressed Space on a row checkbox landed back on <body>
 * and the next Tab restarted at the top of the table.
 *
 * `captureFocus(container)` records which control inside `container` has
 * focus, by tag plus the stable attributes the table markup carries.
 * `restoreFocus(container, snapshot)` focuses the matching control in the
 * rebuilt markup. If it is gone (filtered out, collapsed), focus is left alone.
 *
 * Matching compares attributes directly instead of building a CSS selector,
 * so attribute values never need escaping.
 */

/** Attributes that identify a control across rebuilds of the same table. */
const KEY_ATTRS = [
  'id',
  'type',
  'data-rec-id',
  'data-action',
  'data-cell-key',
  'data-sp-group-key',
  'data-sort',
  'data-column',
] as const;

export interface FocusSnapshot {
  tag: string;
  attrs: ReadonlyArray<readonly [string, string]>;
}

/**
 * Snapshot the focused element inside `container`, or null when focus is
 * elsewhere or the element carries none of the identifying attributes.
 */
export function captureFocus(container: HTMLElement): FocusSnapshot | null {
  const active = document.activeElement;
  if (!(active instanceof HTMLElement) || active === container || !container.contains(active)) {
    return null;
  }
  const attrs: Array<readonly [string, string]> = [];
  for (const name of KEY_ATTRS) {
    const value = active.getAttribute(name);
    if (value !== null && value !== '') attrs.push([name, value]);
  }
  // `type` alone (e.g. every checkbox) is not an identity.
  if (attrs.every(([name]) => name === 'type')) return null;
  return { tag: active.tagName, attrs };
}

/**
 * Focus the element in `container` matching `snapshot`. Returns true when
 * focus moved. No-op when the snapshot is null, focus already moved
 * somewhere else on purpose (e.g. a modal opened), or no match exists.
 */
export function restoreFocus(container: HTMLElement, snapshot: FocusSnapshot | null): boolean {
  if (!snapshot) return false;
  const active = document.activeElement;
  if (active && active !== document.body && container.ownerDocument.contains(active)) {
    return false;
  }
  for (const el of Array.from(container.getElementsByTagName(snapshot.tag))) {
    if (!(el instanceof HTMLElement)) continue;
    if (snapshot.attrs.every(([name, value]) => el.getAttribute(name) === value)) {
      el.focus({ preventScroll: true });
      return document.activeElement === el;
    }
  }
  return false;
}
