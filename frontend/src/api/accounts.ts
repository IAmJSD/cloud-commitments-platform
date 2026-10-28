/**
 * Cloud Accounts API functions
 */

import { apiRequest, getApiBase, getAuthHeaders } from './client';
import { httpStatusMessage, makeApiError } from './errors';
import type { Provider } from './types';

export interface CloudAccount {
  id: string;
  name: string;
  description?: string;
  provider: Provider;
  external_id: string;
  contact_email?: string;
  enabled: boolean;
  aws_auth_mode?: 'access_keys' | 'role_arn' | 'bastion' | 'workload_identity_federation';
  aws_role_arn?: string;
  aws_external_id?: string;
  aws_bastion_id?: string;
  bastion_account_name?: string;
  aws_web_identity_token_file?: string;
  aws_is_org_root?: boolean;
  azure_auth_mode?: 'client_secret' | 'managed_identity' | 'workload_identity_federation';
  azure_subscription_id?: string;
  azure_tenant_id?: string;
  azure_client_id?: string;
  gcp_project_id?: string;
  gcp_client_email?: string;
  gcp_auth_mode?: 'service_account_key' | 'application_default' | 'workload_identity_federation';
  gcp_wif_audience?: string;
  credentials_configured: boolean;
  is_self?: boolean;
  created_at: string;
  updated_at: string;
}

export interface CloudAccountRequest {
  name: string;
  description?: string;
  contact_email?: string;
  provider: Provider;
  external_id: string;
  enabled?: boolean;
  aws_auth_mode?: string;
  aws_role_arn?: string;
  aws_external_id?: string;
  aws_bastion_id?: string;
  bastion_account_name?: string;
  aws_web_identity_token_file?: string;
  aws_is_org_root?: boolean;
  azure_auth_mode?: string;
  azure_subscription_id?: string;
  azure_tenant_id?: string;
  azure_client_id?: string;
  gcp_project_id?: string;
  gcp_client_email?: string;
  gcp_auth_mode?: string;
  gcp_wif_audience?: string;
}

export interface AccountListFilters {
  provider?: Provider;
  enabled?: boolean;
  search?: string;
}

export interface AccountCredentialsRequest {
  credential_type: 'aws_access_keys' | 'azure_client_secret' | 'gcp_service_account' | 'gcp_workload_identity_config';
  payload: Record<string, unknown>;
}

export interface AccountTestResult {
  ok: boolean;
  message: string;
}

export interface AccountServiceOverride {
  id: string;
  account_id: string;
  provider: string;
  service: string;
  enabled?: boolean;
  term?: number;
  payment?: string;
  coverage?: number;
  ramp_schedule?: string;
  include_engines?: string[];
  exclude_engines?: string[];
  include_regions?: string[];
  exclude_regions?: string[];
  include_types?: string[];
  exclude_types?: string[];
  // Row timestamps from account_service_overrides; the list endpoint returns
  // the full row. updated_at moves on every save (issue #8).
  created_at?: string;
  updated_at?: string;
}

export interface AccountServiceOverrideRequest {
  enabled?: boolean;
  term?: number;
  payment?: string;
  coverage?: number;
  ramp_schedule?: string;
  include_engines?: string[];
  exclude_engines?: string[];
  include_regions?: string[];
  exclude_regions?: string[];
  include_types?: string[];
  exclude_types?: string[];
}

/**
 * Minimal-disclosure account projection returned by GET /api/accounts/list.
 * Carries only the fields the global topbar filter and the create-plan target
 * prefill need; it deliberately omits every credential/config field so the
 * endpoint can be served to Standard / Read-Only users (gated on
 * view:recommendations rather than the admin-grade view:accounts). See issues
 * #949 / #951.
 */
export interface AccountSummary {
  id: string;
  name: string;
  external_id: string;
  provider: Provider;
}

/**
 * Fetch the minimal account list available to any user who can view
 * recommendations. Used by the global filter dropdown and the plan modal so
 * non-admin users get a populated account list without the 403 they'd hit on
 * the full GET /api/accounts endpoint.
 */
export async function listAccountsMinimal(filters?: AccountListFilters): Promise<AccountSummary[]> {
  const params = new URLSearchParams();
  if (filters?.provider) params.set('provider', filters.provider);
  if (filters?.search) params.set('search', filters.search);
  const qs = params.toString();
  return apiRequest<AccountSummary[]>(`/accounts/list${qs ? `?${qs}` : ''}`);
}

export async function listAccounts(filters?: AccountListFilters): Promise<CloudAccount[]> {
  const params = new URLSearchParams();
  if (filters?.provider) params.set('provider', filters.provider);
  if (filters?.enabled !== undefined) params.set('enabled', String(filters.enabled));
  if (filters?.search) params.set('search', filters.search);
  const qs = params.toString();
  return apiRequest<CloudAccount[]>(`/accounts${qs ? `?${qs}` : ''}`);
}

export async function createAccount(req: CloudAccountRequest): Promise<CloudAccount> {
  return apiRequest<CloudAccount>('/accounts', {
    method: 'POST',
    body: JSON.stringify(req)
  });
}

export async function createSelfAccount(): Promise<CloudAccount> {
  return apiRequest<CloudAccount>('/accounts/self', { method: 'POST' });
}

export async function getAccount(id: string): Promise<CloudAccount> {
  return apiRequest<CloudAccount>(`/accounts/${id}`);
}

export async function updateAccount(id: string, req: CloudAccountRequest): Promise<CloudAccount> {
  return apiRequest<CloudAccount>(`/accounts/${id}`, {
    method: 'PUT',
    body: JSON.stringify(req)
  });
}

export async function deleteAccount(id: string): Promise<void> {
  return apiRequest<void>(`/accounts/${id}`, { method: 'DELETE' });
}

export async function saveAccountCredentials(id: string, req: AccountCredentialsRequest): Promise<void> {
  return apiRequest<void>(`/accounts/${id}/credentials`, {
    method: 'POST',
    body: JSON.stringify(req)
  });
}

export async function testAccountCredentials(id: string): Promise<AccountTestResult> {
  return apiRequest<AccountTestResult>(`/accounts/${id}/test`, { method: 'POST' });
}

export async function listAccountServiceOverrides(id: string): Promise<AccountServiceOverride[]> {
  return apiRequest<AccountServiceOverride[]>(`/accounts/${id}/service-overrides`);
}

export async function saveAccountServiceOverride(
  accountId: string,
  provider: string,
  service: string,
  req: AccountServiceOverrideRequest
): Promise<AccountServiceOverride> {
  return apiRequest<AccountServiceOverride>(
    `/accounts/${accountId}/service-overrides/${provider}/${service}`,
    { method: 'PUT', body: JSON.stringify(req) }
  );
}

export async function deleteAccountServiceOverride(
  accountId: string,
  provider: string,
  service: string
): Promise<void> {
  return apiRequest<void>(
    `/accounts/${accountId}/service-overrides/${provider}/${service}`,
    { method: 'DELETE' }
  );
}

/** One override in an export/import file (GET/POST /api/accounts/service-overrides/*, issue #9). */
export interface OverrideTransferRow extends AccountServiceOverrideRequest {
  account_id?: string;
  account_name?: string;
  account_external_id?: string;
  provider: string;
  service: string;
  created_at?: string;
  updated_at?: string;
}

export interface OverrideExport {
  version: number;
  exported_at: string;
  overrides: OverrideTransferRow[];
}

export type OverrideImportMode = 'strict' | 'best_effort';

export interface OverrideImportRequest {
  mode: OverrideImportMode;
  dry_run: boolean;
  overrides?: OverrideTransferRow[];
  csv?: string;
}

export interface OverrideImportRowResult {
  row: number;
  account_id?: string;
  provider: string;
  service: string;
  action: 'insert' | 'update' | 'unchanged' | 'rejected';
  error?: string;
}

export interface OverrideImportReport {
  mode: OverrideImportMode;
  dry_run: boolean;
  applied: boolean;
  summary: Record<string, number>;
  rows: OverrideImportRowResult[];
}

export async function exportAccountServiceOverrides(): Promise<OverrideExport> {
  return apiRequest<OverrideExport>('/accounts/service-overrides/export');
}

/** The same export as CSV text. apiRequest parses JSON only, so this fetches directly. */
export async function exportAccountServiceOverridesCSV(): Promise<string> {
  const endpoint = '/accounts/service-overrides/export?format=csv';
  const response = await fetch(`${getApiBase()}${endpoint}`, { headers: getAuthHeaders() });
  if (!response.ok) {
    throw makeApiError(httpStatusMessage(response.status), response.status, `HTTP ${response.status} from ${endpoint}`);
  }
  return response.text();
}

/**
 * Import overrides. A strict import with a rejected row answers 400 with the
 * report under error.details.report.
 */
export async function importAccountServiceOverrides(req: OverrideImportRequest): Promise<OverrideImportReport> {
  return apiRequest<OverrideImportReport>('/accounts/service-overrides/import', {
    method: 'POST',
    body: JSON.stringify(req),
  });
}

export interface OrgDiscoveryResult {
  job_id?: string;
  status?: string;
  message?: string;
}

export async function discoverOrgAccounts(): Promise<OrgDiscoveryResult> {
  return apiRequest<OrgDiscoveryResult>('/accounts/discover-org', { method: 'POST' });
}

export interface FederationIaCResponse {
  filename: string;
  content: string;
  content_type: string;
  content_encoding?: string; // "base64" for binary content (zip bundles)
}

export async function getFederationIaC(
  target: string,
  source: string,
  format: string,
): Promise<FederationIaCResponse> {
  const params = new URLSearchParams({ target, source, format });
  return apiRequest<FederationIaCResponse>(`/federation/iac?${params.toString()}`);
}

export async function listPlanAccounts(planId: string): Promise<CloudAccount[]> {
  return apiRequest<CloudAccount[]>(`/plans/${planId}/accounts`);
}

export async function setPlanAccounts(planId: string, accountIds: string[]): Promise<void> {
  return apiRequest<void>(`/plans/${planId}/accounts`, {
    method: 'PUT',
    body: JSON.stringify({ account_ids: accountIds })
  });
}
