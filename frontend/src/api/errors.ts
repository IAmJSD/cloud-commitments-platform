/**
 * User-facing wording for API and transport failures (issue #748).
 *
 * The API client used to surface developer text: "HTTP 502", "Request to
 * /api/x timed out after 90000ms", the browser's "Failed to fetch", or a
 * JSON parse error such as "Unexpected token '<'" when a proxy answered
 * with an HTML error page. Those strings were interpolated into dozens of
 * toasts and inline errors. The client now builds its errors from these
 * helpers, so every caller that shows `err.message` gets a sentence a user
 * can act on, while the raw text is kept on `ApiError.detail` (and in the
 * console) for support. A message the server sent is always preferred
 * over these generic ones.
 */

import type { ApiError } from './types';

export const NETWORK_ERROR_MESSAGE = "Can't reach CUDly. Check your connection and try again.";
export const TIMEOUT_ERROR_MESSAGE = 'CUDly took too long to respond. Try again in a moment.';
export const UNAVAILABLE_ERROR_MESSAGE = 'CUDly is temporarily unavailable. Try again in a moment.';
export const BAD_RESPONSE_MESSAGE = 'CUDly sent an unexpected response. Try again in a moment.';
export const SERVER_ERROR_MESSAGE = 'CUDly hit an unexpected error. Try again in a moment.';
const UNKNOWN_ERROR_MESSAGE = 'Something went wrong. Try again in a moment.';

/** Generic sentence for an HTTP status when the server sent no message. */
export function httpStatusMessage(status: number): string {
  if (status === 401) return 'Your session has expired. Sign in again.';
  if (status === 403) return "You don't have permission to do that.";
  if (status === 404) return 'The requested item was not found.';
  if (status === 408 || status === 504) return TIMEOUT_ERROR_MESSAGE;
  if (status === 429) return 'Too many requests. Wait a moment and try again.';
  if (status === 500) return SERVER_ERROR_MESSAGE;
  if (status > 500) return UNAVAILABLE_ERROR_MESSAGE;
  return `The request could not be completed (error ${status}).`;
}

/**
 * The backend's catch-all 500 body ({"error": "internal server error"})
 * says nothing the status does not, so it gets the friendly status text;
 * any other server message is specific and is shown as sent.
 */
export function isGenericServerMessage(message: string): boolean {
  return /^internal server error\.?$/i.test(message.trim());
}

/** Build an ApiError with a user-facing message and the raw text for support. */
export function makeApiError(message: string, status: number, detail?: string): ApiError {
  const error: ApiError = new Error(message);
  error.status = status;
  if (detail) error.detail = detail;
  return error;
}

/**
 * True for the TypeError fetch() rejects with when the request never got
 * a response (offline, DNS, CORS, connection reset). Chrome says "Failed
 * to fetch", Firefox "NetworkError when attempting to fetch resource.",
 * Safari "Load failed".
 */
export function isNetworkFailure(err: unknown): boolean {
  return err instanceof TypeError
    && /failed to fetch|networkerror|load failed|network request failed/i.test(err.message);
}

/**
 * A message safe to show a user for any thrown value: the server's (or
 * client's) message for an ApiError, friendly text for a raw network or
 * parse failure, otherwise the error's own message.
 */
export function friendlyError(err: unknown): string {
  if (isNetworkFailure(err)) return NETWORK_ERROR_MESSAGE;
  if (err instanceof SyntaxError) return BAD_RESPONSE_MESSAGE;
  if (err instanceof Error) {
    if (err.message) return err.message;
    const status = (err as ApiError).status;
    return typeof status === 'number' && status > 0 ? httpStatusMessage(status) : UNKNOWN_ERROR_MESSAGE;
  }
  if (typeof err === 'string' && err) return err;
  return UNKNOWN_ERROR_MESSAGE;
}

/*
 * Helpers for the few auth calls that use fetch() directly instead of
 * apiRequest (login, password reset, first-admin setup), so they fail
 * with the same wording.
 */

/** fetch() that turns a transport failure into a friendly ApiError. */
export async function fetchOrNetworkError(url: string, init?: RequestInit): Promise<Response> {
  try {
    return await fetch(url, init);
  } catch (err) {
    if (isNetworkFailure(err)) {
      throw makeApiError(NETWORK_ERROR_MESSAGE, 0, `Request to ${url} failed: ${(err as Error).message}`);
    }
    throw err;
  }
}

/**
 * The error to throw for a non-OK response: the server's `error` (or
 * `message`) when it sent a specific one, the friendly status text for a
 * 5xx or an unreadable body, otherwise the caller's fallback.
 */
export async function responseError(response: Response, fallback: string): Promise<ApiError> {
  const data = await response.json().catch(() => ({})) as { error?: unknown; message?: unknown };
  const raw = typeof data.error === 'string' ? data.error : typeof data.message === 'string' ? data.message : '';
  const generic = !raw || isGenericServerMessage(raw);
  const message = !generic ? raw : response.status >= 500 ? httpStatusMessage(response.status) : fallback;
  return makeApiError(message, response.status, `HTTP ${response.status} from ${response.url || 'auth endpoint'}`);
}

/** response.json() that fails with friendly text on an HTML or truncated body. */
export async function readJson<T>(response: Response): Promise<T> {
  try {
    return await response.json() as T;
  } catch (err) {
    throw makeApiError(BAD_RESPONSE_MESSAGE, response.status, `Unparseable response: ${(err as Error).message}`);
  }
}
