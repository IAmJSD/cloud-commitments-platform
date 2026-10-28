/**
 * Wire format for GET /api/analytics/trends (issue #30). The handler and
 * its scope enforcement are covered by handler_analytics tests on the Go side.
 */

import { apiRequest } from '../api/client';
import { getAnalyticsTrends } from '../api/analytics';

jest.mock('../api/client', () => ({
  apiRequest: jest.fn(),
}));

describe('getAnalyticsTrends', () => {
  beforeEach(() => {
    (apiRequest as jest.Mock).mockReset().mockResolvedValue({ monthly: [] });
  });

  test('calls the bare endpoint when no filter is set', async () => {
    await getAnalyticsTrends();
    expect(apiRequest).toHaveBeenCalledWith('/analytics/trends');
  });

  test('forwards account_ids, provider and the window, URL-encoded', async () => {
    await getAnalyticsTrends({
      accountIDs: ['acc/1 x', 'acc2'],
      provider: 'aws',
      start: '2026-05-01T00:00:00.000Z',
      end: '2026-10-23T12:00:00.000Z',
    });
    const url = (apiRequest as jest.Mock).mock.calls[0][0] as string;
    const qs = new URLSearchParams(url.split('?')[1]);
    expect(url.startsWith('/analytics/trends?')).toBe(true);
    expect(qs.get('account_ids')).toBe('acc/1 x,acc2');
    expect(qs.has('account_id')).toBe(false);
    expect(qs.get('provider')).toBe('aws');
    expect(qs.get('start')).toBe('2026-05-01T00:00:00.000Z');
    expect(qs.get('end')).toBe('2026-10-23T12:00:00.000Z');
  });

  test('omits empty filter values', async () => {
    await getAnalyticsTrends({ accountIDs: [], provider: '' });
    expect(apiRequest).toHaveBeenCalledWith('/analytics/trends');
  });
});
