/**
 * Cross-tab logout sync regression (issue #493).
 * Guards installStorageListener() in api/client.ts: if its key filter,
 * newValue check, install-order, or idempotency ever regresses the
 * SECURITY mitigation noted in client.ts goes silently false.
 */

jest.mock('../utils', () => ({
  ...jest.requireActual('../utils'),
  reloadPage: jest.fn(),
}));

afterEach(() => {
  // initAuth() seeds in-memory auth from localStorage, so clear it between
  // tests to stop stale tokens from one case leaking into the next.
  localStorage.clear();
});

function loadAuth(): { api: typeof import('../api'); handler: (e: StorageEvent) => void; reload: jest.Mock; addSpy: jest.SpyInstance } {
  let out: ReturnType<typeof loadAuth> | undefined;
  jest.isolateModules(() => {
    const addSpy = jest.spyOn(window, 'addEventListener');
    // eslint-disable-next-line @typescript-eslint/no-require-imports
    const { reloadPage: reload } = require('../utils') as { reloadPage: jest.Mock };
    // eslint-disable-next-line @typescript-eslint/no-require-imports
    const api = require('../api') as typeof import('../api');
    api.initAuth();
    const call = addSpy.mock.calls.find(c => c[0] === 'storage');
    if (!call) throw new Error('initAuth() did not install a storage listener');
    out = { api, handler: call[1] as (e: StorageEvent) => void, reload, addSpy };
  });
  if (!out) throw new Error('loadAuth failed to capture storage listener');
  return out;
}

// jsdom rejects the jest.fn() storage mock as storageArea, so omit it
// (the listener never reads it).
const evt = (key: string | null, newValue: string | null): StorageEvent =>
  new StorageEvent('storage', { key, newValue });

describe('cross-tab logout sync (issue #493)', () => {
  test.each(['authToken', 'apiKey', 'csrfToken'])('cleared %s in another tab clears in-memory auth and reloads', key => {
    const { api, handler, reload } = loadAuth();
    api.setAuthToken('t'); api.setApiKey('k');
    handler(evt(key, null));
    expect(api.isAuthenticated()).toBe(false);
    expect(reload).toHaveBeenCalledTimes(1);
  });

  test('ignores non-auth keys', () => {
    const { api, handler, reload } = loadAuth();
    api.setAuthToken('t');
    handler(evt('someOtherKey', null));
    expect(api.isAuthenticated()).toBe(true);
    expect(reload).not.toHaveBeenCalled();
  });

  test('ignores partial updates such as token refresh (newValue !== null)', () => {
    const { api, handler, reload } = loadAuth();
    api.setAuthToken('t');
    handler(evt('authToken', 'rotated'));
    expect(api.isAuthenticated()).toBe(true);
    expect(reload).not.toHaveBeenCalled();
  });

  test('localStorage.clear() in another tab (key === null) signs this tab out (#317)', () => {
    const { api, handler, reload } = loadAuth();
    api.setAuthToken('t');
    handler(evt(null, null));
    expect(api.isAuthenticated()).toBe(false);
    expect(reload).toHaveBeenCalledTimes(1);
  });

  test('localStorage.clear() does not reload a tab that is already signed out', () => {
    const { api, handler, reload } = loadAuth();
    handler(evt(null, null));
    expect(api.isAuthenticated()).toBe(false);
    expect(reload).not.toHaveBeenCalled();
  });

  test('initAuth() is idempotent: storage listener installs exactly once', () => {
    const { api, addSpy } = loadAuth();
    api.initAuth(); api.initAuth();
    expect(addSpy.mock.calls.filter(c => c[0] === 'storage')).toHaveLength(1);
  });
});
