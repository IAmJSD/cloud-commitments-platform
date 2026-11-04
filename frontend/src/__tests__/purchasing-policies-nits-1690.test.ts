/**
 * Issue #1690: Admin > Purchasing policies nits.
 *  - the Planned purchases From (UTC) prefill matches the documented
 *    YYYY-MM-DD HH:MM format (no seconds);
 *  - an empty timeline shows one empty-state message, not three;
 *  - the page Save/Reset row sits above the Laddering section, which saves
 *    itself and is not covered by it.
 */
import fs from 'fs';
import path from 'path';

jest.mock('chart.js', () => ({ Chart: Object.assign(jest.fn().mockImplementation(() => ({ destroy: jest.fn(), update: jest.fn() })), { register: jest.fn() }), registerables: [] }));
jest.mock('../api/accounts', () => ({ listAccountsMinimal: jest.fn() }));
jest.mock('../api/ladder', () => ({ getLadderEvents: jest.fn(), getLadderRuns: jest.fn(), amendLadderEvent: jest.fn() }));
jest.mock('../permissions', () => ({ canAccess: jest.fn(() => true) }));

import { listAccountsMinimal } from '../api/accounts';
import { getLadderEvents, getLadderRuns } from '../api/ladder';
import { initLadderTimeline, disposeLadderTimeline } from '../ladder-timeline';

const text = (selector: string): string => document.querySelector(selector)?.textContent?.trim() ?? '';

describe('Planned purchases timeline (issue #1690)', () => {
  beforeEach(() => {
    document.body.innerHTML = '<section id="timeline"></section>';
    jest.mocked(listAccountsMinimal).mockResolvedValue([{ id: 'account', name: 'Account', external_id: '123', provider: 'aws' }]);
    jest.mocked(getLadderRuns).mockResolvedValue([]);
  });

  afterEach(() => {
    disposeLadderTimeline();
    jest.useRealTimers();
  });

  test('the From prefill is the current UTC minute, without seconds', async () => {
    jest.useFakeTimers({ doNotFake: ['nextTick', 'setImmediate', 'queueMicrotask'] });
    jest.setSystemTime(new Date('2026-11-04T09:56:36.512Z'));
    jest.mocked(getLadderEvents).mockResolvedValue([]);

    await initLadderTimeline(document.getElementById('timeline')!, [], false);

    expect(document.querySelector<HTMLInputElement>('[data-date="from"]')!.value).toBe('2026-11-04 09:56');
  });

  test('with no tranches only the table states it; no "0 of 0" summary or run hint', async () => {
    jest.mocked(getLadderEvents).mockResolvedValue([]);

    await initLadderTimeline(document.getElementById('timeline')!, [], false);

    expect(text('[data-table]')).toContain('No planned purchase tranches.');
    expect(text('[data-summary]')).toBe('');
    expect(text('[data-snapshot]')).toBe('');
  });
});

describe('Purchasing policies layout (issue #1690)', () => {
  const html = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf8');

  test('the Save/Reset row comes before the self-saving Laddering section', () => {
    document.body.innerHTML = html.match(/<body[^>]*>([\s\S]*?)<\/body>/i)?.[1] ?? '';
    const buttons = document.getElementById('save-purchasing-btn')!.closest('.settings-buttons')!;
    const laddering = document.getElementById('commitment-laddering-settings')!;
    const exchange = document.getElementById('ri-exchange-automation-settings')!;

    // Exchange automation is saved by this button, so it stays above it.
    expect(exchange.compareDocumentPosition(buttons) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(buttons.compareDocumentPosition(laddering) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});
