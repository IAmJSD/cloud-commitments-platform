/**
 * Issue #737: one status -> tone map and one label map for every badge.
 */
import { badgeClass, badgeHtml, statusBadgeHtml, statusLabel, statusTone } from '../lib/status-badge';

describe('statusTone', () => {
  test.each([
    ['completed', 'success'],
    ['approved', 'success'],
    ['active', 'success'],
    ['pending', 'warning'],
    ['notified', 'warning'],
    ['scheduled', 'warning'],
    ['running', 'warning'],
    ['processing', 'warning'],
    ['in_progress', 'warning'],
    ['partially_completed', 'warning'],
    ['failed', 'danger'],
    ['rejected', 'danger'],
    ['canceled', 'muted'],
    ['cancelled', 'muted'],
    ['expired', 'muted'],
    ['paused', 'muted'],
    ['disabled', 'muted'],
  ])('%s -> %s', (status, tone) => {
    expect(statusTone(status)).toBe(tone);
  });

  test('pending and running share one tone on every page', () => {
    // The bug: pending was blue in RI exchange and amber on Purchases; running
    // was orange in one table and blue in another.
    expect(statusTone('pending')).toBe(statusTone('notified'));
    expect(statusTone('running')).toBe(statusTone('processing'));
  });

  test('is case- and whitespace-insensitive', () => {
    expect(statusTone(' Completed ')).toBe('success');
    expect(statusTone('FAILED')).toBe('danger');
  });

  test('unknown, empty and prototype-key values are muted, never success', () => {
    expect(statusTone('something_new')).toBe('muted');
    expect(statusTone('')).toBe('muted');
    expect(statusTone(null)).toBe('muted');
    expect(statusTone(undefined)).toBe('muted');
    expect(statusTone('constructor')).toBe('muted');
  });
});

describe('statusLabel', () => {
  test.each([
    ['pending', 'Pending'],
    ['processing', 'Processing'],
    ['in_progress', 'In Progress'],
    ['partially_completed', 'Partial'],
    ['canceled', 'Cancelled'],
    ['cancelled', 'Cancelled'],
    ['PAUSED', 'Paused'],
    ['awaiting_quote', 'Awaiting quote'],
  ])('%s -> %s', (status, label) => {
    expect(statusLabel(status)).toBe(label);
  });

  test('empty status reads Unknown', () => {
    expect(statusLabel('')).toBe('Unknown');
    expect(statusLabel(null)).toBe('Unknown');
  });
});

describe('badge markup', () => {
  test('badgeClass builds the shared class list', () => {
    expect(badgeClass('info')).toBe('badge badge-info');
  });

  test('badgeHtml escapes the label and title', () => {
    const html = badgeHtml('danger', '<b>x</b>', 'a "quoted" title');
    expect(html).toBe('<span class="badge badge-danger" title="a &quot;quoted&quot; title">&lt;b&gt;x&lt;/b&gt;</span>');
  });

  test('statusBadgeHtml combines tone and label', () => {
    expect(statusBadgeHtml('in_progress')).toBe('<span class="badge badge-warning">In Progress</span>');
  });

  test('statusBadgeHtml neutralizes markup in an unknown status', () => {
    const host = document.createElement('div');
    host.innerHTML = statusBadgeHtml('<img src=x onerror=alert(1)>');
    expect(host.querySelector('img')).toBeNull();
    expect(host.querySelector('.badge')?.className).toBe('badge badge-muted');
  });
});
