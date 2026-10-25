/**
 * Issue #748: user-facing wording for API and transport failures.
 */
import {
  BAD_RESPONSE_MESSAGE,
  NETWORK_ERROR_MESSAGE,
  friendlyError,
  httpStatusMessage,
  isGenericServerMessage,
  isNetworkFailure,
  readJson,
  responseError,
} from '../api/errors';

function response(status: number, body: () => Promise<unknown>): Response {
  return { ok: status < 400, status, url: '/api/auth/login', json: body } as unknown as Response;
}

describe('isNetworkFailure', () => {
  test.each(['Failed to fetch', 'NetworkError when attempting to fetch resource.', 'Load failed'])(
    'recognizes the browser fetch TypeError "%s"', (msg) => {
      expect(isNetworkFailure(new TypeError(msg))).toBe(true);
    });

  test('ignores other errors', () => {
    expect(isNetworkFailure(new TypeError('x is not a function'))).toBe(false);
    expect(isNetworkFailure(new Error('Failed to fetch'))).toBe(false);
  });
});

describe('friendlyError', () => {
  test('maps a raw fetch failure and a JSON parse error', () => {
    expect(friendlyError(new TypeError('Failed to fetch'))).toBe(NETWORK_ERROR_MESSAGE);
    expect(friendlyError(new SyntaxError("Unexpected token '<'"))).toBe(BAD_RESPONSE_MESSAGE);
  });

  test('keeps an error message the server or client already made readable', () => {
    expect(friendlyError(Object.assign(new Error('plan name already in use'), { status: 409 }))).toBe('plan name already in use');
  });

  test('falls back to the status text, then a generic sentence', () => {
    expect(friendlyError(Object.assign(new Error(''), { status: 503 }))).toBe(httpStatusMessage(503));
    expect(friendlyError(undefined)).toBe('Something went wrong. Try again in a moment.');
    expect(friendlyError('quota exceeded')).toBe('quota exceeded');
  });
});

describe('isGenericServerMessage', () => {
  test('matches only the backend catch-all', () => {
    expect(isGenericServerMessage('Internal server error')).toBe(true);
    expect(isGenericServerMessage('internal server error.')).toBe(true);
    expect(isGenericServerMessage('internal server error: db down')).toBe(false);
  });
});

describe('responseError', () => {
  test('prefers a specific server error, then message', async () => {
    await expect(responseError(response(400, () => Promise.resolve({ error: 'bad email' })), 'Login failed'))
      .resolves.toMatchObject({ message: 'bad email', status: 400 });
    await expect(responseError(response(400, () => Promise.resolve({ message: 'weak password' })), 'x'))
      .resolves.toMatchObject({ message: 'weak password' });
  });

  test('uses the friendly status text for a 5xx with no or a generic body', async () => {
    await expect(responseError(response(502, () => Promise.reject(new SyntaxError('<'))), 'Login failed'))
      .resolves.toMatchObject({ message: httpStatusMessage(502), status: 502, detail: 'HTTP 502 from /api/auth/login' });
    await expect(responseError(response(500, () => Promise.resolve({ error: 'Internal server error' })), 'Login failed'))
      .resolves.toMatchObject({ message: httpStatusMessage(500) });
  });

  test('uses the caller fallback for a 4xx with no message', async () => {
    await expect(responseError(response(401, () => Promise.resolve({})), 'Login failed'))
      .resolves.toMatchObject({ message: 'Login failed', status: 401 });
  });
});

describe('readJson', () => {
  test('returns the parsed body', async () => {
    await expect(readJson(response(200, () => Promise.resolve({ a: 1 })))).resolves.toEqual({ a: 1 });
  });

  test('turns a parse failure into friendly text', async () => {
    await expect(readJson(response(200, () => Promise.reject(new SyntaxError("Unexpected token '<'")))))
      .rejects.toMatchObject({ message: BAD_RESPONSE_MESSAGE, status: 200 });
  });
});
