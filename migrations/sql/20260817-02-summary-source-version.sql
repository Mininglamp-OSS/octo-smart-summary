-- +migrate Up
ALTER TABLE summary_source
  ADD COLUMN source_version VARCHAR(128) NOT NULL DEFAULT '' AFTER source_name;

-- +migrate Down
ALTER TABLE summary_source
  DROP COLUMN source_version;
