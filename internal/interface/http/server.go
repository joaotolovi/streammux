package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/streammux/streammux/internal/application/muxer"
	"github.com/streammux/streammux/internal/domain/constants"
	"github.com/streammux/streammux/internal/domain/model"
	"github.com/streammux/streammux/internal/domain/ports"
)

type Server struct {
	users   ports.UserRepository
	store   ports.MuxStore
	muxer   *muxer.Muxer
	baseURL string
	web     fs.FS
	mux     *http.ServeMux
}

type Options struct {
	BaseURL string
	WebFS   fs.FS
}

func New(users ports.UserRepository, store ports.MuxStore, mux *muxer.Muxer, opts Options) *Server {
	s := &Server{
		users:   users,
		store:   store,
		muxer:   mux,
		baseURL: opts.BaseURL,
		web:     opts.WebFS,
		mux:     http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	// Stremio protocol — public manifest (redirects to configure).
	s.mux.HandleFunc("GET /manifest.json", s.handlePublicManifest)
	s.mux.HandleFunc("GET /stream/{type}/{id...}", s.handleConfigureRedirect)

	// Stremio protocol — authenticated routes (credenciais no path, como o AIOStreams).
	s.mux.HandleFunc("GET /stremio/{uuid}/{password}/manifest.json", s.handleManifest)
	s.mux.HandleFunc("GET /stremio/{uuid}/{password}/stream/{type}/{id...}", s.handleStream)

	// HLS endpoints — master/video/audio playlists and on-demand segments
	s.mux.HandleFunc("GET /mux/{jobId}", s.handleMuxRedirect)
	s.mux.HandleFunc("GET /mux/{jobId}/master.m3u8", s.handleHLSPlaylist)
	s.mux.HandleFunc("GET /mux/{jobId}/playlist.m3u8", s.handleHLSPlaylist)
	s.mux.HandleFunc("GET /mux/{jobId}/video.m3u8", s.handleHLSMediaPlaylist)
	s.mux.HandleFunc("GET /mux/{jobId}/audio.m3u8", s.handleHLSMediaPlaylist)
	s.mux.HandleFunc("GET /mux/{jobId}/{segment}", s.handleHLSSegment)

	// API
	s.mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	s.mux.HandleFunc("POST /api/v1/user", s.handleCreateUser)
	s.mux.HandleFunc("GET /api/v1/user", s.handleGetUser)
	s.mux.HandleFunc("PUT /api/v1/user", s.handleUpdateUser)
	s.mux.HandleFunc("DELETE /api/v1/user", s.handleDeleteUser)

	// Static / SPA
	s.mux.Handle("/", s.spaHandler())
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handlePublicManifest(w http.ResponseWriter, r *http.Request) {
	// Public manifest points at the configure page so the user can set up.
	manifest := model.Manifest{
		ID:          "com.streammux.viren070",
		Version:     "1.0.0",
		Name:        "StreamMux",
		Description: "Combina a melhor qualidade de vídeo com o áudio no seu idioma.",
		Types:       []string{"movie", "series"},
		Resources:   []string{constants.StreamResource},
		BehaviorHints: map[string]any{
			"configurable":          true,
			"configurationRequired": true,
		},
	}
	writeJSON(w, manifest)
}

func (s *Server) handleConfigureRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	password := r.PathValue("password")
	if _, err := s.users.Get(r.Context(), uuid, password); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	base := s.baseURL + "/stremio/" + uuid + "/" + password
	manifest := model.Manifest{
		ID:          "com.streammux.viren070",
		Version:     "1.0.0",
		Name:        "StreamMux",
		Description: "Combina a melhor qualidade de vídeo com o áudio no seu idioma.",
		Types:       []string{"movie", "series"},
		Resources:   []string{constants.StreamResource},
	}
	_ = base
	writeJSON(w, manifest)
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	contentType := r.PathValue("type")
	contentID := strings.TrimSuffix(r.PathValue("id"), ".json")

	cfg := s.resolvePathConfig(w, r)
	if cfg == nil {
		return
	}

	ctx := r.Context()
	result, err := s.muxer.Process(ctx, cfg, contentType, contentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var streams []model.StremioStream
	if result.Dubbed != nil {
		streams = append(streams, *result.Dubbed)
	}
	if result.Subtitled != nil {
		streams = append(streams, *result.Subtitled)
	}

	writeJSON(w, map[string]any{"streams": streams})
}

func (s *Server) handleMuxRedirect(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobId")
	http.Redirect(w, r, "/mux/"+jobID+"/master.m3u8", http.StatusFound)
}

func (s *Server) handleHLSPlaylist(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobId")
	job, ok := s.store.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "mux job not found")
		return
	}

	// Generate the static playlist (probes duration once, cached on the job).
	if err := s.muxer.EnsurePlaylist(r.Context(), job); err != nil {
		log.Printf("mux playlist: %v", err)
		writeError(w, http.StatusInternalServerError, "playlist generation failed")
		return
	}

	playlistPath := s.muxer.PlaylistPath(job)
	if playlistPath == "" {
		writeError(w, http.StatusInternalServerError, "playlist not ready")
		return
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, playlistPath)
}

func (s *Server) handleHLSMediaPlaylist(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobId")
	job, ok := s.store.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "mux job not found")
		return
	}
	if err := s.muxer.EnsurePlaylist(r.Context(), job); err != nil {
		log.Printf("mux playlist: %v", err)
		writeError(w, http.StatusInternalServerError, "playlist generation failed")
		return
	}
	name := filepath.Base(r.URL.Path)
	path := filepath.Join(job.CacheDir, name)
	if _, err := os.Stat(path); err != nil {
		writeError(w, http.StatusNotFound, "playlist not found")
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, path)
}

func (s *Server) handleHLSSegment(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobId")
	segment := filepath.Base(r.PathValue("segment"))

	job, ok := s.store.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "mux job not found")
		return
	}

	var segIndex int
	var kind string
	var finalName string
	if _, err := fmt.Sscanf(segment, "v_%05d.ts", &segIndex); err == nil {
		kind = "v"
		finalName = fmt.Sprintf("v_%05d.ts", segIndex)
	} else if _, err := fmt.Sscanf(segment, "a_%05d.ts", &segIndex); err == nil {
		kind = "a"
		finalName = fmt.Sprintf("a_%05d.ts", segIndex)
	} else {
		writeError(w, http.StatusBadRequest, "invalid segment")
		return
	}

	cached := func() string {
		if kind == "v" {
			return s.muxer.VideoSegmentPath(job, segIndex)
		}
		return s.muxer.AudioSegmentPath(job, segIndex)
	}
	gen := func(ctx context.Context, f *os.File) error {
		if kind == "v" {
			return s.muxer.GenerateVideoSegment(ctx, job, segIndex, f)
		}
		return s.muxer.GenerateAudioSegment(ctx, job, segIndex, f)
	}
	lockKey := fmt.Sprintf("%s:%s:%05d", jobID, kind, segIndex)

	s.generateSegment(w, r, job, segIndex, cached, finalName, lockKey, gen)
}

// generateSegment serves an on-demand HLS segment. gen produces it, cachedPath
// resolves the cache, and lockKey is the singleflight key (per job+segment).
func (s *Server) generateSegment(w http.ResponseWriter, r *http.Request, job *model.MuxJob, segIndex int, cachedPath func() string, finalName, lockKey string, gen func(ctx context.Context, f *os.File) error) {
	// Serve from cache if already generated.
	if cached := cachedPath(); cached != "" {
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, cached)
		return
	}

	// Singleflight: only one request generates a given segment at a time.
	lock := s.muxer.SegmentLock(lockKey)
	lock.Lock()
	defer lock.Unlock()

	// Re-check the cache after acquiring the lock — the generator may have
	// finished while we were waiting.
	if cached := cachedPath(); cached != "" {
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, cached)
		return
	}

	// Ensure the cache dir exists (EnsurePlaylist creates it and probes
	// duration once).
	if err := s.muxer.EnsurePlaylist(r.Context(), job); err != nil {
		log.Printf("mux segment: %v", err)
		writeError(w, http.StatusInternalServerError, "segment generation failed")
		return
	}

	tmpPath := filepath.Join(job.CacheDir, finalName+".tmp")
	f, err := os.Create(tmpPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create segment file failed")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	if err := gen(ctx, f); err != nil {
		f.Close()
		os.Remove(tmpPath)
		if ctx.Err() != nil {
			return
		}
		log.Printf("mux segment %d: %v", segIndex, err)
		writeError(w, http.StatusInternalServerError, "segment generation failed")
		return
	}
	f.Close()

	// Atomic rename: .tmp → final so concurrent requests never read a partial
	// segment.
	finalPath := filepath.Join(job.CacheDir, finalName)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		writeError(w, http.StatusInternalServerError, "segment rename failed")
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, finalPath)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"success": true,
		"version": "1.0.0",
		"channel": "stable",
	})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Config   model.Config `json:"config"`
		Password string       `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if body.Password == "" {
		writeError(w, http.StatusBadRequest, "password is required")
		return
	}
	if body.Config.Language == "" {
		writeError(w, http.StatusBadRequest, "language is required")
		return
	}

	uuid, encryptedPassword, err := s.users.Create(r.Context(), &body.Config, body.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"uuid": uuid, "encryptedPassword": encryptedPassword})
}

func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	uuid, password, ok := s.parseBasicAuth(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing credentials")
		return
	}
	cfg, err := s.users.Get(r.Context(), uuid, password)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	writeJSON(w, map[string]any{"config": cfg})
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	uuid, password, ok := s.parseBasicAuth(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing credentials")
		return
	}
	var body struct {
		Config model.Config `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := s.users.Update(r.Context(), uuid, password, &body.Config); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	writeJSON(w, map[string]string{"uuid": uuid})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	uuid, password, ok := s.parseBasicAuth(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing credentials")
		return
	}
	if err := s.users.Delete(r.Context(), uuid, password); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	writeJSON(w, map[string]bool{"success": true})
}

func (s *Server) resolvePathConfig(w http.ResponseWriter, r *http.Request) *model.Config {
	uuid := r.PathValue("uuid")
	password := r.PathValue("password")
	if uuid == "" || password == "" {
		writeError(w, http.StatusUnauthorized, "missing credentials")
		return nil
	}
	cfg, err := s.users.Get(r.Context(), uuid, password)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return nil
	}
	return cfg
}

func (s *Server) parseBasicAuth(r *http.Request) (string, string, bool) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return "", "", false
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	parts := strings.SplitN(token, ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (s *Server) spaHandler() http.Handler {
	if s.web == nil {
		return http.NotFoundHandler()
	}
	index, err := fs.ReadFile(s.web, "index.html")
	if err != nil {
		return http.NotFoundHandler()
	}
	fileServer := http.FileServer(http.FS(s.web))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(s.web, path); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(index)
	})
}

func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"success": false,
		"error":   map[string]string{"message": message},
	})
}
