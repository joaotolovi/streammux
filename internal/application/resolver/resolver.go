// Package resolver turns unresolved torrent streams (infoHash only) into
// direct streamable URLs using the user's configured debrid services.
package resolver

import (
	"context"

	"github.com/streammux/streammux/internal/application/debrid"
	"github.com/streammux/streammux/internal/domain/model"
)

// Resolver resolves torrents through the user's debrid services.
type Resolver struct{}

func New() *Resolver { return &Resolver{} }

// Resolve attempts to resolve a torrent (infoHash) into a direct URL using the
// user's enabled debrid services, in order. It returns the resolved URL and the
// service that resolved it, or an empty string when no service could resolve.
func (r *Resolver) Resolve(ctx context.Context, cfg *model.Config, infoHash, filename string) (string, string) {
	for _, svc := range cfg.ValidServices() {
		credential := serviceCredential(svc.ID, svc.Credentials)
		if credential == "" {
			continue
		}
		service, err := debrid.Factory(svc.ID, credential, "")
		if err != nil {
			continue
		}
		torrentSvc, ok := service.(debrid.TorrentService)
		if !ok {
			continue
		}
		url, err := torrentSvc.Resolve(ctx, debrid.TorrentInfo{
			Type:  "torrent",
			Hash:  infoHash,
			Title: filename,
		}, filename, true)
		if err != nil || url == "" {
			continue
		}
		return url, svc.ID
	}
	return "", ""
}

// serviceCredential returns the credential token for a debrid service.
func serviceCredential(serviceID string, creds map[string]string) string {
	switch serviceID {
	case "putio":
		return creds["clientId"] + "@" + creds["token"]
	case "pikpak":
		return creds["email"]
	default:
		return creds["apiKey"]
	}
}
