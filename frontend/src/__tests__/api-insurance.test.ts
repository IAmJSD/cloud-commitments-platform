import { getInsuranceComparison, getInsuranceStatus } from '../api/insurance';
import { apiRequest } from '../api/client';

jest.mock('../api/client', () => ({ apiRequest: jest.fn() }));

describe('insurance api', () => {
  beforeEach(() => jest.resetAllMocks());

  it('status is a plain GET with no query or body', async () => {
    await getInsuranceStatus();
    expect(apiRequest).toHaveBeenCalledWith('/insurance/status');
  });

  it('comparison is a plain GET with no query or body', async () => {
    await getInsuranceComparison();
    expect(apiRequest).toHaveBeenCalledWith('/insurance/comparison');
  });
});
