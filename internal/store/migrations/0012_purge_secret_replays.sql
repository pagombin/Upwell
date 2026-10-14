-- A1: responses that carried a secret (TOTP secret, recovery codes, API
-- token) were kept for idempotent replay. They are no longer stored; remove
-- the ones already there.
DELETE FROM idempotency WHERE method = 'POST' AND path IN ('/api/v1/auth/totp/enroll', '/api/v1/auth/totp/confirm', '/api/v1/auth/totp/recovery-codes', '/api/v1/api-tokens');
