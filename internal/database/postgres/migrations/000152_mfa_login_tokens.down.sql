-- Reverse 000152: drop the MFA login continuation tokens (issue #134).
-- Lossy by design: pending MFA logins are dropped, so a user between the
-- password and code steps has to enter the password again.
DROP TABLE IF EXISTS mfa_login_tokens;
