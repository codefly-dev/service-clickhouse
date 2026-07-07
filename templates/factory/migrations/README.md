# We use golang-migrate to manage ClickHouse migrations.

ClickHouse DDL is not transactional — write every migration idempotently
(CREATE TABLE IF NOT EXISTS, etc.) so a re-apply after an interrupted run is safe.
