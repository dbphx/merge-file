ALTER TABLE job_files
    ADD COLUMN IF NOT EXISTS source_object_key TEXT NULL;
