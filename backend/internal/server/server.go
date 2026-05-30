package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ml/merge-pdf/backend/internal/auth"
	"github.com/ml/merge-pdf/backend/internal/cache"
	"github.com/ml/merge-pdf/backend/internal/config"
	"github.com/ml/merge-pdf/backend/internal/drive"
	"github.com/ml/merge-pdf/backend/internal/merge"
	"github.com/ml/merge-pdf/backend/internal/model"
	"github.com/ml/merge-pdf/backend/internal/repository"
	"github.com/ml/merge-pdf/backend/internal/storage"
)

type Server struct {
	cfg             config.Config
	repo            *repository.Repository
	auth            auth.Service
	drive           drive.Client
	storage         *storage.Client
	cache           *cache.Client
	httpServer      *http.Server
	maxUploadMB     int64
	allowedOrigin   string
	downloadStateMu sync.RWMutex
	downloadState   map[int64]*jobDownloadState
}

type authContextKey struct{}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type drivePreviewRequest struct {
	URL string `json:"url"`
}

type driveMergeRequest struct {
	URL    string         `json:"url"`
	Orders map[string]int `json:"orders"`
}

type driveCatalogRequest struct {
	URL    string         `json:"url"`
	Orders map[string]int `json:"orders"`
}

type jobDownloadState struct {
	totalBytes          int64
	fileBytes           map[string]int64
	fileStatus          map[string]string
	lastReportedPercent int
	lastReportedAt      time.Time
}

const (
	jobStageQueued           = "queued"
	jobStageDownloading      = "downloading"
	jobStageMerging          = "merging"
	jobStageUploading        = "uploading"
	catalogPageCacheMaxBytes = 10 * 1024 * 1024
)

// New wires the API surface once so every request path shares the same auth, storage, and timeout policy.
func New(cfg config.Config, repo *repository.Repository, authSvc auth.Service, driveClient drive.Client, storageClient *storage.Client, cacheClient *cache.Client) *Server {
	writeTimeout := cfg.RequestTimeout
	if writeTimeout < 30*time.Minute {
		writeTimeout = 30 * time.Minute
	}

	s := &Server{
		cfg:           cfg,
		repo:          repo,
		auth:          authSvc,
		drive:         driveClient,
		storage:       storageClient,
		cache:         cacheClient,
		maxUploadMB:   cfg.MaxUploadBytes,
		allowedOrigin: cfg.AllowedOrigin,
		downloadState: make(map[int64]*jobDownloadState),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", s.handleLogin)
	mux.HandleFunc("/api/auth/logout", s.handleLogout)
	mux.Handle("/api/me", s.withAuth(http.HandlerFunc(s.handleMe)))
	mux.Handle("/api/drive/preview", s.withAuth(http.HandlerFunc(s.handleDrivePreview)))
	mux.Handle("/api/merge/drive", s.withAuth(http.HandlerFunc(s.handleDriveMerge)))
	mux.Handle("/api/merge/upload", s.withAuth(http.HandlerFunc(s.handleUploadMerge)))
	mux.Handle("/api/catalogs/drive", s.withAuth(http.HandlerFunc(s.handleDriveCatalog)))
	mux.Handle("/api/catalogs/upload", s.withAuth(http.HandlerFunc(s.handleUploadCatalog)))
	mux.Handle("/api/catalogs/", s.withAuth(http.HandlerFunc(s.handleCatalogByID)))
	mux.Handle("/api/jobs", s.withAuth(http.HandlerFunc(s.handleJobs)))
	mux.Handle("/api/jobs/", s.withAuth(http.HandlerFunc(s.handleJobByID)))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	s.httpServer = &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      s.withCORS(s.withLogging(mux)),
		ReadTimeout:  cfg.RequestTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  30 * time.Second,
	}

	return s
}

// Start owns the HTTP listener lifecycle for local dev and deployed environments.
func (s *Server) Start() error {
	log.Printf("listening on %s", s.httpServer.Addr)
	return s.httpServer.ListenAndServe()
}

// Shutdown gives in-flight merge requests a brief drain window during process termination.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", s.allowedOrigin)
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}

		claims, err := s.auth.ParseToken(strings.TrimPrefix(header, "Bearer "))
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}

		user, err := s.repo.GetUserByID(r.Context(), claims.UserID)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "user not found")
			return
		}

		ctx := context.WithValue(r.Context(), authContextKey{}, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid login payload")
		return
	}

	user, err := s.repo.GetUserByEmail(r.Context(), strings.ToLower(strings.TrimSpace(req.Email)))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load user")
		return
	}

	if err := s.auth.CheckPassword(user.PasswordHash, req.Password); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	token, err := s.auth.GenerateToken(user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create token")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"token": token,
		"user": map[string]any{
			"id":    user.ID,
			"email": user.Email,
			"role":  user.Role,
		},
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	user := currentUser(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"id":    user.ID,
		"email": user.Email,
		"role":  user.Role,
	})
}

func (s *Server) handleDrivePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req drivePreviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid drive preview payload")
		return
	}

	files, err := s.drive.PreviewFolder(r.Context(), req.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	log.Printf("drive_preview folder_url=%q file_count=%d", req.URL, len(files))
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (s *Server) handleDriveMerge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req driveMergeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid drive merge payload")
		return
	}

	files, err := s.drive.PreviewFolder(r.Context(), req.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := applyDriveOrders(files, req.Orders); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	jobFiles := make([]model.JobFile, 0, len(files))
	for _, file := range files {
		sizeCopy := file.Size
		sourceID := file.SourceID
		driveLink := file.WebViewLink
		jobFiles = append(jobFiles, model.JobFile{
			SourceKind:  string(model.SourceTypeDrive),
			SourceName:  file.Name,
			SourceOrder: file.ExtractedOrder,
			SourceSize:  &sizeCopy,
			DriveFileID: &sourceID,
			DriveLink:   &driveLink,
		})
	}
	outputName := buildOutputFilename("drive-merged")

	job, err := s.repo.CreateJob(
		r.Context(),
		currentUser(r.Context()).ID,
		model.SourceTypeDrive,
		model.JobStatusPending,
		5,
		outputName,
		jobFiles,
		model.JobRuntimeState{
			CurrentStage: jobStageQueued,
			TotalFiles:   len(files),
		},
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create job")
		return
	}
	attachDriveJobFileMetadata(files, job.Files)

	log.Printf("job=%d source=drive event=queued total_files=%d output=%q", job.ID, len(files), job.OutputFilename)
	go s.processDriveMerge(job.ID, outputName, files)
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) processDriveMerge(jobID int64, outputName string, files []model.DrivePreviewFile) {
	log.Printf("job=%d source=drive event=started total_files=%d", jobID, len(files))
	workDir, err := os.MkdirTemp("", "mergepdf-drive-*")
	if err != nil {
		s.failJob(jobID, 5, "failed to create work dir")
		return
	}
	defer os.RemoveAll(workDir)
	defer s.clearDownloadState(jobID)
	inputs := make([]model.MergeFileInput, len(files))
	ctx := context.Background()
	s.initDownloadState(jobID, files)
	_ = s.repo.UpdateJobState(ctx, jobID, model.JobStatusRunning, 10, model.JobRuntimeState{
		CurrentStage: jobStageDownloading,
		TotalFiles:   len(files),
	})
	if err := s.downloadDriveInputs(ctx, jobID, workDir, files, inputs); err != nil {
		s.failJob(jobID, 10, err.Error())
		return
	}

	s.finishMergeJob(ctx, jobID, workDir, outputName, inputs)
}

func (s *Server) handleUploadMerge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.maxUploadMB)
	if err := r.ParseMultipartForm(s.maxUploadMB); err != nil {
		log.Printf("upload_parse_failed limit_bytes=%d err=%v", s.maxUploadMB, err)
		if strings.Contains(err.Error(), "request body too large") {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("upload exceeds %s limit", humanBytes(s.maxUploadMB)))
			return
		}
		writeError(w, http.StatusBadRequest, "failed to parse upload form")
		return
	}

	orderPayload := r.FormValue("orders")
	var orders map[string]int
	if err := json.Unmarshal([]byte(orderPayload), &orders); err != nil {
		writeError(w, http.StatusBadRequest, "invalid orders payload")
		return
	}

	workDir, err := os.MkdirTemp("", "mergepdf-upload-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create work dir")
		return
	}
	shouldCleanupWorkDir := true
	defer func() {
		if shouldCleanupWorkDir {
			os.RemoveAll(workDir)
		}
	}()

	multipartFiles := r.MultipartForm.File["files"]
	inputs := make([]model.MergeFileInput, 0, len(multipartFiles))
	jobFiles := make([]model.JobFile, 0, len(multipartFiles))
	for _, header := range multipartFiles {
		order, ok := orders[header.Filename]
		if !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("missing order for %s", header.Filename))
			return
		}
		if !merge.SupportsUploadFile(header.Filename) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be a PDF or PNG", header.Filename))
			return
		}

		sourcePath, size, err := saveMultipartFile(workDir, header)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to save %s", header.Filename))
			return
		}
		localPath, err := merge.NormalizeUploadInput(workDir, sourcePath)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to prepare %s", header.Filename))
			return
		}

		inputs = append(inputs, model.MergeFileInput{
			Name:      header.Filename,
			LocalPath: localPath,
			Order:     order,
			Size:      size,
		})

		sizeCopy := size
		jobFiles = append(jobFiles, model.JobFile{
			SourceKind:  string(model.SourceTypeUpload),
			SourceName:  header.Filename,
			SourceOrder: order,
			SourceSize:  &sizeCopy,
		})
	}
	outputName := buildOutputFilename("upload-merged")

	job, err := s.repo.CreateJob(
		r.Context(),
		currentUser(r.Context()).ID,
		model.SourceTypeUpload,
		model.JobStatusPending,
		25,
		outputName,
		jobFiles,
		model.JobRuntimeState{
			CurrentStage: jobStageQueued,
			TotalFiles:   len(inputs),
		},
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create job")
		return
	}

	log.Printf("job=%d source=upload event=queued total_files=%d output=%q", job.ID, len(inputs), job.OutputFilename)
	shouldCleanupWorkDir = false
	go s.processUploadMerge(job.ID, workDir, outputName, inputs)
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleDriveCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req driveCatalogRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	files, err := s.drive.PreviewFolder(r.Context(), strings.TrimSpace(req.URL))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := applyDriveOrders(files, req.Orders); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	userID := currentUser(r.Context()).ID
	pages := make([]model.CatalogPage, 0, len(files))
	for index, file := range files {
		reader, err := s.drive.DownloadFile(r.Context(), file.SourceID)
		if err != nil {
			log.Printf("catalog_drive_source_download_failed user_id=%d file_index=%d file_name=%q drive_file_id=%q err=%v", userID, index+1, file.Name, file.SourceID, err)
			writeError(w, http.StatusInternalServerError, "failed to ingest drive catalog source")
			return
		}

		objectKey := buildCatalogDriveObjectKey(userID, file)
		if err := s.storage.UploadObject(r.Context(), objectKey, reader, file.Size, detectContentType(file.Name)); err != nil {
			reader.Close()
			log.Printf("catalog_drive_source_upload_failed user_id=%d file_index=%d file_name=%q object_key=%q err=%v", userID, index+1, file.Name, objectKey, err)
			writeError(w, http.StatusInternalServerError, "failed to ingest drive catalog source")
			return
		}
		reader.Close()
		log.Printf("catalog_drive_source_ingested user_id=%d file_index=%d file_name=%q object_key=%q size=%d", userID, index+1, file.Name, objectKey, file.Size)

		sizeCopy := file.Size
		sourceID := file.SourceID
		objectKeyCopy := objectKey
		pages = append(pages, model.CatalogPage{
			SourceKind:      string(model.SourceTypeDrive),
			SourceName:      file.Name,
			SourceOrder:     file.ExtractedOrder,
			SourceSize:      &sizeCopy,
			DriveFileID:     &sourceID,
			SourceObjectKey: &objectKeyCopy,
			MimeType:        detectContentType(file.Name),
		})
	}

	catalog, err := s.repo.CreateCatalog(r.Context(), userID, model.SourceTypeDrive, buildCatalogTitle("drive-catalog"), pages)
	if err != nil {
		log.Printf("catalog_create_failed source=drive user_id=%d err=%v", userID, err)
		writeError(w, http.StatusInternalServerError, "failed to create catalog")
		return
	}
	s.cacheCatalogDetail(r.Context(), catalog)

	writeJSON(w, http.StatusCreated, catalog)
}

func (s *Server) handleUploadCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.maxUploadMB)
	if err := r.ParseMultipartForm(s.maxUploadMB); err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("upload exceeds %s limit", humanBytes(s.maxUploadMB)))
			return
		}
		writeError(w, http.StatusBadRequest, "failed to parse upload form")
		return
	}

	orderPayload := r.FormValue("orders")
	var orders map[string]int
	if err := json.Unmarshal([]byte(orderPayload), &orders); err != nil {
		writeError(w, http.StatusBadRequest, "invalid orders payload")
		return
	}

	workDir, err := os.MkdirTemp("", "catalog-upload-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create work dir")
		return
	}
	defer os.RemoveAll(workDir)

	multipartFiles := r.MultipartForm.File["files"]
	pages := make([]model.CatalogPage, 0, len(multipartFiles))
	for _, header := range multipartFiles {
		order, ok := orders[header.Filename]
		if !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("missing order for %s", header.Filename))
			return
		}
		if !merge.SupportsUploadFile(header.Filename) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be a PDF or PNG", header.Filename))
			return
		}

		localPath, size, err := saveMultipartFile(workDir, header)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to save %s", header.Filename))
			return
		}

		objectKey := buildCatalogUploadObjectKey(currentUser(r.Context()).ID, header.Filename)
		file, err := os.Open(localPath)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to reopen %s", header.Filename))
			return
		}
		if err := s.storage.UploadObject(r.Context(), objectKey, file, size, detectContentType(header.Filename)); err != nil {
			file.Close()
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed to upload %s", header.Filename))
			return
		}
		file.Close()

		sizeCopy := size
		objectKeyCopy := objectKey
		pages = append(pages, model.CatalogPage{
			SourceKind:      string(model.SourceTypeUpload),
			SourceName:      header.Filename,
			SourceOrder:     order,
			SourceSize:      &sizeCopy,
			SourceObjectKey: &objectKeyCopy,
			MimeType:        detectContentType(header.Filename),
		})
	}

	catalog, err := s.repo.CreateCatalog(r.Context(), currentUser(r.Context()).ID, model.SourceTypeUpload, buildCatalogTitle("upload-catalog"), pages)
	if err != nil {
		log.Printf("catalog_create_failed source=upload user_id=%d err=%v", currentUser(r.Context()).ID, err)
		writeError(w, http.StatusInternalServerError, "failed to create catalog")
		return
	}
	s.cacheCatalogDetail(r.Context(), catalog)

	writeJSON(w, http.StatusCreated, catalog)
}

func (s *Server) processUploadMerge(jobID int64, workDir, outputName string, inputs []model.MergeFileInput) {
	log.Printf("job=%d source=upload event=started total_files=%d", jobID, len(inputs))
	defer os.RemoveAll(workDir)
	ctx := context.Background()
	_ = s.repo.UpdateJobState(ctx, jobID, model.JobStatusRunning, 40, model.JobRuntimeState{
		CurrentStage: jobStageMerging,
		TotalFiles:   len(inputs),
	})
	s.finishMergeJob(ctx, jobID, workDir, outputName, inputs)
}

func (s *Server) downloadDriveInputs(ctx context.Context, jobID int64, workDir string, files []model.DrivePreviewFile, inputs []model.MergeFileInput) error {
	const workerCount = 3
	const maxAttempts = 3

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	workCh := make(chan int)
	errCh := make(chan error, 1)
	var wg sync.WaitGroup

	worker := func() {
		defer wg.Done()
		for index := range workCh {
			file := files[index]
			input, err := s.downloadSingleDriveFile(ctx, jobID, workDir, index, len(files), file, maxAttempts)
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				cancel()
				return
			}
			inputs[index] = input
		}
	}

	for workerIndex := 0; workerIndex < minInt(workerCount, len(files)); workerIndex++ {
		wg.Add(1)
		go worker()
	}

feedLoop:
	for index := range files {
		select {
		case <-ctx.Done():
			break feedLoop
		case workCh <- index:
		}
	}
	close(workCh)
	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func (s *Server) downloadSingleDriveFile(ctx context.Context, jobID int64, workDir string, index, totalFiles int, file model.DrivePreviewFile, maxAttempts int) (model.MergeFileInput, error) {
	if file.SourceObjectKey != "" {
		input, err := s.restoreDriveFileFromCache(ctx, jobID, workDir, index, totalFiles, file)
		if err == nil {
			return input, nil
		}
		log.Printf("job=%d source=drive stage=downloading event=cache_miss file_index=%d file_name=%q object_key=%q err=%v", jobID, index+1, file.Name, file.SourceObjectKey, err)
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return model.MergeFileInput{}, err
		}

		log.Printf(
			"job=%d source=drive stage=downloading event=file_start file_index=%d total_files=%d file_name=%q file_size=%d attempt=%d/%d",
			jobID,
			index+1,
			totalFiles,
			file.Name,
			file.Size,
			attempt,
			maxAttempts,
		)
		s.updateDownloadState(jobID, file, index, totalFiles, 0, "downloading")

		reader, err := s.drive.DownloadFile(ctx, file.SourceID)
		if err != nil {
			log.Printf("job=%d source=drive stage=downloading event=file_retry file_index=%d file_name=%q attempt=%d/%d err=%v", jobID, index+1, file.Name, attempt, maxAttempts, err)
			if attempt == maxAttempts {
				return model.MergeFileInput{}, fmt.Errorf("download %s: %w", file.Name, err)
			}
			time.Sleep(time.Duration(attempt) * 400 * time.Millisecond)
			continue
		}

		localPath := filepath.Join(workDir, file.Name)
		size, saveErr := saveUploadedReaderWithProgress(localPath, reader, func(written int64) {
			s.updateDownloadState(jobID, file, index, totalFiles, written, "downloading")
		})
		reader.Close()
		if saveErr != nil {
			log.Printf("job=%d source=drive stage=downloading event=file_retry file_index=%d file_name=%q attempt=%d/%d err=%v", jobID, index+1, file.Name, attempt, maxAttempts, saveErr)
			s.updateDownloadState(jobID, file, index, totalFiles, 0, "pending")
			if attempt == maxAttempts {
				return model.MergeFileInput{}, fmt.Errorf("save %s: %w", file.Name, saveErr)
			}
			time.Sleep(time.Duration(attempt) * 400 * time.Millisecond)
			continue
		}

		log.Printf(
			"job=%d source=drive stage=downloading event=file_complete file_index=%d total_files=%d file_name=%q bytes=%d attempt=%d/%d",
			jobID,
			index+1,
			totalFiles,
			file.Name,
			size,
			attempt,
			maxAttempts,
		)
		s.updateDownloadState(jobID, file, index, totalFiles, size, "downloaded")
		if file.SourceObjectKey == "" && file.JobFileID > 0 {
			objectKey := buildSourceCacheObjectKey(jobID, file)
			if err := s.uploadDriveSourceCache(ctx, file.JobFileID, objectKey, localPath, size); err != nil {
				log.Printf("job=%d source=drive stage=downloading event=cache_store_failed file_index=%d file_name=%q err=%v", jobID, index+1, file.Name, err)
			} else {
				file.SourceObjectKey = objectKey
			}
		}
		mergePath, err := merge.NormalizeUploadInput(workDir, localPath)
		if err != nil {
			return model.MergeFileInput{}, fmt.Errorf("prepare %s for merge: %w", file.Name, err)
		}

		return model.MergeFileInput{
			Name:      file.Name,
			LocalPath: mergePath,
			Order:     file.ExtractedOrder,
			Size:      size,
			SourceID:  file.SourceID,
			DriveLink: file.WebViewLink,
		}, nil
	}

	return model.MergeFileInput{}, fmt.Errorf("download %s: exhausted retries", file.Name)
}

func (s *Server) restoreDriveFileFromCache(ctx context.Context, jobID int64, workDir string, index, totalFiles int, file model.DrivePreviewFile) (model.MergeFileInput, error) {
	object, err := s.storage.Download(ctx, file.SourceObjectKey)
	if err != nil {
		return model.MergeFileInput{}, err
	}
	defer object.Close()

	localPath := filepath.Join(workDir, file.Name)
	size, err := saveUploadedReaderWithProgress(localPath, object, func(written int64) {
		s.updateDownloadState(jobID, file, index, totalFiles, written, "downloading")
	})
	if err != nil {
		return model.MergeFileInput{}, err
	}
	log.Printf(
		"job=%d source=drive stage=downloading event=file_complete file_index=%d total_files=%d file_name=%q bytes=%d source=cache",
		jobID,
		index+1,
		totalFiles,
		file.Name,
		size,
	)
	s.updateDownloadState(jobID, file, index, totalFiles, size, "downloaded")
	mergePath, err := merge.NormalizeUploadInput(workDir, localPath)
	if err != nil {
		return model.MergeFileInput{}, fmt.Errorf("prepare %s for merge: %w", file.Name, err)
	}
	return model.MergeFileInput{
		Name:      file.Name,
		LocalPath: mergePath,
		Order:     file.ExtractedOrder,
		Size:      size,
		SourceID:  file.SourceID,
		DriveLink: file.WebViewLink,
	}, nil
}

func (s *Server) uploadDriveSourceCache(ctx context.Context, jobFileID int64, objectKey, localPath string, size int64) error {
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open local cache source: %w", err)
	}
	defer file.Close()

	if err := s.storage.UploadObject(ctx, objectKey, file, size, detectContentType(localPath)); err != nil {
		return err
	}
	if err := s.repo.UpdateJobFileSourceObjectKey(ctx, jobFileID, objectKey); err != nil {
		return err
	}
	return nil
}

func (s *Server) finishMergeJob(ctx context.Context, jobID int64, workDir, outputName string, inputs []model.MergeFileInput) {
	log.Printf("job=%d stage=merging event=start total_files=%d output=%q", jobID, len(inputs), outputName)
	_ = s.repo.UpdateJobState(ctx, jobID, model.JobStatusRunning, 75, model.JobRuntimeState{
		CurrentStage: jobStageMerging,
		TotalFiles:   len(inputs),
	})
	outputPath, err := s.runMergeSubprocess(workDir, outputName, inputs)
	if err != nil {
		s.failJob(jobID, 75, err.Error())
		return
	}
	log.Printf("job=%d stage=merging event=complete output_path=%q", jobID, outputPath)

	file, err := os.Open(outputPath)
	if err != nil {
		s.failJob(jobID, 80, "failed to open merged output")
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		s.failJob(jobID, 80, "failed to stat merged output")
		return
	}

	job, err := s.repo.GetJob(ctx, jobID)
	if err != nil {
		s.failJob(jobID, 80, "failed to reload job")
		return
	}

	objectKey := fmt.Sprintf("jobs/%d/%d-%s", job.UserID, time.Now().UnixNano(), outputName)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		s.failJob(jobID, 85, "failed to rewind merged output")
		return
	}
	log.Printf("job=%d stage=uploading event=start object_key=%q", jobID, objectKey)
	_ = s.repo.UpdateJobState(ctx, jobID, model.JobStatusRunning, 90, model.JobRuntimeState{
		CurrentStage: jobStageUploading,
		TotalFiles:   len(inputs),
	})
	if err := s.storage.Upload(ctx, objectKey, file, info.Size()); err != nil {
		s.failJob(jobID, 90, "failed to upload merged output")
		return
	}
	log.Printf("job=%d stage=uploading event=complete object_key=%q size=%d", jobID, objectKey, info.Size())
	if err := s.repo.CompleteJob(ctx, jobID, objectKey); err != nil {
		s.failJob(jobID, 95, "failed to finalize job history")
		return
	}
	log.Printf("job=%d event=completed object_key=%q", jobID, objectKey)
}

func (s *Server) runMergeSubprocess(workDir, outputName string, inputs []model.MergeFileInput) (string, error) {
	inputsPayload, err := json.Marshal(inputs)
	if err != nil {
		return "", fmt.Errorf("encode merge inputs: %w", err)
	}

	manifestPath := filepath.Join(workDir, "merge-inputs.json")
	if err := os.WriteFile(manifestPath, inputsPayload, 0o600); err != nil {
		return "", fmt.Errorf("write merge manifest: %w", err)
	}

	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve server executable: %w", err)
	}

	cmd := exec.Command(exePath, "merge-worker", manifestPath, workDir, outputName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		details := strings.TrimSpace(string(output))
		if details != "" {
			return "", fmt.Errorf("merge worker failed: %w: %s", err, details)
		}
		return "", fmt.Errorf("merge worker failed: %w", err)
	}

	return filepath.Join(workDir, outputName), nil
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	jobs, err := s.repo.ListJobs(r.Context(), currentUser(r.Context()))
	if err != nil {
		log.Printf("jobs_list_failed user_id=%d err=%v", currentUser(r.Context()).ID, err)
		writeError(w, http.StatusInternalServerError, "failed to list jobs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (s *Server) handleCatalogByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/catalogs/")
	if path == "" {
		writeError(w, http.StatusNotFound, "catalog route not found")
		return
	}

	if strings.Contains(path, "/pages/") && strings.HasSuffix(path, "/content") {
		parts := strings.Split(strings.Trim(path, "/"), "/")
		if len(parts) != 4 || parts[1] != "pages" || parts[3] != "content" {
			writeError(w, http.StatusNotFound, "catalog page route not found")
			return
		}
		catalogID, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid catalog id")
			return
		}
		pageID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid catalog page id")
			return
		}
		s.handleCatalogPageContent(w, r, catalogID, pageID)
		return
	}

	catalogID, err := strconv.ParseInt(strings.TrimSuffix(path, "/"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid catalog id")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleCatalogDetail(w, r, catalogID)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleCatalogDetail(w http.ResponseWriter, r *http.Request, catalogID int64) {
	if catalog, ok := s.getCachedCatalogDetail(r.Context(), catalogID); ok {
		if auth.CanAccessJob(currentUser(r.Context()), catalog.UserID) {
			log.Printf("catalog_detail_cache_hit catalog_id=%d user_id=%d page_count=%d", catalogID, currentUser(r.Context()).ID, len(catalog.Pages))
			writeJSON(w, http.StatusOK, catalog)
			return
		}
	}

	catalog, err := s.repo.GetCatalog(r.Context(), catalogID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			log.Printf("catalog_detail_not_found catalog_id=%d user_id=%d", catalogID, currentUser(r.Context()).ID)
			writeError(w, http.StatusNotFound, "catalog not found")
			return
		}
		log.Printf("catalog_detail_load_failed catalog_id=%d user_id=%d err=%v", catalogID, currentUser(r.Context()).ID, err)
		writeError(w, http.StatusInternalServerError, "failed to load catalog")
		return
	}
	if !auth.CanAccessJob(currentUser(r.Context()), catalog.UserID) {
		log.Printf("catalog_detail_forbidden catalog_id=%d actor_user_id=%d owner_user_id=%d", catalogID, currentUser(r.Context()).ID, catalog.UserID)
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	s.cacheCatalogDetail(r.Context(), catalog)
	log.Printf("catalog_detail_loaded catalog_id=%d user_id=%d page_count=%d source_type=%s", catalogID, currentUser(r.Context()).ID, len(catalog.Pages), catalog.SourceType)

	writeJSON(w, http.StatusOK, catalog)
}

func (s *Server) handleCatalogPageContent(w http.ResponseWriter, r *http.Request, catalogID, pageID int64) {
	catalog, err := s.repo.GetCatalog(r.Context(), catalogID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			log.Printf("catalog_page_catalog_not_found catalog_id=%d page_id=%d user_id=%d", catalogID, pageID, currentUser(r.Context()).ID)
			writeError(w, http.StatusNotFound, "catalog not found")
			return
		}
		log.Printf("catalog_page_catalog_load_failed catalog_id=%d page_id=%d user_id=%d err=%v", catalogID, pageID, currentUser(r.Context()).ID, err)
		writeError(w, http.StatusInternalServerError, "failed to load catalog")
		return
	}
	if !auth.CanAccessJob(currentUser(r.Context()), catalog.UserID) {
		log.Printf("catalog_page_forbidden catalog_id=%d page_id=%d actor_user_id=%d owner_user_id=%d", catalogID, pageID, currentUser(r.Context()).ID, catalog.UserID)
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}

	for _, page := range catalog.Pages {
		if page.ID != pageID {
			continue
		}
		w.Header().Set("Content-Type", page.MimeType)
		w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", page.SourceName))

		if payload, ok := s.getCachedCatalogPageContent(r.Context(), catalogID, page); ok {
			log.Printf("catalog_page_cache_hit catalog_id=%d page_id=%d bytes=%d", catalogID, page.ID, len(payload))
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)
			return
		}

		if page.SourceObjectKey != nil && *page.SourceObjectKey != "" {
			object, err := s.storage.Download(r.Context(), *page.SourceObjectKey)
			if err != nil {
				log.Printf("catalog_page_storage_download_failed catalog_id=%d page_id=%d object_key=%q err=%v", catalogID, page.ID, *page.SourceObjectKey, err)
				writeError(w, http.StatusInternalServerError, "failed to load catalog page")
				return
			}
			defer object.Close()

			info, err := object.Stat()
			if err != nil {
				log.Printf("catalog_page_storage_stat_failed catalog_id=%d page_id=%d object_key=%q err=%v", catalogID, page.ID, *page.SourceObjectKey, err)
				writeError(w, http.StatusInternalServerError, "failed to stat catalog page")
				return
			}
			if info.Size <= catalogPageCacheMaxBytes {
				payload, err := io.ReadAll(io.LimitReader(object, catalogPageCacheMaxBytes+1))
				if err != nil {
					log.Printf("catalog_page_storage_read_failed catalog_id=%d page_id=%d object_key=%q err=%v", catalogID, page.ID, *page.SourceObjectKey, err)
					writeError(w, http.StatusInternalServerError, "failed to read catalog page")
					return
				}
				s.cacheCatalogPageContent(r.Context(), catalogID, page, payload)
				log.Printf("catalog_page_served source=storage catalog_id=%d page_id=%d bytes=%d cached=true", catalogID, page.ID, len(payload))
				w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
				_, _ = w.Write(payload)
				return
			}
			w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
			log.Printf("catalog_page_served source=storage catalog_id=%d page_id=%d bytes=%d cached=false", catalogID, page.ID, info.Size)
			io.Copy(w, object)
			return
		}

		if page.DriveFileID != nil && *page.DriveFileID != "" {
			reader, err := s.drive.DownloadFile(r.Context(), *page.DriveFileID)
			if err != nil {
				log.Printf("catalog_page_drive_download_failed catalog_id=%d page_id=%d drive_file_id=%q err=%v", catalogID, page.ID, *page.DriveFileID, err)
				writeError(w, http.StatusInternalServerError, "failed to load catalog page")
				return
			}
			defer reader.Close()
			if page.SourceSize != nil && *page.SourceSize <= catalogPageCacheMaxBytes {
				payload, err := io.ReadAll(io.LimitReader(reader, catalogPageCacheMaxBytes+1))
				if err != nil {
					log.Printf("catalog_page_drive_read_failed catalog_id=%d page_id=%d drive_file_id=%q err=%v", catalogID, page.ID, *page.DriveFileID, err)
					writeError(w, http.StatusInternalServerError, "failed to read catalog page")
					return
				}
				s.cacheCatalogPageContent(r.Context(), catalogID, page, payload)
				log.Printf("catalog_page_served source=drive catalog_id=%d page_id=%d bytes=%d cached=true", catalogID, page.ID, len(payload))
				w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
				_, _ = w.Write(payload)
				return
			}
			sizeLabel := int64(-1)
			if page.SourceSize != nil {
				sizeLabel = *page.SourceSize
			}
			log.Printf("catalog_page_served source=drive catalog_id=%d page_id=%d bytes=%d cached=false", catalogID, page.ID, sizeLabel)
			io.Copy(w, reader)
			return
		}

		log.Printf("catalog_page_missing_source catalog_id=%d page_id=%d source_name=%q", catalogID, page.ID, page.SourceName)
		writeError(w, http.StatusConflict, "catalog page has no stored content")
		return
	}

	log.Printf("catalog_page_not_found catalog_id=%d page_id=%d user_id=%d", catalogID, pageID, currentUser(r.Context()).ID)
	writeError(w, http.StatusNotFound, "catalog page not found")
}

func (s *Server) handleJobByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
	if path == "" {
		writeError(w, http.StatusNotFound, "job route not found")
		return
	}

	if strings.HasSuffix(path, "/download") {
		idValue := strings.TrimSuffix(path, "/download")
		jobID, err := strconv.ParseInt(strings.TrimSuffix(idValue, "/"), 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid job id")
			return
		}
		s.handleJobDownload(w, r, jobID)
		return
	}

	if strings.HasSuffix(path, "/retry") {
		idValue := strings.TrimSuffix(path, "/retry")
		jobID, err := strconv.ParseInt(strings.TrimSuffix(idValue, "/"), 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid job id")
			return
		}
		s.handleJobRetry(w, r, jobID)
		return
	}

	jobID, err := strconv.ParseInt(strings.TrimSuffix(path, "/"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid job id")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleJobDetail(w, r, jobID)
	case http.MethodDelete:
		s.handleJobDelete(w, r, jobID)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleJobDetail(w http.ResponseWriter, r *http.Request, jobID int64) {
	job, err := s.repo.GetJob(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load job")
		return
	}
	if !auth.CanAccessJob(currentUser(r.Context()), job.UserID) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}

	s.hydrateJobRuntimeState(&job)
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleJobDownload(w http.ResponseWriter, r *http.Request, jobID int64) {
	job, err := s.repo.GetJob(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load job")
		return
	}
	if !auth.CanAccessJob(currentUser(r.Context()), job.UserID) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if job.Status != model.JobStatusCompleted || job.OutputObjectKey == "" {
		writeError(w, http.StatusConflict, "job is not ready for download")
		return
	}

	object, err := s.storage.Download(r.Context(), job.OutputObjectKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load merged file")
		return
	}
	defer object.Close()

	stat, err := object.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to stat merged file")
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", job.OutputFilename))
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size, 10))
	io.Copy(w, object)
}

func (s *Server) handleJobDelete(w http.ResponseWriter, r *http.Request, jobID int64) {
	job, err := s.repo.GetJob(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load job")
		return
	}
	if !auth.CanAccessJob(currentUser(r.Context()), job.UserID) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}

	if job.OutputObjectKey != "" {
		if err := s.storage.Delete(r.Context(), job.OutputObjectKey); err != nil {
			log.Printf("job=%d delete_object_failed object_key=%q err=%v", jobID, job.OutputObjectKey, err)
			writeError(w, http.StatusInternalServerError, "failed to delete merged file")
			return
		}
	}
	if err := s.repo.DeleteJob(r.Context(), jobID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete job")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleJobRetry(w http.ResponseWriter, r *http.Request, jobID int64) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	job, err := s.repo.GetJob(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load job")
		return
	}
	if !auth.CanAccessJob(currentUser(r.Context()), job.UserID) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if job.SourceType != model.SourceTypeDrive {
		writeError(w, http.StatusConflict, "retry is only supported for drive jobs")
		return
	}
	if len(job.Files) == 0 {
		writeError(w, http.StatusConflict, "job has no source files to retry")
		return
	}

	driveFiles := make([]model.DrivePreviewFile, 0, len(job.Files))
	jobFiles := make([]model.JobFile, 0, len(job.Files))
	for _, file := range job.Files {
		if file.DriveFileID == nil || file.DriveLink == nil {
			writeError(w, http.StatusConflict, "job is missing drive source metadata")
			return
		}
		size := int64(0)
		if file.SourceSize != nil {
			size = *file.SourceSize
		}
		driveFiles = append(driveFiles, model.DrivePreviewFile{
			SourceID:        *file.DriveFileID,
			Name:            file.SourceName,
			Size:            size,
			ExtractedOrder:  file.SourceOrder,
			WebViewLink:     *file.DriveLink,
			JobFileID:       file.ID,
			SourceObjectKey: stringValue(file.SourceObjectKey),
		})

		sizeCopy := size
		sourceID := *file.DriveFileID
		driveLink := *file.DriveLink
		sourceObjectKey := file.SourceObjectKey
		jobFiles = append(jobFiles, model.JobFile{
			SourceKind:      string(model.SourceTypeDrive),
			SourceName:      file.SourceName,
			SourceOrder:     file.SourceOrder,
			SourceSize:      &sizeCopy,
			DriveFileID:     &sourceID,
			DriveLink:       &driveLink,
			SourceObjectKey: sourceObjectKey,
		})
	}

	outputName := buildOutputFilename("drive-merged")
	retryJob, err := s.repo.CreateJob(
		r.Context(),
		job.UserID,
		model.SourceTypeDrive,
		model.JobStatusPending,
		5,
		outputName,
		jobFiles,
		model.JobRuntimeState{
			CurrentStage: jobStageQueued,
			TotalFiles:   len(driveFiles),
		},
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create retry job")
		return
	}

	log.Printf("job=%d source=drive event=retry_from source_job=%d total_files=%d output=%q", retryJob.ID, jobID, len(driveFiles), retryJob.OutputFilename)
	attachDriveJobFileMetadata(driveFiles, retryJob.Files)
	go s.processDriveMerge(retryJob.ID, outputName, driveFiles)
	writeJSON(w, http.StatusAccepted, retryJob)
}

func saveMultipartFile(workDir string, header *multipart.FileHeader) (string, int64, error) {
	src, err := header.Open()
	if err != nil {
		return "", 0, err
	}
	defer src.Close()

	dstPath := filepath.Join(workDir, header.Filename)
	size, err := saveUploadedReader(dstPath, src)
	if err != nil {
		return "", 0, err
	}
	return dstPath, size, nil
}

func (s *Server) failJob(jobID int64, progressPercent int, message string) {
	log.Printf("job=%d event=failed progress=%d message=%q", jobID, progressPercent, message)
	if err := s.repo.FailJob(context.Background(), jobID, progressPercent, message); err != nil {
		log.Printf("failed to mark job %d as failed: %v", jobID, err)
	}
}

func (s *Server) initDownloadState(jobID int64, files []model.DrivePreviewFile) {
	state := &jobDownloadState{
		fileBytes:           make(map[string]int64, len(files)),
		fileStatus:          make(map[string]string, len(files)),
		lastReportedPercent: -1,
	}
	for _, file := range files {
		state.totalBytes += maxInt64(file.Size, 0)
		state.fileStatus[file.SourceID] = "pending"
	}
	s.downloadStateMu.Lock()
	s.downloadState[jobID] = state
	s.downloadStateMu.Unlock()
}

func (s *Server) clearDownloadState(jobID int64) {
	s.downloadStateMu.Lock()
	delete(s.downloadState, jobID)
	s.downloadStateMu.Unlock()
}

func (s *Server) updateDownloadState(jobID int64, file model.DrivePreviewFile, fileIndex, totalFiles int, written int64, status string) {
	s.downloadStateMu.Lock()
	state, ok := s.downloadState[jobID]
	if !ok {
		s.downloadStateMu.Unlock()
		return
	}
	state.fileBytes[file.SourceID] = maxInt64(written, 0)
	if status != "" {
		state.fileStatus[file.SourceID] = status
	}
	var downloadedBytes int64
	for _, fileBytes := range state.fileBytes {
		downloadedBytes += fileBytes
	}
	percent := 10
	if state.totalBytes > 0 {
		percent = 10 + int((downloadedBytes*60)/state.totalBytes)
	}
	if percent > 70 {
		percent = 70
	}
	now := time.Now()
	shouldReport := percent == 70 || percent != state.lastReportedPercent || now.Sub(state.lastReportedAt) >= 500*time.Millisecond
	if shouldReport {
		state.lastReportedPercent = percent
		state.lastReportedAt = now
	}
	s.downloadStateMu.Unlock()

	if shouldReport {
		_ = s.repo.UpdateJobState(context.Background(), jobID, model.JobStatusRunning, percent, model.JobRuntimeState{
			CurrentStage:     jobStageDownloading,
			CurrentFileName:  file.Name,
			CurrentFileIndex: fileIndex + 1,
			TotalFiles:       totalFiles,
			CurrentFileBytes: written,
			CurrentFileSize:  file.Size,
		})
	}
}

func (s *Server) hydrateJobRuntimeState(job *model.Job) {
	if job == nil || job.SourceType != model.SourceTypeDrive || len(job.Files) == 0 {
		return
	}
	if job.Status == model.JobStatusCompleted || job.CurrentStage == jobStageMerging || job.CurrentStage == jobStageUploading {
		for index := range job.Files {
			job.Files[index].ProgressPercent = 100
			job.Files[index].RuntimeStatus = "downloaded"
		}
		return
	}

	s.downloadStateMu.RLock()
	state := s.downloadState[job.ID]
	s.downloadStateMu.RUnlock()
	if state == nil {
		return
	}

	s.downloadStateMu.RLock()
	defer s.downloadStateMu.RUnlock()
	for index := range job.Files {
		file := &job.Files[index]
		if file.DriveFileID == nil {
			continue
		}
		bytes := state.fileBytes[*file.DriveFileID]
		size := int64(0)
		if file.SourceSize != nil {
			size = *file.SourceSize
		}
		file.ProgressPercent = progressPercent(bytes, size)
		file.RuntimeStatus = state.fileStatus[*file.DriveFileID]
	}
}

func progressStep(current, total, min, max int) int {
	if total <= 0 {
		return max
	}
	if current < 0 {
		current = 0
	}
	if current > total {
		current = total
	}
	return min + ((max - min) * current / total)
}

func progressPercent(current, total int64) int {
	if total <= 0 {
		return 0
	}
	percent := int((current * 100) / total)
	if percent < 0 {
		return 0
	}
	if percent > 100 {
		return 100
	}
	return percent
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func humanBytes(size int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)

	switch {
	case size >= gb:
		return fmt.Sprintf("%.1f GB", float64(size)/float64(gb))
	case size >= mb:
		return fmt.Sprintf("%.0f MB", float64(size)/float64(mb))
	case size >= kb:
		return fmt.Sprintf("%.0f KB", float64(size)/float64(kb))
	default:
		return fmt.Sprintf("%d B", size)
	}
}

func buildOutputFilename(prefix string) string {
	return fmt.Sprintf("%s-%s.pdf", prefix, time.Now().Format("02-01-2006_15-04"))
}

func buildCatalogTitle(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, time.Now().Format("02-01-2006_15-04"))
}

func buildSourceCacheObjectKey(jobID int64, file model.DrivePreviewFile) string {
	return fmt.Sprintf("source-cache/jobs/%d/%s-%s%s", jobID, file.SourceID, sanitizeFilename(strings.TrimSuffix(file.Name, filepath.Ext(file.Name))), strings.ToLower(filepath.Ext(file.Name)))
}

func buildCatalogUploadObjectKey(userID int64, filename string) string {
	extension := strings.ToLower(filepath.Ext(filename))
	base := sanitizeFilename(strings.TrimSuffix(filename, filepath.Ext(filename)))
	return fmt.Sprintf("catalogs/%d/%d-%s%s", userID, time.Now().UnixNano(), base, extension)
}

func buildCatalogDriveObjectKey(userID int64, file model.DrivePreviewFile) string {
	extension := strings.ToLower(filepath.Ext(file.Name))
	base := sanitizeFilename(strings.TrimSuffix(file.Name, filepath.Ext(file.Name)))
	return fmt.Sprintf("catalogs/%d/drive/%s-%s%s", userID, file.SourceID, base, extension)
}

func detectContentType(path string) string {
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if strings.TrimSpace(contentType) == "" {
		return "application/octet-stream"
	}
	return contentType
}

func (s *Server) getCachedCatalogDetail(ctx context.Context, catalogID int64) (model.Catalog, bool) {
	if s.cache == nil {
		return model.Catalog{}, false
	}

	cacheCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	var catalog model.Catalog
	ok, err := s.cache.GetJSON(cacheCtx, catalogDetailCacheKey(catalogID), &catalog)
	if err != nil {
		log.Printf("catalog_cache_get_failed catalog_id=%d err=%v", catalogID, err)
		return model.Catalog{}, false
	}
	return catalog, ok
}

func (s *Server) cacheCatalogDetail(ctx context.Context, catalog model.Catalog) {
	if s.cache == nil {
		return
	}

	cacheCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	if err := s.cache.SetJSON(cacheCtx, catalogDetailCacheKey(catalog.ID), catalog, 15*time.Minute); err != nil {
		log.Printf("catalog_cache_set_failed catalog_id=%d err=%v", catalog.ID, err)
	}
}

func catalogDetailCacheKey(catalogID int64) string {
	return fmt.Sprintf("catalog:%d:detail", catalogID)
}

func (s *Server) getCachedCatalogPageContent(ctx context.Context, catalogID int64, page model.CatalogPage) ([]byte, bool) {
	if s.cache == nil {
		return nil, false
	}

	cacheCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	payload, ok, err := s.cache.GetBytes(cacheCtx, catalogPageContentCacheKey(catalogID, page.ID))
	if err != nil {
		log.Printf("catalog_page_cache_get_failed catalog_id=%d page_id=%d err=%v", catalogID, page.ID, err)
		return nil, false
	}
	return payload, ok
}

func (s *Server) cacheCatalogPageContent(ctx context.Context, catalogID int64, page model.CatalogPage, payload []byte) {
	if s.cache == nil || len(payload) > catalogPageCacheMaxBytes {
		return
	}

	cacheCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	if err := s.cache.SetBytes(cacheCtx, catalogPageContentCacheKey(catalogID, page.ID), payload, 24*time.Hour); err != nil {
		log.Printf("catalog_page_cache_set_failed catalog_id=%d page_id=%d err=%v", catalogID, page.ID, err)
		return
	}
	log.Printf("catalog_page_cache_store catalog_id=%d page_id=%d bytes=%d", catalogID, page.ID, len(payload))
}

func catalogPageContentCacheKey(catalogID, pageID int64) string {
	return fmt.Sprintf("catalog:%d:page:%d:content", catalogID, pageID)
}

func applyDriveOrders(files []model.DrivePreviewFile, orders map[string]int) error {
	if len(orders) == 0 {
		return nil
	}

	for index := range files {
		order, ok := orders[files[index].SourceID]
		if !ok {
			return fmt.Errorf("missing order for %s", files[index].Name)
		}
		files[index].ExtractedOrder = order
	}

	sort.Slice(files, func(i, j int) bool {
		if files[i].ExtractedOrder == files[j].ExtractedOrder {
			return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name)
		}
		return files[i].ExtractedOrder < files[j].ExtractedOrder
	})
	return nil
}

func attachDriveJobFileMetadata(files []model.DrivePreviewFile, jobFiles []model.JobFile) {
	bySourceID := make(map[string]model.JobFile, len(jobFiles))
	for _, file := range jobFiles {
		if file.DriveFileID != nil {
			bySourceID[*file.DriveFileID] = file
		}
	}

	for index := range files {
		if jobFile, ok := bySourceID[files[index].SourceID]; ok {
			files[index].JobFileID = jobFile.ID
			files[index].SourceObjectKey = stringValue(jobFile.SourceObjectKey)
		}
	}
}

func sanitizeFilename(name string) string {
	replacer := strings.NewReplacer("/", "-", "\\", "-", " ", "_", ":", "-")
	return replacer.Replace(name)
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func saveUploadedReader(dstPath string, src io.Reader) (int64, error) {
	return saveUploadedReaderWithProgress(dstPath, src, nil)
}

func saveUploadedReaderWithProgress(dstPath string, src io.Reader, onProgress func(int64)) (int64, error) {
	dst, err := os.Create(dstPath)
	if err != nil {
		return 0, err
	}
	defer dst.Close()

	size, err := io.Copy(dst, &progressReader{reader: src, onProgress: onProgress})
	if err != nil {
		return 0, err
	}
	return size, nil
}

type progressReader struct {
	reader     io.Reader
	onProgress func(int64)
	total      int64
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.total += int64(n)
		if r.onProgress != nil {
			r.onProgress(r.total)
		}
	}
	return n, err
}

func currentUser(ctx context.Context) model.User {
	return ctx.Value(authContextKey{}).(model.User)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
