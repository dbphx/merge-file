ALTER TABLE jobs
    ADD COLUMN IF NOT EXISTS current_stage TEXT NULL;

ALTER TABLE jobs
    ADD COLUMN IF NOT EXISTS current_file_name TEXT NULL;

ALTER TABLE jobs
    ADD COLUMN IF NOT EXISTS current_file_index INTEGER NOT NULL DEFAULT 0 CHECK (current_file_index >= 0);

ALTER TABLE jobs
    ADD COLUMN IF NOT EXISTS total_files INTEGER NOT NULL DEFAULT 0 CHECK (total_files >= 0);
