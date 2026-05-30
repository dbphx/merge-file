# Merge PDF Workspace

Authenticated PDF merge app with:

- `React` frontend
- `Go` backend API
- `PostgreSQL` for users and job history
- `MinIO` for merged output storage
- Google Drive shared-folder ingest ordered by numeric prefixes in filenames

## Features

- Login with seeded email/password accounts
- Merge PDFs from a shared Google Drive folder
- Merge uploaded local PDFs with editable order
- Store merged outputs in MinIO
- View, download, and delete merge history per user
- Show progress percent and the current merge stage/file while a merge job is still running

## Local setup

1. Copy `.env.example` values into your shell or a local env file.
2. Start the full stack:

```bash
docker compose up -d --build
```

Postgres auto-runs the schema and seed scripts on first boot.
If your database volume already exists from an older version, apply the migrations manually:

```bash
psql postgres://mergepdf:mergepdf@localhost:5432/mergepdf -f migrations/002_add_job_progress.sql
psql postgres://mergepdf:mergepdf@localhost:5432/mergepdf -f migrations/003_add_job_runtime_state.sql
psql postgres://mergepdf:mergepdf@localhost:5432/mergepdf -f migrations/004_add_job_file_transfer_state.sql
psql postgres://mergepdf:mergepdf@localhost:5432/mergepdf -f migrations/005_add_job_file_source_object_key.sql
psql postgres://mergepdf:mergepdf@localhost:5432/mergepdf -f migrations/006_add_catalogs.sql
```

Services:

- Frontend: `http://localhost:4173`
- Backend API: `http://localhost:8080`
- MinIO console: `http://localhost:9001`
- Postgres: `localhost:5432`

If you need to rebuild from a clean database state, remove the persisted volumes first:

```bash
docker compose down -v
docker compose up -d --build
```

## Production via Cloudflare Tunnel

Use [docker-compose.prod.yml](docker-compose.prod.yml) when you want a single public app domain fronted by Cloudflare Tunnel.

The production stack is arranged like this:

- `cloudflared` exposes one hostname to Cloudflare
- `frontend` serves the SPA and proxies `/api/*` to `backend`
- `backend` talks to `postgres`, `minio`, and `redis` only on the internal Docker network
- no container ports are published to the host

1. Copy `.env.example` to a prod env file and set these values:

- `APP_URL=https://your-app-domain.example.com`
- `CLOUDFLARE_TUNNEL_TOKEN=...`
- `JWT_SECRET=...`
- `GOOGLE_DRIVE_API_KEY=...`
- `MINIO_ROOT_PASSWORD=...`
- `POSTGRES_PASSWORD=...`

2. Create a Cloudflare Tunnel in the Cloudflare dashboard and assign your public hostname to the tunnel. The tunnel token from that setup goes into `CLOUDFLARE_TUNNEL_TOKEN`.

3. Start the production stack:

Preferred:

```bash
./scripts/deploy_prod.sh up
```

Fallback:

```bash
docker compose -f docker-compose.prod.yml --env-file .env up -d --build
```

The deploy script will:

- start or rebuild the production compose stack
- wait for `postgres` to become healthy
- apply migrations `002` through `006`
- print the final service status

4. If you prefer to run without the script, and this is not a fresh Postgres volume, apply the migrations before using the app:

```bash
psql postgres://mergepdf:mergepdf@localhost:5432/mergepdf -f migrations/002_add_job_progress.sql
psql postgres://mergepdf:mergepdf@localhost:5432/mergepdf -f migrations/003_add_job_runtime_state.sql
psql postgres://mergepdf:mergepdf@localhost:5432/mergepdf -f migrations/004_add_job_file_transfer_state.sql
psql postgres://mergepdf:mergepdf@localhost:5432/mergepdf -f migrations/005_add_job_file_source_object_key.sql
psql postgres://mergepdf:mergepdf@localhost:5432/mergepdf -f migrations/006_add_catalogs.sql
```

Notes:

- frontend calls the API through `/api`, so only `APP_URL` needs to be public
- [frontend/nginx.prod.conf](frontend/nginx.prod.conf) is the reverse-proxy entrypoint used by the prod compose file
- [scripts/deploy_prod.sh](scripts/deploy_prod.sh) also supports `down`, `restart`, `logs`, and `ps`
- if you want a clean production database, remove the persisted volumes first:

```bash
docker compose -f docker-compose.prod.yml down -v
docker compose -f docker-compose.prod.yml --env-file .env up -d --build
```

## Required environment

- `JWT_SECRET`
- `DATABASE_URL`
- `MINIO_ENDPOINT`
- `MINIO_ACCESS_KEY`
- `MINIO_SECRET_KEY`
- `MINIO_BUCKET`
- `GOOGLE_DRIVE_API_KEY`
- `MAX_UPLOAD_SIZE` or legacy `MAX_UPLOAD_MB`

`GOOGLE_DRIVE_API_KEY` is required for shared/public Drive folder preview and download.
`MAX_UPLOAD_SIZE` defaults to `10G` in `docker-compose.yml`. Supported examples: `512M`, `1.5G`, `10G`, `2048`.

## Seed users

`scripts/seed_users.sql` creates:

- `admin@example.com`
- `user@example.com`

Default password for both: `ChangeMe123!`

## API summary

- `POST /api/auth/login`
- `POST /api/auth/logout`
- `GET /api/me`
- `POST /api/drive/preview`
- `POST /api/merge/drive`
- `POST /api/merge/upload`
- `GET /api/jobs`
- `GET /api/jobs/:id`
- `GET /api/jobs/:id/download`
- `DELETE /api/jobs/:id`

## Drive ordering

Drive merges only use the first integer found in each filename:

- `1-cover.pdf`
- `02-chapter.pdf`
- `10-appendix.pdf`

Files without a number in the name are rejected.
