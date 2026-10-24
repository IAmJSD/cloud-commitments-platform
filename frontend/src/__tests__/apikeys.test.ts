/**
 * API Keys module tests
 */
import {
  loadApiKeys,
  renderApiKeysList,
  showCreateKeyModal,
  closeCreateKeyModal,
  createApiKey,
  handleCreateApiKey,
  showKeyCreatedModal,
  revokeApiKey,
  deleteApiKey,
  initApiKeys
} from '../apikeys';

// Mock the api module
jest.mock('../api', () => ({
  getApiKeys: jest.fn(),
  getApiKeysUsageStats: jest.fn(),
  createApiKey: jest.fn(),
  revokeApiKey: jest.fn(),
  deleteApiKey: jest.fn(),
  getUserPermissions: jest.fn(),
  // Real implementation (not mocked): createApiKey encodes the password
  // field with this before building the request, so tests need the real
  // encoding to assert on the resulting request shape.
  base64Encode: (str: string) => Buffer.from(str, 'binary').toString('base64')
}));

// confirmDialog (introduced in P2) is used for revoke/delete in place of
// native window.confirm. Mock it so tests can flip approve/cancel via
// mockResolvedValueOnce without having to drive the actual modal UI.
const mockConfirmDialog = jest.fn<Promise<boolean>, [unknown]>(() => Promise.resolve(true));
jest.mock('../confirmDialog', () => ({
  confirmDialog: (opts: unknown) => mockConfirmDialog(opts),
}));

import * as api from '../api';

describe('API Keys Module', () => {
  beforeEach(() => {
    // Reset DOM
    document.body.innerHTML = `
      <div id="apikeys-usage-summary"></div>
      <div id="apikeys-list"></div>
      <button id="create-apikey-btn"></button>
      <div id="create-apikey-modal" class="hidden">
        <form id="create-apikey-form">
          <input type="text" id="apikey-name" value="">
          <input type="password" id="apikey-password" value="">
          <div id="apikey-permissions-list">
            <label class="apikey-permission-option">
              <input type="checkbox" class="apikey-permission-checkbox" id="apikey-perm-0"
                     data-action="view" data-resource="recommendations" checked>
              view:recommendations
            </label>
          </div>
          <input type="date" id="apikey-expires-at" value="">
          <div id="create-apikey-error" class="hidden"></div>
          <button type="submit" id="create-apikey-submit-btn">Create</button>
        </form>
        <button id="close-create-apikey-modal-btn"></button>
      </div>
    `;

    jest.clearAllMocks();
    window.alert = jest.fn();
    window.confirm = jest.fn().mockReturnValue(true);
    // Reset the confirmDialog mock between tests — clearAllMocks only
    // clears call history, not the mockResolvedValueOnce queue.
    mockConfirmDialog.mockReset();
    mockConfirmDialog.mockImplementation(() => Promise.resolve(true));
    // Default the usage-stats endpoint to an empty response so the
    // parallel-load path in loadApiKeys doesn't reject in tests that
    // only set up `getApiKeys`. Individual tests can override.
    (api.getApiKeysUsageStats as jest.Mock).mockResolvedValue({
      total_active: 0,
      total_requests_window: 0,
      total_requests_lifetime: 0,
      top_keys: [],
    });
    // Default the owner's effective-permissions fetch (issue #61's
    // frontend follow-up) so showCreateKeyModal tests that don't care
    // about the picker's own behavior aren't forced to stub it too.
    (api.getUserPermissions as jest.Mock).mockResolvedValue({
      permissions: [{ action: 'view', resource: 'recommendations' }],
      is_admin: false,
    });
  });

  describe('loadApiKeys', () => {
    test('loads and renders API keys on success', async () => {
      const mockKeys = [
        {
          id: 'key-1',
          name: 'Test Key 1',
          key_prefix: 'abc123',
          is_active: true,
          created_at: '2024-01-15T10:00:00Z',
          last_used_at: '2024-01-16T15:30:00Z'
        },
        {
          id: 'key-2',
          name: 'Test Key 2',
          key_prefix: 'xyz789',
          is_active: false,
          created_at: '2024-01-10T08:00:00Z',
          expires_at: '2024-02-10T08:00:00Z'
        }
      ];

      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: mockKeys });

      await loadApiKeys();

      expect(api.getApiKeys).toHaveBeenCalled();
      const container = document.getElementById('apikeys-list');
      expect(container?.innerHTML).toContain('Test Key 1');
      expect(container?.innerHTML).toContain('Test Key 2');
    });

    test('shows error on API failure', async () => {
      // Remove the create-apikey-error element so showError falls back to alert
      document.getElementById('create-apikey-error')?.remove();

      const consoleError = jest.spyOn(console, 'error').mockImplementation(() => {});
      (api.getApiKeys as jest.Mock).mockRejectedValue(new Error('API Error'));

      await loadApiKeys();

      expect(consoleError).toHaveBeenCalledWith('Failed to load API keys:', expect.any(Error));
      // Q4: fallback when #create-apikey-error is absent now surfaces
      // via toast rather than blocking alert().
      expect(document.querySelector('.toast-error')?.querySelector('.toast-message')?.textContent).toBe('Failed to load API keys');
      consoleError.mockRestore();
    });

    // Regression: GitHub issue #9. Backend returns `{api_keys: [...]}`
    // (auth.APIListAPIKeysResponse with `json:"api_keys"`); the previous
    // frontend read `response.keys`, leaving currentApiKeys = undefined
    // and crashing renderApiKeysList with TypeError. Lock in the
    // contract so a future field rename has to update tests too.
    test('issue #9: parses api_keys field from backend response', async () => {
      const mockKeys = [{
        id: 'key-1', name: 'Test', key_prefix: 'abc',
        is_active: true, created_at: '2024-01-15T10:00:00Z',
      }];
      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: mockKeys });

      const consoleError = jest.spyOn(console, 'error').mockImplementation(() => {});
      await loadApiKeys();

      expect(document.getElementById('apikeys-list')?.innerHTML).toContain('Test');
      expect(consoleError).not.toHaveBeenCalled();
      consoleError.mockRestore();
    });

    test('issue #9: tolerates legacy bare-array response without crashing', async () => {
      // Defence-in-depth — if a proxy or alternate handler returns the
      // bare array (no envelope), we should still render the rows
      // rather than crashing on currentApiKeys.length.
      const mockKeys = [{
        id: 'key-2', name: 'Bare', key_prefix: 'def',
        is_active: true, created_at: '2024-01-15T10:00:00Z',
      }];
      (api.getApiKeys as jest.Mock).mockResolvedValue(mockKeys);

      const consoleError = jest.spyOn(console, 'error').mockImplementation(() => {});
      await expect(loadApiKeys()).resolves.toBeUndefined();
      expect(document.getElementById('apikeys-list')?.innerHTML).toContain('Bare');
      expect(consoleError).not.toHaveBeenCalled();
      consoleError.mockRestore();
    });

    test('issue #9: renders empty state on unexpected response shape', async () => {
      // The exact failure mode from the bug report: response has
      // neither `api_keys` nor is an array. Old code crashed with
      // TypeError; new code should render the empty state and not log
      // an error.
      (api.getApiKeys as jest.Mock).mockResolvedValue({ unexpected: 'shape' });

      const consoleError = jest.spyOn(console, 'error').mockImplementation(() => {});
      await expect(loadApiKeys()).resolves.toBeUndefined();
      expect(document.getElementById('apikeys-list')?.textContent).toContain('No API keys yet');
      expect(consoleError).not.toHaveBeenCalled();
      consoleError.mockRestore();
    });
  });

  describe('renderApiKeysList', () => {
    test('shows empty message when no keys', () => {
      // First load empty keys
      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: [] });

      // Call renderApiKeysList (it uses internal state, so we need to load first)
      loadApiKeys().then(() => {
        const container = document.getElementById('apikeys-list');
        expect(container?.innerHTML).toContain('No API keys yet');
      });
    });

    test('handles missing container gracefully', () => {
      document.body.innerHTML = '';

      // Should not throw
      expect(() => renderApiKeysList()).not.toThrow();
    });

    test('renders active keys with revoke button', async () => {
      const mockKeys = [
        {
          id: 'key-1',
          name: 'Active Key',
          key_prefix: 'abc123',
          is_active: true,
          created_at: '2024-01-15T10:00:00Z'
        }
      ];

      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: mockKeys });

      await loadApiKeys();

      const container = document.getElementById('apikeys-list');
      expect(container?.innerHTML).toContain('revoke-key-btn');
      expect(container?.innerHTML).toContain('Active');
    });

    test('renders revoked keys without revoke button', async () => {
      const mockKeys = [
        {
          id: 'key-1',
          name: 'Revoked Key',
          key_prefix: 'abc123',
          is_active: false,
          created_at: '2024-01-15T10:00:00Z'
        }
      ];

      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: mockKeys });

      await loadApiKeys();

      const container = document.getElementById('apikeys-list');
      expect(container?.innerHTML).toContain('Revoked');
      // Revoked keys should not have revoke button, but should have delete
      expect(container?.innerHTML).toContain('delete-key-btn');
    });

    test('renders expired keys with warning badge', async () => {
      const pastDate = new Date(Date.now() - 24 * 60 * 60 * 1000).toISOString();
      const mockKeys = [
        {
          id: 'key-1',
          name: 'Expired Key',
          key_prefix: 'abc123',
          is_active: true,
          created_at: '2024-01-15T10:00:00Z',
          expires_at: pastDate
        }
      ];

      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: mockKeys });

      await loadApiKeys();

      const container = document.getElementById('apikeys-list');
      expect(container?.innerHTML).toContain('Expired');
      expect(container?.innerHTML).toContain('badge-warning');
    });

    test('renders never used and never expires labels', async () => {
      const mockKeys = [
        {
          id: 'key-1',
          name: 'New Key',
          key_prefix: 'abc123',
          is_active: true,
          created_at: '2024-01-15T10:00:00Z'
          // No last_used_at or expires_at
        }
      ];

      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: mockKeys });

      await loadApiKeys();

      const container = document.getElementById('apikeys-list');
      expect(container?.innerHTML).toContain('Never');
    });

    test('adds event listeners to revoke and delete buttons', async () => {
      const mockKeys = [
        {
          id: 'key-1',
          name: 'Test Key',
          key_prefix: 'abc123',
          is_active: true,
          created_at: '2024-01-15T10:00:00Z'
        }
      ];

      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: mockKeys });
      (api.revokeApiKey as jest.Mock).mockResolvedValue({});

      await loadApiKeys();

      const revokeBtn = document.querySelector('.revoke-key-btn') as HTMLButtonElement;
      expect(revokeBtn).not.toBeNull();

      // Click the revoke button
      revokeBtn.click();

      await new Promise(resolve => setTimeout(resolve, 0));

      expect(api.revokeApiKey).toHaveBeenCalledWith('key-1');
    });

    // Regression for ARCH-07 (issue #1195): the previous local escapeHtml
    // copy (div.textContent -> div.innerHTML round-trip) did not escape
    // quote characters, so a key id containing a double quote interpolated
    // into data-key-id="..." broke out of the attribute and injected a new
    // attribute (here onmouseover) into the button markup.
    test('issue #1195: double quote in key id cannot inject attributes', async () => {
      const hostileId = 'key-1" onmouseover="alert(1)';
      const mockKeys = [
        {
          id: hostileId,
          name: 'Hostile Key',
          key_prefix: 'abc123',
          is_active: true,
          created_at: '2024-01-15T10:00:00Z'
        }
      ];

      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: mockKeys });

      await loadApiKeys();

      const deleteBtn = document.querySelector('.delete-key-btn');
      expect(deleteBtn).not.toBeNull();
      expect(deleteBtn?.getAttribute('onmouseover')).toBeNull();
      expect(deleteBtn?.getAttribute('data-key-id')).toBe(hostileId);
    });

    test('adds event listener to delete buttons', async () => {
      const mockKeys = [
        {
          id: 'key-1',
          name: 'Test Key',
          key_prefix: 'abc123',
          is_active: true,
          created_at: '2024-01-15T10:00:00Z'
        }
      ];

      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: mockKeys });
      (api.deleteApiKey as jest.Mock).mockResolvedValue({});

      await loadApiKeys();

      const deleteBtn = document.querySelector('.delete-key-btn') as HTMLButtonElement;
      expect(deleteBtn).not.toBeNull();

      // Click the delete button
      deleteBtn.click();

      await new Promise(resolve => setTimeout(resolve, 0));

      expect(api.deleteApiKey).toHaveBeenCalledWith('key-1');
    });
  });

  describe('showCreateKeyModal', () => {
    test('shows modal and resets form', () => {
      const modal = document.getElementById('create-apikey-modal');
      const nameInput = document.getElementById('apikey-name') as HTMLInputElement;
      nameInput.value = 'Previous Value';

      showCreateKeyModal();

      expect(modal?.classList.contains('hidden')).toBe(false);
      expect(nameInput.value).toBe('');
    });

    test('hides error element', () => {
      const errorEl = document.getElementById('create-apikey-error');
      errorEl?.classList.remove('hidden');

      showCreateKeyModal();

      expect(errorEl?.classList.contains('hidden')).toBe(true);
    });

    test('prefills expiration date 90 days out', () => {
      const expiresAtInput = document.getElementById('apikey-expires-at') as HTMLInputElement;
      expiresAtInput.value = '';

      showCreateKeyModal();

      // Compare local calendar components, not a UTC round-trip: the field
      // is filled from local date parts (issue #102 CodeRabbit finding), so
      // asserting via toISOString would flake near local midnight in
      // timezones ahead of or behind UTC.
      const expected = new Date();
      expected.setDate(expected.getDate() + 90);
      const expectedValue = [
        expected.getFullYear(),
        String(expected.getMonth() + 1).padStart(2, '0'),
        String(expected.getDate()).padStart(2, '0'),
      ].join('-');
      expect(expiresAtInput.value).toBe(expectedValue);
    });

    test('handles missing modal gracefully', () => {
      document.body.innerHTML = '';

      // Should not throw
      expect(() => showCreateKeyModal()).not.toThrow();
    });

    // Issue #61 frontend follow-up: the create-key modal now fetches the
    // caller's own effective permissions and renders one checkbox per
    // entry, since the backend rejects an empty permissions array.
    describe('permission picker (issue #61)', () => {
      test('fetches and renders the owner\'s effective permissions as unchecked checkboxes', async () => {
        (api.getUserPermissions as jest.Mock).mockResolvedValue({
          permissions: [
            { action: 'view', resource: 'recommendations' },
            { action: 'create', resource: 'plans' },
          ],
          is_admin: false,
        });

        await showCreateKeyModal();

        expect(api.getUserPermissions).toHaveBeenCalled();
        const container = document.getElementById('apikey-permissions-list');
        const checkboxes = container?.querySelectorAll<HTMLInputElement>('.apikey-permission-checkbox');
        expect(checkboxes?.length).toBe(2);
        checkboxes?.forEach(cb => expect(cb.checked).toBe(false));
        expect(container?.textContent).toContain('view:recommendations');
        expect(container?.textContent).toContain('create:plans');
      });

      test('labels the admin wildcard entry in plain language', async () => {
        (api.getUserPermissions as jest.Mock).mockResolvedValue({
          permissions: [{ action: 'admin', resource: '*' }],
          is_admin: true,
        });

        await showCreateKeyModal();

        const container = document.getElementById('apikey-permissions-list');
        expect(container?.textContent).toContain('Admin (all actions, all resources)');
      });

      test('escapes hostile action/resource values instead of injecting markup', async () => {
        const hostile = '"><img src=x onerror=alert(1)>';
        (api.getUserPermissions as jest.Mock).mockResolvedValue({
          permissions: [{ action: hostile, resource: 'plans' }],
          is_admin: false,
        });

        await showCreateKeyModal();

        const container = document.getElementById('apikey-permissions-list');
        // No element was injected -- the quote-breakout attempt stayed
        // inside the data-action attribute's value rather than closing it
        // and opening a real <img> tag.
        expect(container?.querySelector('img')).toBeNull();
        // The checkbox's dataset round-trips the exact hostile string,
        // proving escapeHtml neutralized it as data rather than mangling
        // or truncating it.
        const checkbox = container?.querySelector<HTMLInputElement>('.apikey-permission-checkbox');
        expect(checkbox?.dataset['action']).toBe(hostile);
        // The visible label text is HTML-escaped, not raw markup.
        expect(container?.textContent).toContain(`${hostile}:plans`);
      });

      test('shows a message and no checkboxes when the owner holds no permissions', async () => {
        (api.getUserPermissions as jest.Mock).mockResolvedValue({ permissions: [], is_admin: false });

        await showCreateKeyModal();

        const container = document.getElementById('apikey-permissions-list');
        expect(container?.querySelectorAll('.apikey-permission-checkbox').length).toBe(0);
        expect(container?.textContent).toContain('No permissions available');
      });

      test('shows an inline error when the permissions fetch fails', async () => {
        const consoleError = jest.spyOn(console, 'error').mockImplementation(() => {});
        (api.getUserPermissions as jest.Mock).mockRejectedValue(new Error('boom'));

        await showCreateKeyModal();

        const container = document.getElementById('apikey-permissions-list');
        expect(container?.querySelector('.error')?.textContent).toContain('Failed to load your permissions');
        consoleError.mockRestore();
      });

      test('Create button starts disabled and enables only once a permission is checked', async () => {
        (api.getUserPermissions as jest.Mock).mockResolvedValue({
          permissions: [{ action: 'view', resource: 'recommendations' }],
          is_admin: false,
        });

        await showCreateKeyModal();

        const submitBtn = document.getElementById('create-apikey-submit-btn') as HTMLButtonElement;
        expect(submitBtn.disabled).toBe(true);

        const checkbox = document.querySelector('.apikey-permission-checkbox') as HTMLInputElement;
        checkbox.checked = true;
        checkbox.dispatchEvent(new Event('change'));
        expect(submitBtn.disabled).toBe(false);

        checkbox.checked = false;
        checkbox.dispatchEvent(new Event('change'));
        expect(submitBtn.disabled).toBe(true);
      });
    });
  });

  describe('closeCreateKeyModal', () => {
    test('hides the modal', () => {
      const modal = document.getElementById('create-apikey-modal');
      modal?.classList.remove('hidden');

      closeCreateKeyModal();

      expect(modal?.classList.contains('hidden')).toBe(true);
    });

    test('handles missing modal gracefully', () => {
      document.body.innerHTML = '';

      // Should not throw
      expect(() => closeCreateKeyModal()).not.toThrow();
    });
  });

  describe('createApiKey', () => {
    const expiresAt = new Date('2025-12-31T00:00:00Z');
    // permissions is required (issue #61): the backend rejects an empty
    // array with 400, so every real caller (handleCreateApiKey) always has
    // a non-empty selection by the time it calls this wrapper.
    const permissions = [{ action: 'view', resource: 'recommendations' }];

    test('creates API key with name, password, expiration and permissions', async () => {
      const mockResponse = {
        api_key: 'full-api-key-value',
        key_id: 'key-1',
        key: { id: 'key-1', name: 'Test Key', key_prefix: 'abc' }
      };

      (api.createApiKey as jest.Mock).mockResolvedValue(mockResponse);

      const result = await createApiKey('Test Key', 'my-password', expiresAt, permissions);

      expect(api.createApiKey).toHaveBeenCalledWith({
        name: 'Test Key',
        password: Buffer.from('my-password', 'binary').toString('base64'),
        expires_at: expiresAt.toISOString(),
        permissions,
      });
      expect(result.api_key).toBe('full-api-key-value');
    });

    test('sends every permission passed in', async () => {
      const mockResponse = {
        api_key: 'full-api-key-value',
        key_id: 'key-1',
        key: { id: 'key-1', name: 'Test Key', key_prefix: 'abc' }
      };

      (api.createApiKey as jest.Mock).mockResolvedValue(mockResponse);

      const multiplePermissions = [
        { action: 'view', resource: 'recommendations' },
        { action: 'create', resource: 'plans' },
      ];
      await createApiKey('Test Key', 'my-password', expiresAt, multiplePermissions);

      expect(api.createApiKey).toHaveBeenCalledWith({
        name: 'Test Key',
        password: Buffer.from('my-password', 'binary').toString('base64'),
        expires_at: expiresAt.toISOString(),
        permissions: multiplePermissions
      });
    });

    test('throws error on API failure', async () => {
      const consoleError = jest.spyOn(console, 'error').mockImplementation(() => {});
      (api.createApiKey as jest.Mock).mockRejectedValue(new Error('Create failed'));

      await expect(createApiKey('Test Key', 'my-password', expiresAt, permissions)).rejects.toThrow('Create failed');
      expect(consoleError).toHaveBeenCalled();
      consoleError.mockRestore();
    });
  });

  describe('handleCreateApiKey', () => {
    beforeEach(() => {
      document.getElementById('create-apikey-modal')?.classList.remove('hidden');
    });

    test('prevents default form submission', async () => {
      (api.createApiKey as jest.Mock).mockResolvedValue({
        api_key: 'test-key',
        key_id: 'key-1',
        key: { id: 'key-1', name: 'Test', key_prefix: 'abc' }
      });
      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: [] });

      (document.getElementById('apikey-name') as HTMLInputElement).value = 'Test Key';

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await handleCreateApiKey(event);

      expect(event.preventDefault).toHaveBeenCalled();
    });

    test('shows error when name is empty', async () => {
      const event = { preventDefault: jest.fn() } as unknown as Event;
      await handleCreateApiKey(event);

      const errorEl = document.getElementById('create-apikey-error');
      expect(errorEl?.classList.contains('hidden')).toBe(false);
      expect(errorEl?.textContent).toBe('API key name is required');
    });

    // Regression coverage for issue #102: the form must not let a key be
    // created with no password re-verification.
    test('shows error when password is empty', async () => {
      const futureDate = new Date(Date.now() + 30 * 24 * 60 * 60 * 1000);
      (document.getElementById('apikey-name') as HTMLInputElement).value = 'Test Key';
      (document.getElementById('apikey-expires-at') as HTMLInputElement).value = futureDate.toISOString().split('T')[0] || "";

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await handleCreateApiKey(event);

      const errorEl = document.getElementById('create-apikey-error');
      expect(errorEl?.textContent).toBe('Your password is required to create an API key');
      expect(api.createApiKey).not.toHaveBeenCalled();
    });

    // Regression coverage for issue #102: the form must not let a key be
    // created with no expiration.
    test('shows error when expiration date is empty', async () => {
      (document.getElementById('apikey-name') as HTMLInputElement).value = 'Test Key';
      (document.getElementById('apikey-password') as HTMLInputElement).value = 'my-password';
      (document.getElementById('apikey-expires-at') as HTMLInputElement).value = '';

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await handleCreateApiKey(event);

      const errorEl = document.getElementById('create-apikey-error');
      expect(errorEl?.textContent).toBe('An expiration date is required');
      expect(api.createApiKey).not.toHaveBeenCalled();
    });

    test('shows error when expiration date is in the past', async () => {
      const pastDate = new Date(Date.now() - 24 * 60 * 60 * 1000);

      (document.getElementById('apikey-name') as HTMLInputElement).value = 'Test Key';
      (document.getElementById('apikey-password') as HTMLInputElement).value = 'my-password';
      (document.getElementById('apikey-expires-at') as HTMLInputElement).value = pastDate.toISOString().split('T')[0] || "";

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await handleCreateApiKey(event);

      const errorEl = document.getElementById('create-apikey-error');
      expect(errorEl?.textContent).toBe('Expiration date must be in the future');
    });

    test('creates key and shows success modal', async () => {
      const futureDate = new Date(Date.now() + 30 * 24 * 60 * 60 * 1000);
      const mockResponse = {
        api_key: 'new-api-key-12345',
        key_id: 'key-1',
        key: { id: 'key-1', name: 'Test Key', key_prefix: 'new' }
      };

      (api.createApiKey as jest.Mock).mockResolvedValue(mockResponse);
      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: [] });

      (document.getElementById('apikey-name') as HTMLInputElement).value = 'Test Key';
      (document.getElementById('apikey-password') as HTMLInputElement).value = 'my-password';
      (document.getElementById('apikey-expires-at') as HTMLInputElement).value = futureDate.toISOString().split('T')[0] || "";

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await handleCreateApiKey(event);

      expect(api.createApiKey).toHaveBeenCalledWith(
        expect.objectContaining({
          name: 'Test Key',
          password: expect.any(String),
          expires_at: expect.any(String)
        })
      );

      // Check that the key created modal is shown
      const createdModal = document.getElementById('apikey-created-modal');
      expect(createdModal).not.toBeNull();
      expect(createdModal?.innerHTML).toContain('new-api-key-12345');
    });

    test('shows error on API failure', async () => {
      const futureDate = new Date(Date.now() + 30 * 24 * 60 * 60 * 1000);
      (api.createApiKey as jest.Mock).mockRejectedValue(new Error('Create failed'));

      (document.getElementById('apikey-name') as HTMLInputElement).value = 'Test Key';
      (document.getElementById('apikey-password') as HTMLInputElement).value = 'my-password';
      (document.getElementById('apikey-expires-at') as HTMLInputElement).value = futureDate.toISOString().split('T')[0] || "";

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await handleCreateApiKey(event);

      const errorEl = document.getElementById('create-apikey-error');
      expect(errorEl?.textContent).toContain('Failed to create API key');
    });

    // Issue #61: the backend now 400s on an empty/missing permissions
    // array, so the request body sent from the form must always carry the
    // checked selection.
    test('sends the checked permissions in the request body', async () => {
      const futureDate = new Date(Date.now() + 30 * 24 * 60 * 60 * 1000);
      (api.createApiKey as jest.Mock).mockResolvedValue({
        api_key: 'test-key',
        key_id: 'key-1',
        key: { id: 'key-1', name: 'Test', key_prefix: 'abc' }
      });
      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: [] });

      (document.getElementById('apikey-name') as HTMLInputElement).value = 'Test Key';
      (document.getElementById('apikey-password') as HTMLInputElement).value = 'my-password';
      (document.getElementById('apikey-expires-at') as HTMLInputElement).value = futureDate.toISOString().split('T')[0] || "";
      // The default fixture ships one pre-checked view:recommendations box.

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await handleCreateApiKey(event);

      expect(api.createApiKey).toHaveBeenCalledWith(
        expect.objectContaining({
          name: 'Test Key',
          permissions: [{ action: 'view', resource: 'recommendations' }],
        })
      );
      const [request] = (api.createApiKey as jest.Mock).mock.calls[0] as [{ permissions: unknown[] }];
      expect(request.permissions.length).toBeGreaterThan(0);
    });

    test('rejects submission with an error, and never calls the API, when no permission is checked', async () => {
      const futureDate = new Date(Date.now() + 30 * 24 * 60 * 60 * 1000);
      (document.getElementById('apikey-name') as HTMLInputElement).value = 'Test Key';
      (document.getElementById('apikey-password') as HTMLInputElement).value = 'my-password';
      (document.getElementById('apikey-expires-at') as HTMLInputElement).value = futureDate.toISOString().split('T')[0] || "";
      const checkbox = document.querySelector('.apikey-permission-checkbox') as HTMLInputElement;
      checkbox.checked = false;

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await handleCreateApiKey(event);

      const errorEl = document.getElementById('create-apikey-error');
      expect(errorEl?.textContent).toBe('Select at least one permission');
      expect(api.createApiKey).not.toHaveBeenCalled();
    });

    test('sends every checked permission when more than one is selected', async () => {
      const futureDate = new Date(Date.now() + 30 * 24 * 60 * 60 * 1000);
      document.getElementById('apikey-permissions-list')!.innerHTML = `
        <label><input type="checkbox" class="apikey-permission-checkbox" data-action="view" data-resource="recommendations" checked></label>
        <label><input type="checkbox" class="apikey-permission-checkbox" data-action="create" data-resource="plans" checked></label>
      `;
      (api.createApiKey as jest.Mock).mockResolvedValue({
        api_key: 'test-key',
        key_id: 'key-1',
        key: { id: 'key-1', name: 'Test', key_prefix: 'abc' }
      });
      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: [] });
      (document.getElementById('apikey-name') as HTMLInputElement).value = 'Test Key';
      (document.getElementById('apikey-password') as HTMLInputElement).value = 'my-password';
      (document.getElementById('apikey-expires-at') as HTMLInputElement).value = futureDate.toISOString().split('T')[0] || "";

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await handleCreateApiKey(event);

      expect(api.createApiKey).toHaveBeenCalledWith(
        expect.objectContaining({
          permissions: [
            { action: 'view', resource: 'recommendations' },
            { action: 'create', resource: 'plans' },
          ],
        })
      );
    });
  });

  describe('showKeyCreatedModal', () => {
    test('creates and shows modal with API key', () => {
      showKeyCreatedModal('test-api-key-12345');

      const modal = document.getElementById('apikey-created-modal');
      expect(modal).not.toBeNull();
      expect(modal?.classList.contains('hidden')).toBe(false);
      expect(modal?.innerHTML).toContain('test-api-key-12345');
    });

    test('shows warning about one-time display', () => {
      showKeyCreatedModal('test-key');

      const modal = document.getElementById('apikey-created-modal');
      expect(modal?.innerHTML).toContain('only time');
    });

    test('removes existing modal before creating new one', () => {
      // Create first modal
      showKeyCreatedModal('first-key');

      // Create second modal
      showKeyCreatedModal('second-key');

      // Should only be one modal
      const modals = document.querySelectorAll('#apikey-created-modal');
      expect(modals.length).toBe(1);
      expect(modals[0]?.innerHTML).toContain('second-key');
    });

    test('copy button copies key to clipboard', async () => {
      const writeTextMock = jest.fn().mockResolvedValue(undefined);
      Object.defineProperty(navigator, 'clipboard', {
        value: { writeText: writeTextMock },
        writable: true,
        configurable: true
      });

      showKeyCreatedModal('test-api-key');

      const copyBtn = document.getElementById('copy-apikey-btn');
      copyBtn?.click();

      await new Promise(resolve => setTimeout(resolve, 0));

      expect(writeTextMock).toHaveBeenCalledWith('test-api-key');
    });

    test('copy button shows feedback on success', async () => {
      jest.useFakeTimers();

      const writeTextMock = jest.fn().mockResolvedValue(undefined);
      Object.defineProperty(navigator, 'clipboard', {
        value: { writeText: writeTextMock },
        writable: true,
        configurable: true
      });

      showKeyCreatedModal('test-api-key');

      const copyBtn = document.getElementById('copy-apikey-btn') as HTMLButtonElement;
      copyBtn?.click();

      await Promise.resolve();

      expect(copyBtn.textContent).toBe('Copied!');
      expect(copyBtn.classList.contains('copied')).toBe(true);

      jest.advanceTimersByTime(2000);

      expect(copyBtn.textContent).toBe('Copy');
      expect(copyBtn.classList.contains('copied')).toBe(false);

      jest.useRealTimers();
    });

    test('copy button shows alert on clipboard error', async () => {
      const consoleError = jest.spyOn(console, 'error').mockImplementation(() => {});
      const writeTextMock = jest.fn().mockRejectedValue(new Error('Clipboard error'));
      Object.defineProperty(navigator, 'clipboard', {
        value: { writeText: writeTextMock },
        writable: true,
        configurable: true
      });

      showKeyCreatedModal('test-api-key');

      const copyBtn = document.getElementById('copy-apikey-btn');
      copyBtn?.click();

      await new Promise(resolve => setTimeout(resolve, 0));

      expect(window.alert).toHaveBeenCalledWith('Failed to copy to clipboard. Please copy manually.');
      consoleError.mockRestore();
    });

    test('close button removes modal', () => {
      showKeyCreatedModal('test-key');

      const closeBtn = document.getElementById('close-apikey-created-btn');
      closeBtn?.click();

      const modal = document.getElementById('apikey-created-modal');
      expect(modal).toBeNull();
    });
  });

  describe('revokeApiKey', () => {
    beforeEach(async () => {
      const mockKeys = [
        {
          id: 'key-1',
          name: 'Test Key',
          key_prefix: 'abc123',
          is_active: true,
          created_at: '2024-01-15T10:00:00Z'
        }
      ];

      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: mockKeys });
      await loadApiKeys();
    });

    test('does nothing if key not found', async () => {
      await revokeApiKey('non-existent-key');

      expect(api.revokeApiKey).not.toHaveBeenCalled();
    });

    test('does nothing if user cancels confirmation', async () => {
      mockConfirmDialog.mockResolvedValueOnce(false);

      await revokeApiKey('key-1');

      expect(api.revokeApiKey).not.toHaveBeenCalled();
    });

    test('revokes key and reloads list', async () => {
      (api.revokeApiKey as jest.Mock).mockResolvedValue({});

      await revokeApiKey('key-1');

      expect(api.revokeApiKey).toHaveBeenCalledWith('key-1');
      expect(api.getApiKeys).toHaveBeenCalledTimes(2); // Initial load + after revoke
    });

    test('toasts a failure while the hidden create modal still has its error element (issue #717)', async () => {
      const consoleError = jest.spyOn(console, 'error').mockImplementation(() => {});
      (api.revokeApiKey as jest.Mock).mockRejectedValue(new Error('Revoke failed'));

      await revokeApiKey('key-1');

      expect(document.querySelector('.toast-error')?.querySelector('.toast-message')?.textContent).toBe('Failed to revoke API key');
      expect(document.getElementById('create-apikey-error')?.classList.contains('hidden')).toBe(true);
      consoleError.mockRestore();
    });

    test('shows error on API failure', async () => {
      // Remove the create-apikey-error element so showError falls back to alert
      document.getElementById('create-apikey-error')?.remove();

      const consoleError = jest.spyOn(console, 'error').mockImplementation(() => {});
      (api.revokeApiKey as jest.Mock).mockRejectedValue(new Error('Revoke failed'));

      await revokeApiKey('key-1');

      expect(consoleError).toHaveBeenCalledWith('Failed to revoke API key:', expect.any(Error));
      expect(document.querySelector('.toast-error')?.querySelector('.toast-message')?.textContent).toBe('Failed to revoke API key');
      consoleError.mockRestore();
    });
  });

  describe('deleteApiKey', () => {
    beforeEach(async () => {
      const mockKeys = [
        {
          id: 'key-1',
          name: 'Test Key',
          key_prefix: 'abc123',
          is_active: false,
          created_at: '2024-01-15T10:00:00Z'
        }
      ];

      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: mockKeys });
      await loadApiKeys();
    });

    test('does nothing if key not found', async () => {
      await deleteApiKey('non-existent-key');

      expect(api.deleteApiKey).not.toHaveBeenCalled();
    });

    test('does nothing if user cancels confirmation', async () => {
      mockConfirmDialog.mockResolvedValueOnce(false);

      await deleteApiKey('key-1');

      expect(api.deleteApiKey).not.toHaveBeenCalled();
    });

    test('deletes key and reloads list', async () => {
      (api.deleteApiKey as jest.Mock).mockResolvedValue({});

      await deleteApiKey('key-1');

      expect(api.deleteApiKey).toHaveBeenCalledWith('key-1');
      expect(api.getApiKeys).toHaveBeenCalledTimes(2); // Initial load + after delete
    });

    test('shows error on API failure', async () => {
      // Remove the create-apikey-error element so showError falls back to alert
      document.getElementById('create-apikey-error')?.remove();

      const consoleError = jest.spyOn(console, 'error').mockImplementation(() => {});
      (api.deleteApiKey as jest.Mock).mockRejectedValue(new Error('Delete failed'));

      await deleteApiKey('key-1');

      expect(consoleError).toHaveBeenCalledWith('Failed to delete API key:', expect.any(Error));
      expect(document.querySelector('.toast-error')?.querySelector('.toast-message')?.textContent).toBe('Failed to delete API key');
      consoleError.mockRestore();
    });
  });

  describe('initApiKeys', () => {
    test('sets up create key button', () => {
      initApiKeys();

      const createBtn = document.getElementById('create-apikey-btn');
      createBtn?.click();

      const modal = document.getElementById('create-apikey-modal');
      expect(modal?.classList.contains('hidden')).toBe(false);
    });

    test('sets up close modal button', () => {
      initApiKeys();

      // First show the modal
      const modal = document.getElementById('create-apikey-modal');
      modal?.classList.remove('hidden');

      // Click close button
      const closeBtn = document.getElementById('close-create-apikey-modal-btn');
      closeBtn?.click();

      expect(modal?.classList.contains('hidden')).toBe(true);
    });

    test('sets up form submission handler', async () => {
      const futureDate = new Date(Date.now() + 30 * 24 * 60 * 60 * 1000);
      (api.createApiKey as jest.Mock).mockResolvedValue({
        api_key: 'test-key',
        key_id: 'key-1',
        key: { id: 'key-1', name: 'Test', key_prefix: 'abc' }
      });
      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: [] });

      initApiKeys();

      (document.getElementById('apikey-name') as HTMLInputElement).value = 'Test Key';
      (document.getElementById('apikey-password') as HTMLInputElement).value = 'my-password';
      (document.getElementById('apikey-expires-at') as HTMLInputElement).value = futureDate.toISOString().split('T')[0] || "";

      const form = document.getElementById('create-apikey-form');
      form?.dispatchEvent(new Event('submit'));

      await new Promise(resolve => setTimeout(resolve, 0));

      expect(api.createApiKey).toHaveBeenCalled();
    });

    test('sets up modal backdrop click to close', () => {
      initApiKeys();

      const modal = document.getElementById('create-apikey-modal');
      modal?.classList.remove('hidden');

      // Simulate click on modal backdrop
      const clickEvent = new MouseEvent('click', { bubbles: true });
      Object.defineProperty(clickEvent, 'target', { value: modal });
      modal?.dispatchEvent(clickEvent);

      expect(modal?.classList.contains('hidden')).toBe(true);
    });

    test('handles missing elements gracefully', () => {
      document.body.innerHTML = '';

      // Should not throw
      expect(() => initApiKeys()).not.toThrow();
    });
  });

  describe('secret handling (issue #732)', () => {
    test('a second submit while the first is in flight mints only one key and disables submit', async () => {
      document.getElementById('create-apikey-modal')?.classList.remove('hidden');
      (document.getElementById('apikey-name') as HTMLInputElement).value = 'Test Key';
      (document.getElementById('apikey-password') as HTMLInputElement).value = 'pw';
      (document.getElementById('apikey-expires-at') as HTMLInputElement).value = '2999-01-01';
      let resolve!: (v: unknown) => void;
      (api.createApiKey as jest.Mock).mockReturnValue(new Promise((r) => { resolve = r; }));
      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: [] });
      const event = { preventDefault: jest.fn() } as unknown as Event;

      const first = handleCreateApiKey(event);
      await handleCreateApiKey(event);
      expect((document.getElementById('create-apikey-submit-btn') as HTMLButtonElement).disabled).toBe(true);
      resolve({ api_key: 'k', key_id: 'key-1', key: { id: 'key-1', name: 'Test Key', key_prefix: 'abc' } });
      await first;

      expect(api.createApiKey).toHaveBeenCalledTimes(1);
    });

    test('Escape does not dismiss the created-key modal; the button does', () => {
      showKeyCreatedModal('secret-key');
      const modal = document.getElementById('apikey-created-modal')!;
      modal.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
      expect(document.getElementById('apikey-created-modal')).not.toBeNull();
      expect(modal.classList.contains('hidden')).toBe(false);

      document.getElementById('close-apikey-created-btn')!.click();
      expect(document.getElementById('apikey-created-modal')).toBeNull();
    });
  });

  describe('Error display', () => {
    test('shows error in error element when available', async () => {
      document.getElementById('create-apikey-modal')?.classList.remove('hidden');
      const event = { preventDefault: jest.fn() } as unknown as Event;
      await handleCreateApiKey(event);

      const errorEl = document.getElementById('create-apikey-error');
      expect(errorEl?.classList.contains('hidden')).toBe(false);
      expect(errorEl?.textContent).not.toBe('');
    });

    test('falls back to toast when error element not available', async () => {
      // Remove the inline error element so showError falls back to the
      // shared toast system (Q4).
      document.getElementById('create-apikey-error')?.remove();

      const event = { preventDefault: jest.fn() } as unknown as Event;
      await handleCreateApiKey(event);

      expect(document.querySelector('.toast-error')?.querySelector('.toast-message')?.textContent).toBe('API key name is required');
    });
  });

  describe('HTML escaping', () => {
    test('escapes HTML in key names to prevent XSS', async () => {
      const mockKeys = [
        {
          id: 'key-1',
          name: '<script>alert("xss")</script>',
          key_prefix: 'abc123',
          is_active: true,
          created_at: '2024-01-15T10:00:00Z'
        }
      ];

      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: mockKeys });

      await loadApiKeys();

      const container = document.getElementById('apikeys-list');
      expect(container?.innerHTML).not.toContain('<script>');
      expect(container?.innerHTML).toContain('&lt;script&gt;');
    });

    test('escapes HTML in API key display', () => {
      showKeyCreatedModal('<script>alert("xss")</script>');

      const modal = document.getElementById('apikey-created-modal');
      expect(modal?.innerHTML).not.toContain('<script>alert');
    });
  });

  describe('Date formatting', () => {
    test('formats dates correctly in the list', async () => {
      const mockKeys = [
        {
          id: 'key-1',
          name: 'Test Key',
          key_prefix: 'abc123',
          is_active: true,
          created_at: '2024-01-15T10:00:00Z',
          last_used_at: '2024-01-16T15:30:00Z',
          expires_at: '2025-01-15T10:00:00Z'
        }
      ];

      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: mockKeys });

      await loadApiKeys();

      const container = document.getElementById('apikeys-list');
      // The exact format depends on locale, but it should contain date parts
      expect(container?.innerHTML).toContain('2024');
    });

    // #492: last_used_at null renders "Never", not "0 min ago" or empty
    test('renders Never for null last_used_at (issue #492)', async () => {
      const mockKeys = [
        {
          id: 'key-1',
          name: 'Unused Key',
          key_prefix: 'abc123',
          is_active: true,
          created_at: '2024-01-15T10:00:00Z'
          // last_used_at intentionally absent
        }
      ];

      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: mockKeys });
      await loadApiKeys();

      const container = document.getElementById('apikeys-list');
      expect(container?.innerHTML).toContain('Never');
    });

    test('one malformed last_used_at degrades to one cell instead of blanking the table (#136)', async () => {
      const mockKeys = [
        { id: 'key-bad', name: 'Bad Timestamp', key_prefix: 'bad111', is_active: true,
          created_at: '2024-01-15T10:00:00Z', last_used_at: 'not-a-date' },
        { id: 'key-ok', name: 'Leaked Key', key_prefix: 'ok2222', is_active: true,
          created_at: '2024-01-15T10:00:00Z', last_used_at: '2024-01-16T15:30:00Z' },
      ];

      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: mockKeys });
      await loadApiKeys();

      const container = document.getElementById('apikeys-list');
      const rows = container?.querySelectorAll('tbody tr');
      expect(rows).toHaveLength(2);
      expect(rows?.[0]?.textContent).toContain('--');
      expect(container?.querySelectorAll('.revoke-key-btn')).toHaveLength(2);
    });

    // #492: populated last_used_at renders relative time with ISO title for hover
    test('renders relative time with ISO title for populated last_used_at (issue #492)', async () => {
      const recentTs = new Date(Date.now() - 2 * 3600 * 1000).toISOString(); // 2 hours ago
      const mockKeys = [
        {
          id: 'key-1',
          name: 'Used Key',
          key_prefix: 'abc123',
          is_active: true,
          created_at: '2024-01-15T10:00:00Z',
          last_used_at: recentTs
        }
      ];

      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: mockKeys });
      await loadApiKeys();

      const container = document.getElementById('apikeys-list');
      // Relative label: ends with "h ago" for a 2-hour-old timestamp
      expect(container?.innerHTML).toMatch(/\d+h ago/);
      // Absolute ISO on the title attribute for hover
      const span = container?.querySelector<HTMLElement>('[title]');
      expect(span?.getAttribute('title')).toBe(new Date(recentTs).toISOString());
    });
  });

  // Usage stats (issue #340/#344 deferred sub-task) — section-level summary
  // card above the table + per-row request count columns.
  describe('Usage stats', () => {
    test('renders summary tiles when stats load successfully', async () => {
      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: [] });
      (api.getApiKeysUsageStats as jest.Mock).mockResolvedValue({
        total_active: 2,
        total_requests_window: 42,
        total_requests_lifetime: 1234,
        top_keys: [
          { id: 'key-1', name: 'Busy', key_prefix: 'aaaa1111', request_count_window: 30 },
          { id: 'key-2', name: 'Medium', key_prefix: 'bbbb2222', request_count_window: 12 },
        ],
      });

      await loadApiKeys();
      // Allow the parallel stats fetch to resolve.
      await new Promise((r) => setTimeout(r, 0));

      const summary = document.getElementById('apikeys-usage-summary');
      expect(summary?.textContent).toContain('Active keys');
      expect(summary?.textContent).toContain('2');
      expect(summary?.textContent).toContain('Requests (window)');
      expect(summary?.textContent).toContain('42');
      expect(summary?.textContent).toContain('Requests (lifetime)');
      // formatCount abbreviates large numbers in the summary tiles to
      // fit the tile width — table cells keep the exact integer.
      expect(summary?.textContent).toContain('1.2k');
      expect(summary?.textContent).toContain('Most active (window)');
      expect(summary?.textContent).toContain('Busy');
      expect(summary?.textContent).toContain('aaaa1111');
    });

    test('omits top-list when no key has window activity', async () => {
      (api.getApiKeys as jest.Mock).mockResolvedValue({ api_keys: [] });
      (api.getApiKeysUsageStats as jest.Mock).mockResolvedValue({
        total_active: 1,
        total_requests_window: 0,
        total_requests_lifetime: 7,
        top_keys: [],
      });

      await loadApiKeys();
      await new Promise((r) => setTimeout(r, 0));

      const summary = document.getElementById('apikeys-usage-summary');
      expect(summary?.textContent).not.toContain('Most active (window)');
    });

    test('shows inline error when stats endpoint fails, table still renders', async () => {
      (api.getApiKeys as jest.Mock).mockResolvedValue({
        api_keys: [
          { id: 'key-1', name: 'k', key_prefix: 'abc12345', is_active: true, created_at: '2024-01-15T10:00:00Z' },
        ],
      });
      (api.getApiKeysUsageStats as jest.Mock).mockRejectedValue(new Error('boom'));

      await loadApiKeys();
      await new Promise((r) => setTimeout(r, 0));

      const summary = document.getElementById('apikeys-usage-summary');
      expect(summary?.querySelector('.error')?.textContent).toBe('Failed to load usage summary');
      // The table itself still renders despite the summary failure.
      const list = document.getElementById('apikeys-list');
      expect(list?.innerHTML).toContain('abc12345');
    });

    test('renders Requests (window) and Requests (total) columns per row', async () => {
      (api.getApiKeys as jest.Mock).mockResolvedValue({
        api_keys: [
          {
            id: 'key-1',
            name: 'k1',
            key_prefix: 'abc12345',
            is_active: true,
            created_at: '2024-01-15T10:00:00Z',
            request_count_window: 12,
            request_count_total: 1234,
          },
        ],
      });

      await loadApiKeys();

      const container = document.getElementById('apikeys-list');
      expect(container?.innerHTML).toContain('Requests (window)');
      expect(container?.innerHTML).toContain('Requests (total)');
      // Counts render with thousands separator for readability.
      expect(container?.innerHTML).toContain('1,234');
      expect(container?.innerHTML).toContain('apikeys-count-cell');
    });

    test('shows no data when counter fields are missing from the response', async () => {
      (api.getApiKeys as jest.Mock).mockResolvedValue({
        api_keys: [
          // No request_count_* fields — simulates a cached older response.
          { id: 'key-1', name: 'k1', key_prefix: 'abc12345', is_active: true, created_at: '2024-01-15T10:00:00Z' },
        ],
      });

      await loadApiKeys();

      const container = document.getElementById('apikeys-list');
      const cells = container?.querySelectorAll('.apikeys-count-cell');
      expect(cells?.length).toBe(2);
      expect(cells?.[0]?.textContent).toBe('n/a');
      expect(cells?.[1]?.textContent).toBe('n/a');
    });

    // Regression: a key already in use before migration 000094 has no
    // recoverable lifetime count. The backend sends null for it; rendering
    // "0" would state a request volume nobody measured. A genuinely-unused
    // key still shows 0.
    test('renders an unknown lifetime count as no data, and a real zero as 0', async () => {
      (api.getApiKeys as jest.Mock).mockResolvedValue({
        api_keys: [
          {
            id: 'pre-migration',
            name: 'old',
            key_prefix: 'abc12345',
            is_active: true,
            created_at: '2024-01-15T10:00:00Z',
            request_count_window: 0,
            request_count_total: null,
          },
          {
            id: 'never-used',
            name: 'fresh',
            key_prefix: 'def67890',
            is_active: true,
            created_at: '2024-01-15T10:00:00Z',
            request_count_window: 0,
            request_count_total: 0,
          },
        ],
      });

      await loadApiKeys();

      const cells = document.getElementById('apikeys-list')?.querySelectorAll('.apikeys-count-cell');
      expect(cells?.length).toBe(4);
      // Row 1: window 0 is a real value, lifetime is unknown.
      expect(cells?.[0]?.textContent).toBe('0');
      expect(cells?.[1]?.textContent).toBe('n/a');
      // Row 2: never used, so both are a genuine zero.
      expect(cells?.[2]?.textContent).toBe('0');
      expect(cells?.[3]?.textContent).toBe('0');
    });

    test('list-fetch failure clears summary skeleton and shows error', async () => {
      (api.getApiKeys as jest.Mock).mockRejectedValue(new Error('list boom'));
      (api.getApiKeysUsageStats as jest.Mock).mockResolvedValue({
        total_active: 0,
        total_requests_window: 0,
        total_requests_lifetime: 0,
        top_keys: [],
      });

      await loadApiKeys();
      await new Promise((r) => setTimeout(r, 0));

      const list = document.getElementById('apikeys-list');
      expect(list?.querySelector('.error')?.textContent).toBe('Failed to load API keys');
      // The skeleton-active marker is gone (success/error both clear it).
      expect(list?.dataset['skeletonActive']).toBeUndefined();
    });
  });
});
