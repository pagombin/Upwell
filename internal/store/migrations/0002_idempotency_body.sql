-- Idempotency keys remember a hash of the request body, so a reused key with
-- a different request is refused instead of replaying another request's answer.
ALTER TABLE idempotency ADD COLUMN body_hash TEXT NOT NULL DEFAULT '';
