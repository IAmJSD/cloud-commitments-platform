/**
 * Request options for the Azure RI exchange API calls (issue #81).
 */
import { apiRequest } from '../api/client';
import { executeAzureExchange, getAzureCompatibleOfferings } from '../api/riexchange';

jest.mock('../api/client', () => ({ apiRequest: jest.fn() }));

const req = {
  subscription_id: 'sub',
  sources: [{ reservation_id: '/r/one', quantity: 1 }],
  targets: [{ sku: 'Standard_D4s_v5', location: 'eastus', term: 'P3Y', quantity: 1 }],
};

beforeEach(() => (apiRequest as jest.Mock).mockReset().mockResolvedValue({}));

it('gives execute a client timeout longer than the backend 4-minute execution budget', async () => {
  await executeAzureExchange({ ...req, max_payment_due: '150', currency: 'USD' });
  expect(apiRequest).toHaveBeenCalledWith('/ri-exchange/azure-instances/exchange', {
    method: 'POST',
    body: JSON.stringify({ ...req, max_payment_due: '150', currency: 'USD' }),
    timeoutMs: 5 * 60_000,
  });
});

it('keeps the default timeout for preview', async () => {
  await getAzureCompatibleOfferings(req);
  const opts = (apiRequest as jest.Mock).mock.calls[0]![1];
  expect(opts).not.toHaveProperty('timeoutMs');
});
