package storage

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Client struct {
	bucket string
	client *minio.Client
}

// New creates the MinIO client once so merge requests avoid repeated connection setup work.
func New(endpoint, accessKey, secretKey, bucket string, useSSL bool) (*Client, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("create minio client: %w", err)
	}

	return &Client{bucket: bucket, client: client}, nil
}

// EnsureBucket front-loads storage validation so merge requests fail fast during startup, not mid-job.
func (c *Client) EnsureBucket(ctx context.Context) error {
	exists, err := c.client.BucketExists(ctx, c.bucket)
	if err != nil {
		return fmt.Errorf("check bucket: %w", err)
	}
	if exists {
		return nil
	}

	if err := c.client.MakeBucket(ctx, c.bucket, minio.MakeBucketOptions{}); err != nil {
		return fmt.Errorf("create bucket: %w", err)
	}
	return nil
}

// Upload stores only merged outputs, keeping the long-term storage policy intentionally narrow.
func (c *Client) Upload(ctx context.Context, objectKey string, reader io.Reader, size int64) error {
	return c.UploadPDF(ctx, objectKey, reader, size)
}

func (c *Client) UploadPDF(ctx context.Context, objectKey string, reader io.Reader, size int64) error {
	return c.UploadObject(ctx, objectKey, reader, size, "application/pdf")
}

func (c *Client) UploadObject(ctx context.Context, objectKey string, reader io.Reader, size int64, contentType string) error {
	if strings.TrimSpace(contentType) == "" {
		contentType = "application/octet-stream"
	}
	_, err := c.client.PutObject(ctx, c.bucket, objectKey, reader, size, minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return fmt.Errorf("upload object: %w", err)
	}
	return nil
}

// Download rehydrates a merged output for history downloads without touching the database again.
func (c *Client) Download(ctx context.Context, objectKey string) (*minio.Object, error) {
	object, err := c.client.GetObject(ctx, c.bucket, objectKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("download object: %w", err)
	}
	return object, nil
}

func (c *Client) Stat(ctx context.Context, objectKey string) (minio.ObjectInfo, error) {
	info, err := c.client.StatObject(ctx, c.bucket, objectKey, minio.StatObjectOptions{})
	if err != nil {
		return minio.ObjectInfo{}, fmt.Errorf("stat object: %w", err)
	}
	return info, nil
}

// Delete supports per-user history cleanup by removing the persisted merged object.
func (c *Client) Delete(ctx context.Context, objectKey string) error {
	if err := c.client.RemoveObject(ctx, c.bucket, objectKey, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete object: %w", err)
	}
	return nil
}
