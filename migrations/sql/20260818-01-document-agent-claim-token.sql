-- +migrate Up
ALTER TABLE summary_document_agent_idempotency
  ADD COLUMN claim_token VARCHAR(64) NOT NULL DEFAULT '';

-- +migrate Down
ALTER TABLE summary_document_agent_idempotency
  DROP COLUMN claim_token;
