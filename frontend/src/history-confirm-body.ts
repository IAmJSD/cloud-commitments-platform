/**
 * Confirm-dialog body that names the purchase a History row action will
 * act on (issue #728, UX-29). Cancel and Retry used to say only "Cancel
 * this pending purchase?", so in a long table the user could not tell
 * which row they were confirming. The summary mirrors the row: provider,
 * service, resource and count, region, account, upfront cost and date.
 */

import type { HistoryPurchase } from './types';
import { getAccountName } from './recommendations';
import { formatCurrency, formatDate, formatTerm } from './utils';

/** One-line description of a History row, e.g. "AWS ec2 m5.large × 2 in us-east-1". */
export function describeHistoryPurchase(p: HistoryPurchase): string {
  const provider = p.provider && p.provider !== 'multiple' ? p.provider.toUpperCase() : '';
  const what = [provider, p.service, p.resource_type].filter(Boolean).join(' ');
  const count = p.count > 1 ? ` × ${p.count}` : '';
  const region = p.region ? ` in ${p.region}` : '';
  return `${what || 'Purchase'}${count}${region}`;
}

/**
 * Body element for a row-action confirm: a definition-style summary of the
 * purchase followed by the action's explanation.
 */
export function historyConfirmBody(p: HistoryPurchase | undefined, explanation: string): HTMLElement {
  const wrap = document.createElement('div');
  wrap.className = 'history-confirm-body';
  if (p) {
    const summary = document.createElement('p');
    summary.className = 'history-confirm-summary';
    const name = document.createElement('strong');
    name.textContent = describeHistoryPurchase(p);
    summary.appendChild(name);
    const facts: string[] = [];
    if (p.account_id) facts.push(`Account: ${getAccountName(p.account_id)}`);
    if (p.term) facts.push(`Term: ${formatTerm(p.term)}`);
    if (p.upfront_cost != null) facts.push(`Upfront: ${formatCurrency(p.upfront_cost)}`);
    const date = formatDate(p.timestamp);
    if (date) facts.push(`Created: ${date}`);
    if (facts.length > 0) {
      summary.appendChild(document.createElement('br'));
      summary.appendChild(document.createTextNode(facts.join(' · ')));
    }
    wrap.appendChild(summary);
  }
  const text = document.createElement('p');
  text.textContent = explanation;
  wrap.appendChild(text);
  return wrap;
}
