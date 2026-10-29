/**
 * Error callout for a section whose data failed to load.
 *
 * A bare "Failed to load X: reason" line gave no way forward but a full page
 * reload. The callout keeps that line (as `p.error`, so existing selectors
 * and messages still match) and adds an icon, a short hint and, when the
 * caller passes one, a "Try again" button that re-runs the section's own
 * load function. It matches the start-up error card (init-error.ts).
 *
 *   <div class="error-callout" role="alert">
 *     <svg class="error-callout-icon" aria-hidden="true">
 *     <div class="error-callout-body">
 *       <p class="error">Failed to load X: reason</p>
 *       <p class="error-callout-hint">Try again; if the problem continues, ...</p>
 *       <button class="btn btn-small">Try again</button>        (optional)
 *     </div>
 *   </div>
 *
 * Text is set with textContent, never innerHTML.
 */

export const ERROR_CALLOUT_HINT = 'Try again; if the problem continues, contact your administrator.';

export interface ErrorCalloutSpec {
  /** The failure line, e.g. "Failed to load plans: internal error". */
  message: string;
  /** Re-runs the failed load. Without it the callout has no button. */
  onRetry?: () => void;
}

const SVG_NS = 'http://www.w3.org/2000/svg';

/** The callout's alert-circle icon (decorative), also used by init-error.ts. */
export function buildAlertIcon(): SVGSVGElement {
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('class', 'error-callout-icon');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('width', '20');
  svg.setAttribute('height', '20');
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', '2');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  svg.setAttribute('aria-hidden', 'true');
  svg.setAttribute('focusable', 'false');
  const circle = document.createElementNS(SVG_NS, 'circle');
  circle.setAttribute('cx', '12');
  circle.setAttribute('cy', '12');
  circle.setAttribute('r', '10');
  const bar = document.createElementNS(SVG_NS, 'line');
  bar.setAttribute('x1', '12');
  bar.setAttribute('y1', '8');
  bar.setAttribute('x2', '12');
  bar.setAttribute('y2', '12');
  const dot = document.createElementNS(SVG_NS, 'line');
  dot.setAttribute('x1', '12');
  dot.setAttribute('y1', '16');
  dot.setAttribute('x2', '12.01');
  dot.setAttribute('y2', '16');
  svg.append(circle, bar, dot);
  return svg;
}

export function buildErrorCallout(spec: ErrorCalloutSpec): HTMLDivElement {
  const callout = document.createElement('div');
  callout.className = 'error-callout';
  callout.setAttribute('role', 'alert');

  const body = document.createElement('div');
  body.className = 'error-callout-body';

  const message = document.createElement('p');
  message.className = 'error';
  message.textContent = spec.message;

  const hint = document.createElement('p');
  hint.className = 'error-callout-hint';
  hint.textContent = ERROR_CALLOUT_HINT;

  body.append(message, hint);

  const { onRetry } = spec;
  if (onRetry) {
    const retry = document.createElement('button');
    retry.type = 'button';
    retry.className = 'btn btn-small error-callout-retry';
    retry.textContent = 'Try again';
    retry.addEventListener('click', () => onRetry());
    body.appendChild(retry);
  }

  callout.append(buildAlertIcon(), body);
  return callout;
}

/** Replace `container`'s children with an error callout. */
export function renderErrorCallout(container: HTMLElement, spec: ErrorCalloutSpec): void {
  container.replaceChildren(buildErrorCallout(spec));
}
