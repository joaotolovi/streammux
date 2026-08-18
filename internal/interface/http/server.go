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
	"time"

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

	// HLS endpoints — master, renditions and segments.
	s.mux.HandleFunc("GET /mux/{jobId}", s.handleMuxRedirect)
	s.mux.HandleFunc("GET /mux/{jobId}/playlist.m3u8", s.handleHLSMaster)
	s.mux.HandleFunc("GET /mux/{jobId}/video/video.m3u8", s.handleHLSVideoPlaylist)
	s.mux.HandleFunc("GET /mux/{jobId}/audio/audio.m3u8", s.handleHLSAudioPlaylist)
	s.mux.HandleFunc("GET /mux/{jobId}/video/{segment}", s.handleHLSSegment)
	s.mux.HandleFunc("GET /mux/{jobId}/audio/{segment}", s.handleHLSAudioSegment)

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
	sw := &statusWriter{ResponseWriter: w, status: 200}
	s.mux.ServeHTTP(sw, r)
	if sw.status >= 400 {
		log.Printf("http %d %s %s", sw.status, r.Method, r.URL.Path)
	}
}

// statusWriter records the response status for request logging.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
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
	manifest := model.Manifest{
		ID:          "com.streammux.viren070",
		Version:     "1.0.0",
		Name:        "StreamMux",
		Description: "Combina a melhor qualidade de vídeo com o áudio no seu idioma.",
		Types:       []string{"movie", "series"},
		Resources:   []string{constants.StreamResource},
	}
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

// handleHLSMaster starts playback (if needed) and serves our master playlist:
// one variant with the audio declared as a rendition group.
func (s *Server) handleHLSMaster(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobId")
	job, ok := s.store.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "mux job not found")
		return
	}

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

	data, ok := s.muxer.MasterPlaylist(job)
	if !ok {
		writeError(w, http.StatusInternalServerError, "playlist not ready")
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

func (s *Server) handleHLSVideoPlaylist(w http.ResponseWriter, r *http.Request) {
	s.serveVodPlaylist(w, r, s.muxer.VideoPlaylist)
}

func (s *Server) handleHLSAudioPlaylist(w http.ResponseWriter, r *http.Request) {
	s.serveVodPlaylist(w, r, s.muxer.AudioPlaylist)
}

func (s *Server) serveVodPlaylist(w http.ResponseWriter, r *http.Request, render func(*model.MuxJob) ([]byte, bool)) {
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
	data, ok := render(job)
	if !ok {
		writeError(w, http.StatusInternalServerError, "playlist not ready")
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

// countingWriter tracks how many bytes were actually written to the player,
// and how long the response took, so the muxer can measure player throughput.
type countingWriter struct {
	http.ResponseWriter
	bytes int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.bytes += int64(n)
	return n, err
}

func (s *Server) handleHLSSegment(w http.ResponseWriter, r *http.Request) {
	s.serveSegment(w, r, false)
}

func (s *Server) handleHLSAudioSegment(w http.ResponseWriter, r *http.Request) {
	s.serveSegment(w, r, true)
}

func (s *Server) serveSegment(w http.ResponseWriter, r *http.Request, audio bool) {
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

	var cached string
	if audio {
		cached = s.muxer.AudioSegmentPath(job, segIndex)
	} else {
		cached = s.muxer.SegmentPath(job, segIndex)
	}
	if cached != "" {
		s.deliverSegment(w, r, job, cached, audio)
		return
	}

	if err := s.muxer.EnsurePlaylist(r.Context(), job); err != nil {
		writeError(w, http.StatusBadGateway, "segment source unavailable")
		return
	}

	var (
		segPath string
		err     error
	)
	if audio {
		segPath, err = s.muxer.EnsureAudioSegment(r.Context(), job, segIndex)
	} else {
		segPath, err = s.muxer.EnsureSegment(r.Context(), job, segIndex)
	}
	if err != nil {
		if errors.Is(err, muxer.ErrBeyondEnd) {
			writeError(w, http.StatusNotFound, "segment beyond end of film")
			return
		}
		log.Printf("mux segment %d (audio=%v): %v", segIndex, audio, err)
		writeError(w, http.StatusBadGateway, "segment source unavailable")
		return
	}

	s.deliverSegment(w, r, job, segPath, audio)
}

// deliverSegment writes the segment file and reports video delivery metrics
// to the muxer so it can detect a player-side bandwidth bottleneck.
func (s *Server) deliverSegment(w http.ResponseWriter, r *http.Request, job *model.MuxJob, path string, audio bool) {
	w.Header().Set("Cache-Control", "no-store")
	if audio {
		http.ServeFile(w, r, path)
		return
	}
	start := time.Now()
	cw := &countingWriter{ResponseWriter: w}
	http.ServeFile(cw, r, path)
	go s.muxer.ObserveDelivery(job, cw.bytes, time.Since(start))
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
