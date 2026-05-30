package model

import "time"

type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

type User struct {
	ID           int64     `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	Role         Role      `json:"role"`
	CreatedAt    time.Time `json:"createdAt"`
}

type SourceType string

const (
	SourceTypeDrive  SourceType = "drive"
	SourceTypeUpload SourceType = "upload"
)

type JobStatus string

const (
	JobStatusPending   JobStatus = "pending"
	JobStatusRunning   JobStatus = "running"
	JobStatusCompleted JobStatus = "completed"
	JobStatusFailed    JobStatus = "failed"
)

type Job struct {
	ID               int64      `json:"id"`
	UserID           int64      `json:"userId"`
	SourceType       SourceType `json:"sourceType"`
	Status           JobStatus  `json:"status"`
	OutputObjectKey  string     `json:"-"`
	OutputFilename   string     `json:"outputFilename"`
	ProgressPercent  int        `json:"progressPercent"`
	CurrentStage     string     `json:"currentStage,omitempty"`
	CurrentFileName  string     `json:"currentFileName,omitempty"`
	CurrentFileIndex int        `json:"currentFileIndex,omitempty"`
	TotalFiles       int        `json:"totalFiles,omitempty"`
	CurrentFileBytes int64      `json:"currentFileBytes,omitempty"`
	CurrentFileSize  int64      `json:"currentFileSize,omitempty"`
	ErrorMessage     *string    `json:"errorMessage,omitempty"`
	CreatedAt        time.Time  `json:"createdAt"`
	Files            []JobFile  `json:"files,omitempty"`
}

type JobRuntimeState struct {
	CurrentStage     string
	CurrentFileName  string
	CurrentFileIndex int
	TotalFiles       int
	CurrentFileBytes int64
	CurrentFileSize  int64
}

type JobFile struct {
	ID              int64   `json:"id"`
	JobID           int64   `json:"jobId"`
	SourceKind      string  `json:"sourceKind"`
	SourceName      string  `json:"name"`
	SourceOrder     int     `json:"order"`
	SourceSize      *int64  `json:"size,omitempty"`
	DriveFileID     *string `json:"driveFileId,omitempty"`
	DriveLink       *string `json:"driveLink,omitempty"`
	SourceObjectKey *string `json:"sourceObjectKey,omitempty"`
	ProgressPercent int     `json:"progressPercent,omitempty"`
	RuntimeStatus   string  `json:"runtimeStatus,omitempty"`
}

type DrivePreviewFile struct {
	SourceID        string `json:"sourceId"`
	Name            string `json:"name"`
	Size            int64  `json:"size"`
	ExtractedOrder  int    `json:"extractedOrder"`
	WebViewLink     string `json:"webViewLink"`
	JobFileID       int64  `json:"-"`
	SourceObjectKey string `json:"-"`
}

type MergeFileInput struct {
	Name      string
	LocalPath string
	Order     int
	Size      int64
	SourceID  string
	DriveLink string
}

type Catalog struct {
	ID         int64         `json:"id"`
	UserID     int64         `json:"userId"`
	SourceType SourceType    `json:"sourceType"`
	Title      string        `json:"title"`
	CreatedAt  time.Time     `json:"createdAt"`
	Pages      []CatalogPage `json:"pages,omitempty"`
}

type CatalogPage struct {
	ID              int64   `json:"id"`
	CatalogID       int64   `json:"catalogId"`
	SourceKind      string  `json:"sourceKind"`
	SourceName      string  `json:"name"`
	SourceOrder     int     `json:"order"`
	SourceSize      *int64  `json:"size,omitempty"`
	DriveFileID     *string `json:"driveFileId,omitempty"`
	SourceObjectKey *string `json:"-"`
	MimeType        string  `json:"mimeType"`
}
