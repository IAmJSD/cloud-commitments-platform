-- Migration 000152: server-issued MFA login continuation tokens (issue #134).
--
-- A login with the right password for an MFA-enrolled user answered
-- mfa_required and nothing else, so the browser kept the plaintext password
-- in memory until the user typed the code and sent password + code again.
-- That login now also returns a short-lived, single-use mfa_token, and the
-- code step can send {email, mfa_token, mfa_code} instead of the password.
--
-- Only the SHA-256 of the token is stored, like sessions.token. The row binds
-- the token to its user and to the users.password_version the password leg
-- verified, so a password change between the two legs voids it. attempts
-- counts code checks against the token; the claim that runs before each check
-- refuses a token at its cap, so concurrent guesses cannot exceed it. The
-- successful code check deletes the row, which is what makes the token single
-- use. Expired rows are swept when new tokens are issued.
CREATE TABLE IF NOT EXISTS mfa_login_tokens (
    token_hash TEXT PRIMARY KEY CHECK (token_hash <> ''),
    user_id UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    password_version BIGINT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_mfa_login_tokens_expires_at
    ON mfa_login_tokens (expires_at);

CREATE INDEX IF NOT EXISTS idx_mfa_login_tokens_user_id
    ON mfa_login_tokens (user_id);
