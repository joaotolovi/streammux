package db

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/streammux/streammux/internal/infrastructure/crypto"
)

func TestAdminPasswordAndUserArePersisted(t *testing.T) {
	repo, err := NewSQLiteUserRepository(filepath.Join(t.TempDir(), "streammux.db"), crypto.New("test-secret-key-that-is-long-enough"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	configured, err := repo.HasAdminPassword(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if configured {
		t.Fatal("new repository unexpectedly has an admin password")
	}
	if err := repo.SetAdminPassword(ctx, "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	valid, err := repo.VerifyAdminPassword(ctx, "correct horse battery staple")
	if err != nil || !valid {
		t.Fatalf("VerifyAdminPassword() = %v, %v", valid, err)
	}
	valid, err = repo.VerifyAdminPassword(ctx, "wrong password")
	if err != nil {
		t.Fatal(err)
	}
	if valid {
		t.Fatal("wrong admin password was accepted")
	}

	if err := repo.SetAdminUser(ctx, "user-123", "encrypted-user-password"); err != nil {
		t.Fatal(err)
	}
	uuid, password, ok, err := repo.GetAdminUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || uuid != "user-123" || password != "encrypted-user-password" {
		t.Fatalf("GetAdminUser() = %q, %q, %v", uuid, password, ok)
	}
}
