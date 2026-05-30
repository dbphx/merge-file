package repository

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ml/merge-pdf/backend/internal/model"
)

type Repository struct {
	db *pgxpool.Pool
}

func assignNullableString(dest *string, value sql.NullString) {
	if value.Valid {
		*dest = value.String
		return
	}
	*dest = ""
}

func nullableStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	copy := value.String
	return &copy
}

func assignNullableInt(dest *int, value sql.NullInt32) {
	if value.Valid {
		*dest = int(value.Int32)
		return
	}
	*dest = 0
}

func assignNullableInt64(dest *int64, value sql.NullInt64) {
	if value.Valid {
		*dest = value.Int64
		return
	}
	*dest = 0
}

// New binds the repository to a connection pool so handlers can share one database access layer.
func New(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

// GetUserByEmail powers email-password login lookups against the canonical user record.
func (r *Repository) GetUserByEmail(ctx context.Context, email string) (model.User, error) {
	const query = `
		SELECT id, email, password_hash, role, created_at
		FROM users
		WHERE email = $1
	`

	var user model.User
	err := r.db.QueryRow(ctx, query, email).Scan(&user.ID, &user.Email, &user.PasswordHash, &user.Role, &user.CreatedAt)
	if err != nil {
		return model.User{}, err
	}
	return user, nil
}

// GetUserByID reloads the current user on each request so authorization uses fresh role data.
func (r *Repository) GetUserByID(ctx context.Context, id int64) (model.User, error) {
	const query = `
		SELECT id, email, password_hash, role, created_at
		FROM users
		WHERE id = $1
	`

	var user model.User
	err := r.db.QueryRow(ctx, query, id).Scan(&user.ID, &user.Email, &user.PasswordHash, &user.Role, &user.CreatedAt)
	if err != nil {
		return model.User{}, err
	}
	return user, nil
}

// CreateJob records a new merge job before background processing starts so the frontend can poll progress.
func (r *Repository) CreateJob(ctx context.Context, userID int64, sourceType model.SourceType, status model.JobStatus, progressPercent int, outputFilename string, files []model.JobFile, runtimeState model.JobRuntimeState) (model.Job, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return model.Job{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	const insertJob = `
			INSERT INTO jobs (
				user_id, source_type, status, progress_percent, output_filename,
				current_stage, current_file_name, current_file_index, total_files,
				current_file_bytes, current_file_size
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			RETURNING
				id, user_id, source_type, status, progress_percent, output_filename,
				current_stage, current_file_name, current_file_index, total_files,
				current_file_bytes, current_file_size,
				output_object_key, error_message, created_at
	`

	var job model.Job
	var outputObjectKey sql.NullString
	var errorMessage sql.NullString
	var currentStage sql.NullString
	var currentFileName sql.NullString
	var currentFileIndex sql.NullInt32
	var totalFiles sql.NullInt32
	var currentFileBytes sql.NullInt64
	var currentFileSize sql.NullInt64
	err = tx.QueryRow(
		ctx,
		insertJob,
		userID,
		sourceType,
		status,
		progressPercent,
		outputFilename,
		nullIfEmpty(runtimeState.CurrentStage),
		nullIfEmpty(runtimeState.CurrentFileName),
		runtimeState.CurrentFileIndex,
		runtimeState.TotalFiles,
		runtimeState.CurrentFileBytes,
		runtimeState.CurrentFileSize,
	).Scan(
		&job.ID,
		&job.UserID,
		&job.SourceType,
		&job.Status,
		&job.ProgressPercent,
		&job.OutputFilename,
		&currentStage,
		&currentFileName,
		&currentFileIndex,
		&totalFiles,
		&currentFileBytes,
		&currentFileSize,
		&outputObjectKey,
		&errorMessage,
		&job.CreatedAt,
	)
	if err != nil {
		return model.Job{}, fmt.Errorf("insert job: %w", err)
	}
	assignNullableString(&job.CurrentStage, currentStage)
	assignNullableString(&job.CurrentFileName, currentFileName)
	assignNullableInt(&job.CurrentFileIndex, currentFileIndex)
	assignNullableInt(&job.TotalFiles, totalFiles)
	assignNullableInt64(&job.CurrentFileBytes, currentFileBytes)
	assignNullableInt64(&job.CurrentFileSize, currentFileSize)
	assignNullableString(&job.OutputObjectKey, outputObjectKey)
	job.ErrorMessage = nullableStringPointer(errorMessage)

	const insertFile = `
		INSERT INTO job_files (job_id, source_kind, source_name, source_order, source_size, drive_file_id, drive_link, source_object_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, job_id, source_kind, source_name, source_order, source_size, drive_file_id, drive_link, source_object_key
	`

	job.Files = make([]model.JobFile, 0, len(files))
	for _, file := range files {
		var saved model.JobFile
		err = tx.QueryRow(ctx, insertFile, job.ID, file.SourceKind, file.SourceName, file.SourceOrder, file.SourceSize, file.DriveFileID, file.DriveLink, file.SourceObjectKey).
			Scan(&saved.ID, &saved.JobID, &saved.SourceKind, &saved.SourceName, &saved.SourceOrder, &saved.SourceSize, &saved.DriveFileID, &saved.DriveLink, &saved.SourceObjectKey)
		if err != nil {
			return model.Job{}, fmt.Errorf("insert job file: %w", err)
		}
		job.Files = append(job.Files, saved)
	}

	if err := tx.Commit(ctx); err != nil {
		return model.Job{}, fmt.Errorf("commit transaction: %w", err)
	}

	return job, nil
}

// UpdateJobProgress keeps the persisted progress visible to polling clients while a merge runs.
func (r *Repository) UpdateJobState(ctx context.Context, jobID int64, status model.JobStatus, progressPercent int, runtimeState model.JobRuntimeState) error {
	const query = `
		UPDATE jobs
		SET
			status = $2,
			progress_percent = $3,
			current_stage = $4,
			current_file_name = $5,
			current_file_index = $6,
			total_files = $7,
			current_file_bytes = $8,
			current_file_size = $9,
			error_message = NULL
		WHERE id = $1
	`

	if _, err := r.db.Exec(
		ctx,
		query,
		jobID,
		status,
		progressPercent,
		nullIfEmpty(runtimeState.CurrentStage),
		nullIfEmpty(runtimeState.CurrentFileName),
		runtimeState.CurrentFileIndex,
		runtimeState.TotalFiles,
		runtimeState.CurrentFileBytes,
		runtimeState.CurrentFileSize,
	); err != nil {
		return fmt.Errorf("update job state: %w", err)
	}
	return nil
}

// CompleteJob finalizes the stored output location once merge processing has uploaded the result.
func (r *Repository) CompleteJob(ctx context.Context, jobID int64, outputObjectKey string) error {
	const query = `
		UPDATE jobs
		SET
			status = $2,
			progress_percent = 100,
			current_stage = 'completed',
			current_file_name = NULL,
			current_file_index = 0,
			current_file_bytes = 0,
			current_file_size = 0,
			output_object_key = $3,
			error_message = NULL
		WHERE id = $1
	`

	if _, err := r.db.Exec(ctx, query, jobID, model.JobStatusCompleted, outputObjectKey); err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	return nil
}

// FailJob preserves the failure reason so waiting users can see why background processing stopped.
func (r *Repository) FailJob(ctx context.Context, jobID int64, progressPercent int, message string) error {
	const query = `
		UPDATE jobs
		SET
			status = $2,
			progress_percent = $3,
			current_stage = 'failed',
			current_file_bytes = 0,
			current_file_size = 0,
			error_message = $4
		WHERE id = $1
	`

	if _, err := r.db.Exec(ctx, query, jobID, model.JobStatusFailed, progressPercent, message); err != nil {
		return fmt.Errorf("fail job: %w", err)
	}
	return nil
}

// ListJobs returns either user-scoped history or admin-wide history from a single entrypoint.
func (r *Repository) ListJobs(ctx context.Context, actor model.User) ([]model.Job, error) {
	query := `
		SELECT
				id, user_id, source_type, status, progress_percent, output_filename,
				current_stage, current_file_name, current_file_index, total_files,
				current_file_bytes, current_file_size,
				output_object_key, error_message, created_at
			FROM jobs
	`
	args := []any{}
	if actor.Role != model.RoleAdmin {
		query += ` WHERE user_id = $1`
		args = append(args, actor.ID)
	}
	query += ` ORDER BY created_at DESC`

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	var jobs []model.Job
	for rows.Next() {
		var job model.Job
		var currentStage sql.NullString
		var currentFileName sql.NullString
		var currentFileIndex sql.NullInt32
		var totalFiles sql.NullInt32
		var currentFileBytes sql.NullInt64
		var currentFileSize sql.NullInt64
		var outputObjectKey sql.NullString
		var errorMessage sql.NullString
		if err := rows.Scan(
			&job.ID,
			&job.UserID,
			&job.SourceType,
			&job.Status,
			&job.ProgressPercent,
			&job.OutputFilename,
			&currentStage,
			&currentFileName,
			&currentFileIndex,
			&totalFiles,
			&currentFileBytes,
			&currentFileSize,
			&outputObjectKey,
			&errorMessage,
			&job.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		assignNullableString(&job.CurrentStage, currentStage)
		assignNullableString(&job.CurrentFileName, currentFileName)
		assignNullableInt(&job.CurrentFileIndex, currentFileIndex)
		assignNullableInt(&job.TotalFiles, totalFiles)
		assignNullableInt64(&job.CurrentFileBytes, currentFileBytes)
		assignNullableInt64(&job.CurrentFileSize, currentFileSize)
		assignNullableString(&job.OutputObjectKey, outputObjectKey)
		job.ErrorMessage = nullableStringPointer(errorMessage)
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (r *Repository) ListCatalogs(ctx context.Context, actor model.User) ([]model.Catalog, error) {
	query := `
		SELECT id, user_id, source_type, title, created_at
		FROM catalogs
	`
	args := []any{}
	if actor.Role != model.RoleAdmin {
		query += ` WHERE user_id = $1`
		args = append(args, actor.ID)
	}
	query += ` ORDER BY created_at DESC`

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list catalogs: %w", err)
	}
	defer rows.Close()

	var catalogs []model.Catalog
	for rows.Next() {
		var catalog model.Catalog
		if err := rows.Scan(
			&catalog.ID,
			&catalog.UserID,
			&catalog.SourceType,
			&catalog.Title,
			&catalog.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan catalog: %w", err)
		}
		catalogs = append(catalogs, catalog)
	}
	return catalogs, rows.Err()
}

// GetJob loads a single job with its ordered source file metadata for history detail views.
func (r *Repository) GetJob(ctx context.Context, id int64) (model.Job, error) {
	const jobQuery = `
		SELECT
			id, user_id, source_type, status, progress_percent, output_filename,
			current_stage, current_file_name, current_file_index, total_files,
			current_file_bytes, current_file_size,
			output_object_key, error_message, created_at
		FROM jobs
		WHERE id = $1
	`

	var job model.Job
	var currentStage sql.NullString
	var currentFileName sql.NullString
	var currentFileIndex sql.NullInt32
	var totalFiles sql.NullInt32
	var currentFileBytes sql.NullInt64
	var currentFileSize sql.NullInt64
	var outputObjectKey sql.NullString
	var errorMessage sql.NullString
	err := r.db.QueryRow(ctx, jobQuery, id).
		Scan(
			&job.ID,
			&job.UserID,
			&job.SourceType,
			&job.Status,
			&job.ProgressPercent,
			&job.OutputFilename,
			&currentStage,
			&currentFileName,
			&currentFileIndex,
			&totalFiles,
			&currentFileBytes,
			&currentFileSize,
			&outputObjectKey,
			&errorMessage,
			&job.CreatedAt,
		)
	if err != nil {
		return model.Job{}, err
	}
	assignNullableString(&job.CurrentStage, currentStage)
	assignNullableString(&job.CurrentFileName, currentFileName)
	assignNullableInt(&job.CurrentFileIndex, currentFileIndex)
	assignNullableInt(&job.TotalFiles, totalFiles)
	assignNullableInt64(&job.CurrentFileBytes, currentFileBytes)
	assignNullableInt64(&job.CurrentFileSize, currentFileSize)
	assignNullableString(&job.OutputObjectKey, outputObjectKey)
	job.ErrorMessage = nullableStringPointer(errorMessage)

	const filesQuery = `
		SELECT id, job_id, source_kind, source_name, source_order, source_size, drive_file_id, drive_link, source_object_key
		FROM job_files
		WHERE job_id = $1
		ORDER BY source_order ASC, source_name ASC
	`

	rows, err := r.db.Query(ctx, filesQuery, id)
	if err != nil {
		return model.Job{}, fmt.Errorf("query job files: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var file model.JobFile
		if err := rows.Scan(&file.ID, &file.JobID, &file.SourceKind, &file.SourceName, &file.SourceOrder, &file.SourceSize, &file.DriveFileID, &file.DriveLink, &file.SourceObjectKey); err != nil {
			return model.Job{}, fmt.Errorf("scan job file: %w", err)
		}
		job.Files = append(job.Files, file)
	}

	return job, rows.Err()
}

// DeleteJob removes a history record and its child file metadata together to avoid orphan rows.
func (r *Repository) DeleteJob(ctx context.Context, id int64) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin delete transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM job_files WHERE job_id = $1`, id); err != nil {
		return fmt.Errorf("delete job files: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM jobs WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete job: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit delete transaction: %w", err)
	}
	return nil
}

func (r *Repository) UpdateJobFileSourceObjectKey(ctx context.Context, jobFileID int64, objectKey string) error {
	const query = `
		UPDATE job_files
		SET source_object_key = $2
		WHERE id = $1
	`

	if _, err := r.db.Exec(ctx, query, jobFileID, objectKey); err != nil {
		return fmt.Errorf("update job file source object key: %w", err)
	}
	return nil
}

func (r *Repository) CreateCatalog(ctx context.Context, userID int64, sourceType model.SourceType, title string, pages []model.CatalogPage) (model.Catalog, error) {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return model.Catalog{}, fmt.Errorf("begin catalog transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	const insertCatalog = `
		INSERT INTO catalogs (user_id, source_type, title)
		VALUES ($1, $2, $3)
		RETURNING id, user_id, source_type, title, created_at
	`

	var catalog model.Catalog
	if err := tx.QueryRow(ctx, insertCatalog, userID, sourceType, title).Scan(
		&catalog.ID,
		&catalog.UserID,
		&catalog.SourceType,
		&catalog.Title,
		&catalog.CreatedAt,
	); err != nil {
		return model.Catalog{}, fmt.Errorf("insert catalog: %w", err)
	}

	const insertPage = `
		INSERT INTO catalog_pages (catalog_id, source_kind, source_name, source_order, source_size, drive_file_id, source_object_key, mime_type)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, catalog_id, source_kind, source_name, source_order, source_size, drive_file_id, source_object_key, mime_type
	`

	catalog.Pages = make([]model.CatalogPage, 0, len(pages))
	for _, page := range pages {
		var saved model.CatalogPage
		if err := tx.QueryRow(
			ctx,
			insertPage,
			catalog.ID,
			page.SourceKind,
			page.SourceName,
			page.SourceOrder,
			page.SourceSize,
			page.DriveFileID,
			page.SourceObjectKey,
			page.MimeType,
		).Scan(
			&saved.ID,
			&saved.CatalogID,
			&saved.SourceKind,
			&saved.SourceName,
			&saved.SourceOrder,
			&saved.SourceSize,
			&saved.DriveFileID,
			&saved.SourceObjectKey,
			&saved.MimeType,
		); err != nil {
			return model.Catalog{}, fmt.Errorf("insert catalog page: %w", err)
		}
		catalog.Pages = append(catalog.Pages, saved)
	}

	if err := tx.Commit(ctx); err != nil {
		return model.Catalog{}, fmt.Errorf("commit catalog transaction: %w", err)
	}
	return catalog, nil
}

func (r *Repository) AddCatalogPage(ctx context.Context, catalogID int64, page model.CatalogPage) (model.CatalogPage, error) {
	const insertPage = `
		INSERT INTO catalog_pages (catalog_id, source_kind, source_name, source_order, source_size, drive_file_id, source_object_key, mime_type)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, catalog_id, source_kind, source_name, source_order, source_size, drive_file_id, source_object_key, mime_type
	`

	var saved model.CatalogPage
	if err := r.db.QueryRow(
		ctx,
		insertPage,
		catalogID,
		page.SourceKind,
		page.SourceName,
		page.SourceOrder,
		page.SourceSize,
		page.DriveFileID,
		page.SourceObjectKey,
		page.MimeType,
	).Scan(
		&saved.ID,
		&saved.CatalogID,
		&saved.SourceKind,
		&saved.SourceName,
		&saved.SourceOrder,
		&saved.SourceSize,
		&saved.DriveFileID,
		&saved.SourceObjectKey,
		&saved.MimeType,
	); err != nil {
		return model.CatalogPage{}, fmt.Errorf("insert catalog page: %w", err)
	}

	return saved, nil
}

func (r *Repository) GetCatalog(ctx context.Context, id int64) (model.Catalog, error) {
	const catalogQuery = `
		SELECT id, user_id, source_type, title, created_at
		FROM catalogs
		WHERE id = $1
	`

	var catalog model.Catalog
	if err := r.db.QueryRow(ctx, catalogQuery, id).Scan(
		&catalog.ID,
		&catalog.UserID,
		&catalog.SourceType,
		&catalog.Title,
		&catalog.CreatedAt,
	); err != nil {
		return model.Catalog{}, err
	}

	const pagesQuery = `
		SELECT id, catalog_id, source_kind, source_name, source_order, source_size, drive_file_id, source_object_key, mime_type
		FROM catalog_pages
		WHERE catalog_id = $1
		ORDER BY source_order ASC, source_name ASC
	`

	rows, err := r.db.Query(ctx, pagesQuery, id)
	if err != nil {
		return model.Catalog{}, fmt.Errorf("query catalog pages: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var page model.CatalogPage
		if err := rows.Scan(
			&page.ID,
			&page.CatalogID,
			&page.SourceKind,
			&page.SourceName,
			&page.SourceOrder,
			&page.SourceSize,
			&page.DriveFileID,
			&page.SourceObjectKey,
			&page.MimeType,
		); err != nil {
			return model.Catalog{}, fmt.Errorf("scan catalog page: %w", err)
		}
		catalog.Pages = append(catalog.Pages, page)
	}

	return catalog, rows.Err()
}


func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
