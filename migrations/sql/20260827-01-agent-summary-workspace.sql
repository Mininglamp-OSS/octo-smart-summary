-- +migrate Up
-- Compatibility marker for environments that already applied the original
-- all-in-one workspace migration under this immutable migration ID.
-- New installations apply the split migrations that follow this marker.
SELECT 1;

-- +migrate Down
SELECT 1;
