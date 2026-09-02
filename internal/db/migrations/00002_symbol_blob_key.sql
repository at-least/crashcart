-- Symbol files may live in a blob store (BLOB_STORE=s3|fs) instead of the
-- data column: exactly one of data / blob_key is set per row. Existing rows
-- keep their bytes in data; nothing is moved — a row's location is its own,
-- so a database can hold both kinds after the backend is switched
-- (internal/symbolicate/files.go).
--
-- +goose Up
ALTER TABLE symbol_files ALTER COLUMN data DROP NOT NULL;
ALTER TABLE symbol_files ADD COLUMN blob_key TEXT;
ALTER TABLE symbol_files ADD CONSTRAINT symbol_files_location
    CHECK ((data IS NULL) <> (blob_key IS NULL));

-- +goose Down
-- No down: this project authors forward-only migrations. Restore an
-- earlier schema from a `crashcart export` taken before upgrading.
