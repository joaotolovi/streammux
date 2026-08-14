package debrid

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// StremThru is the generic multi-store debrid wrapper. It speaks to every
// supported debrid service (Real-Debrid, TorBox, AllDebrid, …) through a single
// unified API, so StreamMux does not need a per-provider integration.
type StremThru struct {
	serviceName string
	token       string
	clientIP    string
	baseURL     string
	store       string
	client      *http.Client
}

// NewStremThru constructs a StremThru service.
func NewStremThru(serviceName, token, clientIP string) *StremThru {
	return &StremThru{
		serviceName: serviceName,
		token:       token,
		clientIP:    clientIP,
		baseURL:     "https://stremthru.13377001.xyz",
		store:       serviceName,
		client:      &http.Client{Timeout: 30 * time.Second},
	}
}

// ServiceName returns the service name.
func (s *StremThru) ServiceName() string { return s.serviceName }

// Capabilities reports torrents and usenet support.
func (s *StremThru) Capabilities() (bool, bool) { return true, false }

func (s *StremThru) request(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("X-StremThru-Authorization", "Basic "+s.token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, NewError(CodeBadGateway, 0, err.Error())
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, NewError(CodeUnauthorized, resp.StatusCode, "debrid credentials rejected")
	}
	if resp.StatusCode >= 400 {
		return nil, NewError(CodeUnknown, resp.StatusCode, fmt.Sprintf("stremthru: %d %s", resp.StatusCode, resp.Status))
	}
	return data, nil
}

// CheckMagnets checks magnet availability.
func (s *StremThru) CheckMagnets(ctx context.Context, magnets []string) ([]Download, error) {
	if len(magnets) == 0 {
		return nil, nil
	}
	data, err := s.request(ctx, http.MethodPost, "/v0/magnets/check", map[string]any{"magnets": magnets})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data struct {
			Items []struct {
				Hash   string `json:"hash"`
				Status string `json:"status"`
				Files  []struct {
					Name  string `json:"name"`
					Size  int64  `json:"size"`
					Path  string `json:"path"`
					Index int    `json:"index"`
					Link  string `json:"link"`
				} `json:"files"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	var out []Download
	for _, item := range resp.Data.Items {
		d := Download{ID: item.Hash, Hash: item.Hash, Status: mapStatus(item.Status)}
		for _, f := range item.Files {
			d.Files = append(d.Files, File{Name: f.Name, Size: f.Size, Path: f.Path, Index: f.Index, Link: f.Link})
		}
		out = append(out, d)
	}
	return out, nil
}

// AddMagnet adds a magnet.
func (s *StremThru) AddMagnet(ctx context.Context, magnet string) (Download, error) {
	data, err := s.request(ctx, http.MethodPost, "/v0/magnets", map[string]any{"magnet": magnet})
	if err != nil {
		return Download{}, err
	}
	var resp struct {
		Data struct {
			ID     string `json:"id"`
			Hash   string `json:"hash"`
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return Download{}, err
	}
	return Download{ID: resp.Data.ID, Hash: resp.Data.Hash, Name: resp.Data.Name, Status: mapStatus(resp.Data.Status)}, nil
}

// GenerateTorrentLink generates a direct download link for a file.
func (s *StremThru) GenerateTorrentLink(ctx context.Context, link, clientIP string) (string, error) {
	path := "/v0/magnets/generate?link=" + url.QueryEscape(link)
	if clientIP != "" {
		path += "&client_ip=" + url.QueryEscape(clientIP)
	}
	data, err := s.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	var resp struct {
		Data struct {
			Link string `json:"link"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	return resp.Data.Link, nil
}

// Resolve resolves a torrent to a direct URL.
//
// cacheOnly controls whether an uncached torrent is queued for download. When
// true, only already-cached torrents resolve successfully; when false, an
// uncached torrent is added to the debrid (which may take time to complete).
func (s *StremThru) Resolve(ctx context.Context, info PlaybackInfo, filename string, cacheOnly bool) (string, error) {
	ti, ok := info.(TorrentInfo)
	if !ok {
		return "", fmt.Errorf("stremthru: expected torrent info")
	}
	magnet := "magnet:?xt=urn:btih:" + ti.Hash
	downloads, err := s.CheckMagnets(ctx, []string{magnet})
	if err != nil {
		return "", err
	}

	var d *Download
	if len(downloads) > 0 {
		d = &downloads[0]
	}

	if d == nil || (d.Status != StatusCached && d.Status != StatusDownloaded) {
		if cacheOnly {
			return "", NewError(CodeNotFound, 0, "torrent not cached")
		}
		added, err := s.AddMagnet(ctx, magnet)
		if err != nil {
			return "", err
		}
		d = &added
	}

	if d.Status != StatusCached && d.Status != StatusDownloaded {
		return "", NewError(CodeDownloadFailed, 0, "torrent not yet available")
	}

	if len(d.Files) == 0 {
		return "", NewError(CodeNoMatchingFile, 0, "no files available")
	}

	best := selectFile(d.Files, filename)
	if best == nil {
		return "", NewError(CodeNoMatchingFile, 0, "no matching file")
	}
	return s.GenerateTorrentLink(ctx, best.Link, s.clientIP)
}

func selectFile(files []File, filename string) *File {
	if len(files) == 0 {
		return nil
	}
	videoExts := []string{".mkv", ".mp4", ".avi", ".mov", ".m4v", ".ts", ".webm"}
	var best *File
	var bestScore int
	for i := range files {
		f := &files[i]
		score := 0
		name := strings.ToLower(f.Name)
		for _, ext := range videoExts {
			if strings.HasSuffix(name, ext) {
				score += 1000
				break
			}
		}
		if filename != "" && f.Name == filename {
			score += 500
		}
		if strings.Contains(name, "sample") {
			score -= 500
		}
		if best == nil || score > bestScore {
			best = f
			bestScore = score
		}
	}
	return best
}

func mapStatus(status string) string {
	switch strings.ToLower(status) {
	case "cached", "downloaded":
		return StatusCached
	case "downloading":
		return StatusDownloading
	case "queued":
		return StatusQueued
	case "failed":
		return StatusFailed
	default:
		return StatusDownloading
	}
}
