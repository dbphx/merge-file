ALTER TABLE jobs
    ADD COLUMN IF NOT EXISTS current_file_bytes BIGINT NOT NULL DEFAULT 0 CHECK (current_file_bytes >= 0);

ALTER TABLE jobs
    ADD COLUMN IF NOT EXISTS current_file_size BIGINT NOT NULL DEFAULT 0 CHECK (current_file_size >= 0);
