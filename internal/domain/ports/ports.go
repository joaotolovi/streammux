package ports

import (
	"context"

	"github.com/streammux/streammux/internal/domain/model"
)

type UserRepository interface {
	// Create persists a new config, returning the uuid and the encrypted
	// password used to reference this user on subsequent Stremio routes.
	Create(ctx context.Context, cfg *model.Config, password string) (uuid, encryptedPassword string, err error)
	// Get resolves a config from a uuid and the encrypted password that was
	// returned by Create.
	Get(ctx context.Context, uuid, encryptedPassword string) (*model.Config, error)
	Update(ctx context.Context, uuid, encryptedPassword string, cfg *model.Config) error
	Delete(ctx context.Context, uuid, encryptedPassword string) error
}

type MuxStore interface {
	Save(job *model.MuxJob) string
	Get(id string) (*model.MuxJob, bool)
	Delete(id string)
	Cleanup()
}
