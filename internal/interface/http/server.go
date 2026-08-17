package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
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

	// HLS endpoints — playlist and segments
	s.mux.HandleFunc("GET /mux/{jobId}", s.handleMuxRedirect)
	s.mux.HandleFunc("GET /mux/{jobId}/playlist.m3u8", s.handleHLSPlaylist)
	s.mux.HandleFunc("GET /mux/{jobId}/video/video.m3u8", s.handleHLSVideoPlaylist)
	s.mux.HandleFunc("GET /mux/{jobId}/audio/audio.m3u8", s.handleHLSAudioPlaylist)
	s.mux.HandleFunc("GET /mux/{jobId}/video/{segment}", s.handleHLSSegment)
	s.mux.HandleFunc("GET /mux/{jobId}/audio/{segment}", s.handleHLSAudioSegment)
	// ABR variant endpoints — per-plan media playlists and segments.
	s.mux.HandleFunc("GET /mux/{jobId}/v{planIndex}/video/video.m3u8", s.handleHLSVariantVideoPlaylist)
	s.mux.HandleFunc("GET /mux/{jobId}/v{planIndex}/audio/audio.m3u8", s.handleHLSVariantAudioPlaylist)
	s.mux.HandleFunc("GET /mux/{jobId}/v{planIndex}/video/{segment}", s.handleHLSVariantSegment)
	s.mux.HandleFunc("GET /mux/{jobId}/v{planIndex}/audio/{segment}", s.handleHLSVariantAudioSegment)

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
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Range")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
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
	http.Redirect(w, r, "/mux/"+jobID+"/playlist.m3u8", http.StatusFound)
}

func (s *Server) handleHLSPlaylist(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobId")
	job, ok := s.store.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "mux job not found")
		return
	}

	// Start the bounded playback race. If every HLS plan misses the startup
	// budget, redirect to the best direct source rather than leaving the player
	// with a dead stream.
	if err := s.muxer.EnsurePlaylist(r.Context(), job); err != nil {
		var direct *muxer.DirectFallbackError
		if errors.As(err, &direct) && direct.URL != "" {
			log.Printf("mux playlist: using direct fallback after HLS startup failure: %v", err)
			http.Redirect(w, r, direct.URL, http.StatusTemporaryRedirect)
			return
		}
		log.Printf("mux playlist: %v", err)
		writeError(w, http.StatusBadGateway, "no playable source started in time")
		return
	}

	// Serve the ABR master playlist with all variants.
	if data, ok := s.muxer.MasterPlaylist(job); ok {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(data)
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

func (s *Server) handleHLSSegment(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobId")
	segment := filepath.Base(r.PathValue("segment"))
	job, ok := s.store.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "mux job not found")
		return
	}

	// Parse segment index from filename like "seg_00123.ts".
	var segIndex int
	if _, err := fmt.Sscanf(segment, "seg_%05d.ts", &segIndex); err != nil {
		writeError(w, http.StatusBadRequest, "invalid segment")
		return
	}

	// Serve from cache if already generated.
	if cached := s.muxer.SegmentPath(job, segIndex); cached != "" {
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, cached)
		return
	}

	// Ensure the cache dir / playlists exist (probes duration + picks sources
	// once), then start (or reuse) the continuous ffmpeg session and wait for
	// the segment to be written.
	if err := s.muxer.EnsurePlaylist(r.Context(), job); err != nil {
		log.Printf("mux segment: %v", err)
		writeError(w, http.StatusBadGateway, "segment source unavailable")
		return
	}

	segPath, err := s.muxer.EnsureSegment(r.Context(), job, segIndex)
	if err != nil {
		log.Printf("mux segment %d: %v", segIndex, err)
		writeError(w, http.StatusBadGateway, "segment source unavailable")
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, segPath)
}

func (s *Server) handleHLSVideoPlaylist(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobId")
	job, ok := s.store.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "mux job not found")
		return
	}
	if err := s.muxer.EnsurePlaylist(r.Context(), job); err != nil {
		writeError(w, http.StatusBadGateway, "playlist not ready")
		return
	}
	if data, ok := s.muxer.PaddedVideoPlaylist(job); ok {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(data)
		return
	}
	if data, ok := s.muxer.PlaceholderVideoPlaylist(job); ok {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(data)
		return
	}
	path := s.muxer.VideoPlaylistPath(job)
	if path == "" {
		writeError(w, http.StatusInternalServerError, "video playlist not ready")
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, path)
}

func (s *Server) handleHLSAudioPlaylist(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobId")
	job, ok := s.store.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "mux job not found")
		return
	}
	if err := s.muxer.EnsurePlaylist(r.Context(), job); err != nil {
		writeError(w, http.StatusBadGateway, "playlist not ready")
		return
	}
	if data, ok := s.muxer.PaddedAudioPlaylist(job); ok {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(data)
		return
	}
	if data, ok := s.muxer.PlaceholderAudioPlaylist(job); ok {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(data)
		return
	}
	path := s.muxer.AudioPlaylistPath(job)
	if path == "" {
		writeError(w, http.StatusInternalServerError, "audio playlist not ready")
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, path)
}

// handleHLSAudioSegment serves an audio-only segment.
func (s *Server) handleHLSAudioSegment(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobId")
	segment := filepath.Base(r.PathValue("segment"))
	job, ok := s.store.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "mux job not found")
		return
	}

	var segIndex int
	if _, err := fmt.Sscanf(segment, "seg_%05d.ts", &segIndex); err != nil {
		writeError(w, http.StatusBadRequest, "invalid segment")
		return
	}

	if cached := s.muxer.AudioSegmentPath(job, segIndex); cached != "" {
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, cached)
		return
	}

	if err := s.muxer.EnsurePlaylist(r.Context(), job); err != nil {
		writeError(w, http.StatusBadGateway, "segment source unavailable")
		return
	}

	// Wait for the audio segment to be produced (same session as video).
	segPath, err := s.muxer.EnsureAudioSegment(r.Context(), job, segIndex)
	if err != nil {
		log.Printf("mux audio segment %d: %v", segIndex, err)
		writeError(w, http.StatusBadGateway, "segment source unavailable")
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, segPath)
}

// handleHLSVariantVideoPlaylist serves the video media playlist for an ABR
// variant, starting its generation on demand.
func (s *Server) handleHLSVariantVideoPlaylist(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobId")
	planIndex, err := parsePlanIndex(r.PathValue("planIndex"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid variant")
		return
	}
	job, ok := s.store.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "mux job not found")
		return
	}
	if err := s.muxer.EnsurePlaylist(r.Context(), job); err != nil {
		writeError(w, http.StatusBadGateway, "playlist not ready")
		return
	}
	path, err := s.muxer.EnsureVariant(r.Context(), job, planIndex)
	if err != nil {
		log.Printf("mux variant %d: %v", planIndex, err)
		writeError(w, http.StatusBadGateway, "variant source unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, path)
}

// handleHLSVariantAudioPlaylist serves the audio media playlist for an ABR
// variant.
func (s *Server) handleHLSVariantAudioPlaylist(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobId")
	planIndex, err := parsePlanIndex(r.PathValue("planIndex"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid variant")
		return
	}
	job, ok := s.store.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "mux job not found")
		return
	}
	if err := s.muxer.EnsurePlaylist(r.Context(), job); err != nil {
		writeError(w, http.StatusBadGateway, "playlist not ready")
		return
	}
	path, err := s.muxer.EnsureVariant(r.Context(), job, planIndex)
	if err != nil {
		log.Printf("mux variant %d: %v", planIndex, err)
		writeError(w, http.StatusBadGateway, "variant source unavailable")
		return
	}
	audioPath := filepath.Join(filepath.Dir(filepath.Dir(path)), "audio", "audio.m3u8")
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, audioPath)
}

// handleHLSVariantSegment serves a video segment for an ABR variant.
func (s *Server) handleHLSVariantSegment(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobId")
	planIndex, err := parsePlanIndex(r.PathValue("planIndex"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid variant")
		return
	}
	segment := filepath.Base(r.PathValue("segment"))
	job, ok := s.store.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "mux job not found")
		return
	}
	var segIndex int
	if _, err := fmt.Sscanf(segment, "seg_%05d.ts", &segIndex); err != nil {
		writeError(w, http.StatusBadRequest, "invalid segment")
		return
	}
	if err := s.muxer.EnsurePlaylist(r.Context(), job); err != nil {
		writeError(w, http.StatusBadGateway, "segment source unavailable")
		return
	}
	path, err := s.muxer.EnsureVariant(r.Context(), job, planIndex)
	if err != nil {
		log.Printf("mux variant %d segment: %v", planIndex, err)
		writeError(w, http.StatusBadGateway, "variant source unavailable")
		return
	}
	segPath := filepath.Join(filepath.Dir(path), fmt.Sprintf("seg_%05d.ts", segIndex))
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, segPath)
}

// handleHLSVariantAudioSegment serves an audio segment for an ABR variant.
func (s *Server) handleHLSVariantAudioSegment(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobId")
	planIndex, err := parsePlanIndex(r.PathValue("planIndex"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid variant")
		return
	}
	segment := filepath.Base(r.PathValue("segment"))
	job, ok := s.store.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "mux job not found")
		return
	}
	var segIndex int
	if _, err := fmt.Sscanf(segment, "seg_%05d.ts", &segIndex); err != nil {
		writeError(w, http.StatusBadRequest, "invalid segment")
		return
	}
	if err := s.muxer.EnsurePlaylist(r.Context(), job); err != nil {
		writeError(w, http.StatusBadGateway, "segment source unavailable")
		return
	}
	path, err := s.muxer.EnsureVariant(r.Context(), job, planIndex)
	if err != nil {
		log.Printf("mux variant %d audio segment: %v", planIndex, err)
		writeError(w, http.StatusBadGateway, "variant source unavailable")
		return
	}
	audioDir := filepath.Join(filepath.Dir(filepath.Dir(path)), "audio")
	segPath := filepath.Join(audioDir, fmt.Sprintf("seg_%05d.ts", segIndex))
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, segPath)
}

func parsePlanIndex(value string) (int, error) {
	var index int
	if _, err := fmt.Sscanf(value, "%d", &index); err != nil {
		return 0, err
	}
	if index < 0 {
		return 0, fmt.Errorf("negative plan index")
	}
	return index, nil
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
