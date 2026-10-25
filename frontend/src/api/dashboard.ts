/**
 * Dashboard API functions
 */

import { apiRequest } from './client';
import type { DashboardSummary, UpcomingPurchasesResponse, Provider } from './types';

/**
 * Get dashboard summary
 */
export async function getDashboardSummary(provider: Provider | '' = '', accountIDs: string[] = []): Promise<DashboardSummary> {
  const params = new URLSearchParams({ provider });
  if (accountIDs.length > 0) params.set('account_ids', accountIDs.join(','));
  return apiRequest<DashboardSummary>(`/dashboard/summary?${params}`);
}

/**
 * Get upcoming purchases. The endpoint returns `{ purchases: [...] }`, not a
 * bare array (#1189).
 */
export async function getUpcomingPurchases(): Promise<UpcomingPurchasesResponse> {
  return apiRequest<UpcomingPurchasesResponse>('/dashboard/upcoming');
}
