/**
 * Issue #731: the login, first-admin setup and password reset gates are
 * accessible dialogs: named by their heading, focus moves inside and Tab is
 * trapped, Escape cannot dismiss them, and their errors are announced.
 */
import { showLoginModal, showAdminSetupModal, showResetPasswordModal } from '../auth';

jest.mock('../utils', () => ({
  ...jest.requireActual('../utils'),
  reloadPage: jest.fn(),
}));

jest.mock('../api', () => {
  class MFALoginError extends Error {
    code: string;
    constructor(code: string) {
      super(code);
      this.name = 'MFALoginError';
      this.code = code;
      Object.setPrototypeOf(this, MFALoginError.prototype);
    }
  }
  return {
    login: jest.fn(),
    getResetTokenStatus: jest.fn(),
    MFALoginError,
  };
});

jest.mock('../state', () => ({
  getCurrentUser: jest.fn(),
  setCurrentUser: jest.fn(),
}));

jest.mock('../toast', () => ({ showToast: jest.fn() }));

import * as api from '../api';

function press(el: HTMLElement, key: string, shiftKey = false): void {
  el.dispatchEvent(new KeyboardEvent('keydown', { key, shiftKey, bubbles: true, cancelable: true }));
}

function expectNamedDialog(modal: HTMLElement, heading: string): void {
  const dialog = modal.querySelector('[role="dialog"]')!;
  expect(dialog).not.toBeNull();
  expect(dialog.getAttribute('aria-modal')).toBe('true');
  const titleId = dialog.getAttribute('aria-labelledby') ?? '';
  expect(document.getElementById(titleId)?.textContent).toBe(heading);
}

describe('auth gate dialogs (#731)', () => {
  beforeEach(() => {
    document.body.innerHTML = '';
    jest.clearAllMocks();
  });

  test('login modal is a named dialog, focuses the email field and announces errors', async () => {
    await showLoginModal();
    const modal = document.getElementById('login-modal')!;

    expectNamedDialog(modal, 'CUDly Login');
    expect(document.activeElement?.id).toBe('login-email');
    expect(document.getElementById('login-error')?.getAttribute('role')).toBe('alert');
  });

  test('Tab wraps inside the login modal and Escape does not dismiss it', async () => {
    await showLoginModal();
    const modal = document.getElementById('login-modal')!;

    press(modal, 'Tab', true);
    expect(modal.contains(document.activeElement)).toBe(true);
    expect(document.activeElement?.textContent).toBe('Login');

    press(modal, 'Escape');
    expect(document.getElementById('login-modal')).not.toBeNull();
    expect(modal.classList.contains('hidden')).toBe(false);
  });

  test('MFA step keeps focus on the code field and announces "Incorrect code"', async () => {
    (api.login as jest.Mock).mockRejectedValueOnce(new api.MFALoginError('mfa_required'));
    await showLoginModal();
    (document.getElementById('login-email') as HTMLInputElement).value = 'a@example.com';
    (document.getElementById('login-password') as HTMLInputElement).value = 'secret-password';
    document.getElementById('login-form')!.dispatchEvent(new Event('submit', { cancelable: true }));
    await new Promise(r => setTimeout(r, 0));

    expect(document.activeElement?.id).toBe('mfa-code');
    expect(document.getElementById('login-error')?.getAttribute('role')).toBe('alert');
  });

  test('forgot-password swap moves focus to the email field', async () => {
    await showLoginModal();
    document.getElementById('forgot-password-link')!.click();
    expect(document.activeElement?.id).toBe('reset-email');
  });

  test('admin setup modal is a named dialog with an announced error', async () => {
    await showAdminSetupModal();
    const modal = document.getElementById('admin-setup-modal')!;

    expectNamedDialog(modal, 'Welcome to CUDly');
    expect(document.activeElement?.id).toBe('setup-api-key');
    expect(document.getElementById('setup-error')?.getAttribute('role')).toBe('alert');
  });

  test('reset password modal is a named dialog for each token state', async () => {
    (api.getResetTokenStatus as jest.Mock).mockResolvedValueOnce({ state: 'valid', flow: 'reset' });
    await showResetPasswordModal('tok');
    let modal = document.getElementById('reset-password-modal')!;
    expectNamedDialog(modal, 'Reset Your Password');
    expect(document.activeElement?.id).toBe('new-password');
    expect(document.getElementById('reset-error')?.getAttribute('role')).toBe('alert');

    (api.getResetTokenStatus as jest.Mock).mockResolvedValueOnce({ state: 'expired', flow: 'reset' });
    await showResetPasswordModal('tok');
    modal = document.getElementById('reset-password-modal')!;
    expectNamedDialog(modal, 'Password reset link expired');
    expect(document.activeElement?.id).toBe('reset-expired-request-new');

    (api.getResetTokenStatus as jest.Mock).mockResolvedValueOnce({ state: 'used', flow: 'invite' });
    await showResetPasswordModal('tok');
    modal = document.getElementById('reset-password-modal')!;
    expectNamedDialog(modal, 'Invitation link already used');
    expect(document.activeElement?.id).toBe('reset-used-go-to-login');
  });
});
