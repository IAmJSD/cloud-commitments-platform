/**
 * Archera comparison API functions (read-only, explicit user action).
 */

import { apiRequest } from './client';
import type { ArcheraComparison, InsuranceStatus } from './types';

/** Reports whether the comparison is configured. Makes no vendor call. */
export async function getInsuranceStatus(): Promise<InsuranceStatus> {
  return apiRequest<InsuranceStatus>('/insurance/status');
}

/** Fetches the Archera comparison now. One vendor request per call, no caching. */
export async function getInsuranceComparison(): Promise<ArcheraComparison> {
  return apiRequest<ArcheraComparison>('/insurance/comparison');
}
