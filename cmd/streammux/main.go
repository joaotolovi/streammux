package main

import (
	"context"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/streammux/streammux/internal/application/assets"
	"github.com/streammux/streammux/internal/application/collector"
	"github.com/streammux/streammux/internal/application/ffmpeg"
	"github.com/streammux/streammux/internal/application/muxer"
	"github.com/streammux/streammux/internal/application/planner"
	"github.com/streammux/streammux/internal/application/resolver"
	"github.com/streammux/streammux/internal/infrastructure/crypto"
	"github.com/streammux/streammux/internal/infrastructure/db"
	"github.com/streammux/streammux/internal/infrastructure/store"
	streammuxhttp "github.com/streammux/streammux/internal/interface/http"
	"github.com/streammux/streammux/web"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func mustWebFS() fs.FS {
	sub, err := web.Sub()
	if err != nil {
		log.Fatalf("web assets: %v", err)
	}
	return sub
}

func main() {
	secretKey := envOr("SECRET_KEY", "streammux-default-secret-key-change-me")
	port := envOr("PORT", "3001")
	databaseURI := envOr("DATABASE_URI", "sqlite:///data/streammux.db")
	baseURL := envOr("BASE_URL", "http://localhost:"+port)

	enc := crypto.New(secretKey)
	users, err := db.NewSQLiteUserRepository(databaseURI, enc)
	if err != nil {
		log.Fatalf("database: %v", err)
	}

	store := store.NewMemoryStore(30 * time.Minute)
	collector := collector.New()
	planner := planner.New()
	ff := ffmpeg.New(envOr("FFMPEG_PATH", "ffmpeg"))
	res := resolver.New()

	placeholderPath := envOr("PLACEHOLDER_VIDEO", "")
	if placeholderPath == "" {
		placeholderPath = envOr("PLACEHOLDER_INTRO", "")
	}
	if placeholderPath == "" {
		placeholderPath = envOr("PLACEHOLDER_LOOP", "")
	}
	assetsDir := ""
	errorAssetsDir := ""
	errorPath := envOr("ERROR_VIDEO", "")
	if placeholderPath == "" {
		var err error
		placeholderPath, assetsDir, err = assets.PlaceholderPath()
		if err != nil {
			log.Fatalf("placeholder assets: %v", err)
		}
	}
	if errorPath == "" {
		var err error
		errorPath, errorAssetsDir, err = assets.ErrorPath()
		if err != nil {
			log.Fatalf("error assets: %v", err)
		}
	}
	mux := muxer.NewWithErrorPlaceholder(collector, planner, ff, res, store, baseURL, placeholderPath, errorPath)
	store.SetOnDelete(mux.CleanupJob)
	if assetsDir != "" {
		defer os.RemoveAll(assetsDir)
	}
	if errorAssetsDir != "" {
		defer os.RemoveAll(errorAssetsDir)
	}

	srv := streammuxhttp.New(users, store, mux, streammuxhttp.Options{
		BaseURL: baseURL,
		WebFS:   mustWebFS(),
	})

	httpServer := &http.Server{
		Addr:    ":" + port,
		Handler: srv,
	}

	go func() {
		log.Printf("StreamMux listening on :%s", port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpServer.Shutdown(ctx)
}
