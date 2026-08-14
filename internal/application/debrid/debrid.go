// Package debrid implements the debrid service abstraction used to resolve
// torrents (infoHash) into direct streamable URLs when an addon returns an
// unresolved stream.
package debrid

import (
	"context"
	"fmt"
)

// ErrorCode classifies debrid errors.
type ErrorCode string

const (
	CodeBadGateway         ErrorCode = "BAD_GATEWAY"
	CodeBadRequest         ErrorCode = "BAD_REQUEST"
	CodeNotFound           ErrorCode = "NOT_FOUND"
	CodePaymentRequired    ErrorCode = "PAYMENT_REQUIRED"
	CodeUnauthorized       ErrorCode = "UNAUTHORIZED"
	CodeTooManyRequests    ErrorCode = "TOO_MANY_REQUESTS"
	CodeTimeout            ErrorCode = "TIMEOUT"
	CodeDownloadFailed     ErrorCode = "DOWNLOAD_FAILED"
	CodeNoMatchingFile     ErrorCode = "NO_MATCHING_FILE"
	CodeStoreLimitExceeded ErrorCode = "STORE_LIMIT_EXCEEDED"
	CodeUnknown            ErrorCode = "UNKNOWN"
)

// Error is a debrid error.
type Error struct {
	Message    string
	Code       ErrorCode
	StatusCode int
}

func (e *Error) Error() string { return e.Message }

// NewError constructs a DebridError.
func NewError(code ErrorCode, status int, msg string) *Error {
	return &Error{Message: msg, Code: code, StatusCode: status}
}

// File is a debrid file.
type File struct {
	ID       int    `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Size     int64  `json:"size"`
	MimeType string `json:"mimeType,omitempty"`
	Link     string `json:"link,omitempty"`
	Path     string `json:"path,omitempty"`
	Index    int    `json:"index,omitempty"`
}

// Download is a debrid download (magnet).
type Download struct {
	ID      string `json:"id"`
	Library bool   `json:"library,omitempty"`
	Hash    string `json:"hash,omitempty"`
	Name    string `json:"name,omitempty"`
	Private bool   `json:"private,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Status  string `json:"status"`
	Files   []File `json:"files,omitempty"`
}

// Status constants.
const (
	StatusCached      = "cached"
	StatusDownloaded  = "downloaded"
	StatusDownloading = "downloading"
	StatusFailed      = "failed"
	StatusInvalid     = "invalid"
	StatusProcessing  = "processing"
	StatusQueued      = "queued"
	StatusUploading   = "uploading"
)

// TorrentInfo is a torrent playback info.
type TorrentInfo struct {
	Type        string `json:"type"`
	Hash        string `json:"hash"`
	DownloadURL string `json:"downloadUrl,omitempty"`
	Private     bool   `json:"private,omitempty"`
	Title       string `json:"title,omitempty"`
}

// PlaybackInfo is the shared interface of all playback info types.
type PlaybackInfo interface {
	InfoType() string
}

// InfoType implements PlaybackInfo.
func (t TorrentInfo) InfoType() string { return "torrent" }

// BaseService is the common service interface.
type BaseService interface {
	ServiceName() string
	Capabilities() (torrents, usenet bool)
	Resolve(ctx context.Context, info PlaybackInfo, filename string, cacheAndPlay bool) (string, error)
}

// TorrentService is the torrent-capable debrid interface.
type TorrentService interface {
	BaseService
	CheckMagnets(ctx context.Context, magnets []string) ([]Download, error)
	AddMagnet(ctx context.Context, magnet string) (Download, error)
	GenerateTorrentLink(ctx context.Context, link, clientIP string) (string, error)
}

// Factory builds a debrid service by name.
func Factory(name, token, clientIP string) (BaseService, error) {
	switch name {
	case "realdebrid", "debridlink", "premiumize", "alldebrid", "easydebrid", "debrider", "offcloud", "pikpak", "torrin", "torbox":
		return NewStremThru(name, token, clientIP), nil
	}
	return nil, fmt.Errorf("unknown debrid service %q", name)
}
