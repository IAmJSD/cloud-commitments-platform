import { listAccountsMinimal, type AccountSummary } from './api/accounts';
import type { LadderConfig } from './api/ladder';

type AccountIdentity = Pick<LadderConfig, 'cloud_account_id' | 'provider'>;

export function initLadderAccountSelector(form: HTMLElement, existing?: LadderConfig): () => AccountIdentity | null {
  const account = form.querySelector<HTMLSelectElement>('#ladder-cfg-account')!;
  const provider = form.querySelector<HTMLSelectElement>('#ladder-cfg-provider')!;
  const status = form.querySelector<HTMLElement>('#ladder-cfg-account-status')!;
  const save = form.querySelector<HTMLButtonElement>('#ladder-config-save-btn')!;
  const identity = existing ? { cloud_account_id: existing.cloud_account_id, provider: existing.provider } : null;
  provider.disabled = !!identity;
  let accounts: AccountSummary[] | undefined;
  let loadFailed = false;
  const selection = (): AccountIdentity | null => {
    if (identity) return identity;
    const selected = accounts?.find(item => item.id === account.value && item.provider === provider.value);
    return selected ? { cloud_account_id: selected.id, provider: selected.provider } : null;
  };
  const render = (): void => {
    account.replaceChildren();
    if (identity) {
      const selected = accounts?.find(item => item.id === identity.cloud_account_id && item.provider === identity.provider);
      account.add(new Option(selected ? `${selected.name} (${selected.external_id})` : identity.cloud_account_id, identity.cloud_account_id));
      account.disabled = true;
      provider.disabled = true;
      status.textContent = selected ? 'Account and provider cannot be changed for an existing configuration.'
        : !accounts && !loadFailed ? 'Loading registered account details. Saved identity is unchanged.'
        : loadFailed ? 'Could not load registered accounts. Saved identity is unchanged.'
        : 'Saved account is unavailable in the registered account list. Saved identity is unchanged.';
      save.disabled = false;
      return;
    }
    const available = accounts?.filter(item => item.provider === provider.value) ?? [];
    account.add(new Option('Select a registered account', ''));
    for (const item of available) account.add(new Option(`${item.name} (${item.external_id})`, item.id));
    account.disabled = available.length === 0;
    save.disabled = true;
    status.textContent = loadFailed ? 'Could not load registered accounts. Close and reopen to retry.'
      : !accounts ? 'Loading registered accounts...'
      : available.length === 0 ? 'No registered accounts are available for this provider.'
      : 'Choose a registered account for this provider.';
  };
  provider.addEventListener('change', render);
  account.addEventListener('change', () => { save.disabled = !selection(); });
  render();
  void listAccountsMinimal().then(loaded => {
    accounts = loaded;
    render();
  }).catch(() => {
    loadFailed = true;
    render();
  });
  return selection;
}
