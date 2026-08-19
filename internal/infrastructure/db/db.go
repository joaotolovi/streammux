package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/streammux/streammux/internal/domain/model"
	"github.com/streammux/streammux/internal/infrastructure/crypto"

	_ "modernc.org/sqlite"
)

type SQLiteUserRepository struct {
	db  *sql.DB
	enc *crypto.Encryptor
}

// NewSQLiteUserRepository opens the sqlite database at uri. It accepts both a
// raw file path and a "sqlite://" prefixed URI.
func NewSQLiteUserRepository(uri string, enc *crypto.Encryptor) (*SQLiteUserRepository, error) {
	dsn := strings.TrimPrefix(uri, "sqlite://")
	dsn = strings.TrimPrefix(dsn, "sqlite:")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	repo := &SQLiteUserRepository{db: db, enc: enc}
	if err := repo.migrate(); err != nil {
		return nil, err
	}
	return repo, nil
}

func (r *SQLiteUserRepository) migrate() error {
	_, err := r.db.Exec(`CREATE TABLE IF NOT EXISTS users (
		uuid TEXT PRIMARY KEY,
		password TEXT NOT NULL,
		config TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		accessed_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(`CREATE TABLE IF NOT EXISTS admin (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		password TEXT NOT NULL,
		user_uuid TEXT,
		user_password TEXT
	)`)
	return err
}

func (r *SQLiteUserRepository) HasAdminPassword(ctx context.Context) (bool, error) {
	var password string
	err := r.db.QueryRowContext(ctx, "SELECT password FROM admin WHERE id = 1").Scan(&password)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && password != "", err
}

func (r *SQLiteUserRepository) SetAdminPassword(ctx context.Context, password string) error {
	encPwd, err := r.enc.Encrypt(password)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO admin (id, password) VALUES (1, ?)
		ON CONFLICT(id) DO UPDATE SET password = excluded.password`, encPwd)
	return err
}

func (r *SQLiteUserRepository) VerifyAdminPassword(ctx context.Context, password string) (bool, error) {
	var encrypted string
	if err := r.db.QueryRowContext(ctx, "SELECT password FROM admin WHERE id = 1").Scan(&encrypted); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	stored, err := r.enc.Decrypt(encrypted)
	if err != nil {
		return false, err
	}
	return stored == password, nil
}

func (r *SQLiteUserRepository) GetAdminUser(ctx context.Context) (string, string, bool, error) {
	var uuid, password sql.NullString
	if err := r.db.QueryRowContext(ctx, "SELECT user_uuid, user_password FROM admin WHERE id = 1").Scan(&uuid, &password); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", false, nil
		}
		return "", "", false, err
	}
	if !uuid.Valid || !password.Valid || uuid.String == "" || password.String == "" {
		return "", "", false, nil
	}
	return uuid.String, password.String, true, nil
}

func (r *SQLiteUserRepository) SetAdminUser(ctx context.Context, uuid, encryptedPassword string) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO admin (id, password, user_uuid, user_password)
		VALUES (1, '', ?, ?)
		ON CONFLICT(id) DO UPDATE SET user_uuid = excluded.user_uuid, user_password = excluded.user_password`, uuid, encryptedPassword)
	return err
}

func (r *SQLiteUserRepository) Create(ctx context.Context, cfg *model.Config, password string) (string, string, error) {
	uuid := generateUUID()
	encPwd, err := r.enc.Encrypt(password)
	if err != nil {
		return "", "", err
	}
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return "", "", err
	}
	_, err = r.db.ExecContext(ctx, "INSERT INTO users (uuid, password, config) VALUES (?, ?, ?)", uuid, encPwd, string(configJSON))
	if err != nil {
		return "", "", err
	}
	return uuid, encPwd, nil
}

func (r *SQLiteUserRepository) Get(ctx context.Context, uuid, encryptedPassword string) (*model.Config, error) {
	var storedPwd, configJSON string
	err := r.db.QueryRowContext(ctx, "SELECT password, config FROM users WHERE uuid = ?", uuid).Scan(&storedPwd, &configJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("user not found")
		}
		return nil, err
	}
	if storedPwd != encryptedPassword {
		return nil, fmt.Errorf("invalid credentials")
	}
	go r.db.ExecContext(context.Background(), "UPDATE users SET accessed_at = CURRENT_TIMESTAMP WHERE uuid = ?", uuid)

	var cfg model.Config
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (r *SQLiteUserRepository) Update(ctx context.Context, uuid, encryptedPassword string, cfg *model.Config) error {
	var storedPwd string
	err := r.db.QueryRowContext(ctx, "SELECT password FROM users WHERE uuid = ?", uuid).Scan(&storedPwd)
	if err != nil {
		return errors.New("user not found")
	}
	if storedPwd != encryptedPassword {
		return fmt.Errorf("invalid credentials")
	}
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, "UPDATE users SET config = ? WHERE uuid = ?", string(configJSON), uuid)
	return err
}

func (r *SQLiteUserRepository) Delete(ctx context.Context, uuid, encryptedPassword string) error {
	var storedPwd string
	err := r.db.QueryRowContext(ctx, "SELECT password FROM users WHERE uuid = ?", uuid).Scan(&storedPwd)
	if err != nil {
		return errors.New("user not found")
	}
	if storedPwd != encryptedPassword {
		return fmt.Errorf("invalid credentials")
	}
	_, err = r.db.ExecContext(ctx, "DELETE FROM users WHERE uuid = ?", uuid)
	return err
}

func generateUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
