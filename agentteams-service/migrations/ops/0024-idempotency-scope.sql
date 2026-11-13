-- R1 security correction: Idempotency-Key is scoped to the caller and bound
-- to the normalized create request. The original global key let a second
-- principal replay another caller's 201 body (including env secrets).
ALTER TABLE idempotency_keys
  ADD COLUMN principal_id VARCHAR(253) NOT NULL DEFAULT '' AFTER idem_key,
  ADD COLUMN request_hash CHAR(64) NOT NULL DEFAULT '' AFTER principal_id;
