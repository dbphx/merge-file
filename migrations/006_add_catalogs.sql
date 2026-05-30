CREATE TABLE IF NOT EXISTS catalogs (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source_type TEXT NOT NULL CHECK (source_type IN ('drive', 'upload')),
    title TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS catalog_pages (
    id BIGSERIAL PRIMARY KEY,
    catalog_id BIGINT NOT NULL REFERENCES catalogs(id) ON DELETE CASCADE,
    source_kind TEXT NOT NULL,
    source_name TEXT NOT NULL,
    source_order INTEGER NOT NULL,
    source_size BIGINT NULL,
    drive_file_id TEXT NULL,
    source_object_key TEXT NULL,
    mime_type TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_catalogs_user_id_created_at ON catalogs(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_catalog_pages_catalog_id_order ON catalog_pages(catalog_id, source_order, source_name);
