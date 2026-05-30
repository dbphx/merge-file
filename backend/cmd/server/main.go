package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ml/merge-pdf/backend/internal/auth"
	"github.com/ml/merge-pdf/backend/internal/cache"
	"github.com/ml/merge-pdf/backend/internal/config"
	"github.com/ml/merge-pdf/backend/internal/drive"
	"github.com/ml/merge-pdf/backend/internal/merge"
	"github.com/ml/merge-pdf/backend/internal/model"
	"github.com/ml/merge-pdf/backend/internal/repository"
	"github.com/ml/merge-pdf/backend/internal/server"
	"github.com/ml/merge-pdf/backend/internal/storage"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "merge-worker" {
		runMergeWorker()
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx := context.Background()
	db, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer db.Close()

	minioClient, err := storage.New(cfg.MinIOEndpoint, cfg.MinIOAccessKey, cfg.MinIOSecretKey, cfg.MinIOBucket, cfg.MinIOUseSSL)
	if err != nil {
		log.Fatalf("create storage client: %v", err)
	}
	if err := minioClient.EnsureBucket(ctx); err != nil {
		log.Fatalf("ensure storage bucket: %v", err)
	}

	redisClient := cache.New(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
	if redisClient != nil {
		if err := redisClient.Ping(ctx); err != nil {
			log.Printf("redis unavailable, continuing without cache: %v", err)
			redisClient = nil
		}
	}

	repo := repository.New(db)
	authSvc := auth.NewService(cfg.JWTSecret)
	driveClient := drive.NewClient(cfg.GoogleDriveAPIKey, http.DefaultClient)
	srv := server.New(cfg, repo, authSvc, driveClient, minioClient, redisClient)

	shutdownCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-shutdownCtx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	if err := srv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

func runMergeWorker() {
	if len(os.Args) != 5 {
		log.Fatalf("merge-worker usage: %s merge-worker <inputs-json> <workdir> <output-name>", os.Args[0])
	}

	inputsPayload, err := os.ReadFile(os.Args[2])
	if err != nil {
		log.Fatalf("merge-worker read inputs: %v", err)
	}

	var inputs []model.MergeFileInput
	if err := json.Unmarshal(inputsPayload, &inputs); err != nil {
		log.Fatalf("merge-worker decode inputs: %v", err)
	}

	if _, err := merge.MergeFiles(os.Args[3], os.Args[4], inputs); err != nil {
		log.Fatalf("merge-worker merge files: %v", err)
	}
}
