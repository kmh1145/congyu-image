package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

type User struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	IsAdmin      bool   `json:"is_admin"`
	MaxFileSize  int64  `json:"max_file_size"`
	AllowedTypes string `json:"allowed_types"`
	QuotaBytes   int64  `json:"quota_bytes"`
	UsedBytes    int64  `json:"used_bytes"`
	CreatedAt    string `json:"created_at"`
}

type Settings struct {
	SiteName            string `json:"site_name"`
	SiteDomain          string `json:"site_domain"`
	BackgroundURL       string `json:"background_url"`
	BackgroundOpacity   int    `json:"background_opacity"`
	BackgroundPosition  string `json:"background_position"`
	RegistrationEnabled bool   `json:"registration_enabled"`
	GuestUploadEnabled  bool   `json:"guest_upload_enabled"`
	GuestAllowedTypes   string `json:"guest_allowed_types"`
	GuestMaxFileSize    int64  `json:"guest_max_file_size"`
	GuestExpiryHours    int    `json:"guest_expiry_hours"`
	GalleryEnabled      bool   `json:"gallery_enabled"`
	ThemeColor          string `json:"theme_color"`
	WebPQuality         int    `json:"webp_quality"`
}

type ImageRecord struct {
	ID           int64   `json:"id"`
	UserID       *int64  `json:"user_id,omitempty"`
	Username     string  `json:"username,omitempty"`
	StorageID    int64   `json:"storage_id"`
	OriginalName string  `json:"original_name"`
	MIMEType     string  `json:"mime_type"`
	Size         int64   `json:"size"`
	Width        int     `json:"width"`
	Height       int     `json:"height"`
	Private      bool    `json:"is_private"`
	ShareToken   string  `json:"-"`
	StorageKey   string  `json:"-"`
	ExpiresAt    *string `json:"expires_at,omitempty"`
	CreatedAt    string  `json:"created_at"`
	URL          string  `json:"url"`
	Markdown     string  `json:"markdown"`
}

type StorageRecord struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	ConfigJSON string `json:"config_json,omitempty"`
	IsDefault  bool   `json:"is_default"`
	Enabled    bool   `json:"enabled"`
	TotalBytes int64  `json:"total_bytes"`
	UsedBytes  int64  `json:"used_bytes"`
	FreeBytes  int64  `json:"free_bytes"`
}

func openDatabase(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(time.Hour)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	return db, nil
}

func migrateAndBootstrap(db *sql.DB, cfg Config) (string, error) {
	schema := `
CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL UNIQUE COLLATE NOCASE,
  password_hash TEXT NOT NULL,
  is_admin INTEGER NOT NULL DEFAULT 0,
  max_file_size INTEGER NOT NULL DEFAULT 10485760,
  allowed_types TEXT NOT NULL DEFAULT 'image/jpeg,image/png,image/gif,image/webp',
  quota_bytes INTEGER NOT NULL DEFAULT 1073741824,
  used_bytes INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS settings (
  id INTEGER PRIMARY KEY CHECK(id = 1),
  site_name TEXT NOT NULL DEFAULT '丛云图床', site_domain TEXT NOT NULL DEFAULT '',
  background_url TEXT NOT NULL DEFAULT '', background_opacity INTEGER NOT NULL DEFAULT 18,
  background_position TEXT NOT NULL DEFAULT 'center', registration_enabled INTEGER NOT NULL DEFAULT 1,
  guest_upload_enabled INTEGER NOT NULL DEFAULT 0,
  guest_allowed_types TEXT NOT NULL DEFAULT 'image/jpeg,image/png,image/webp',
  guest_max_file_size INTEGER NOT NULL DEFAULT 5242880, guest_expiry_hours INTEGER NOT NULL DEFAULT 24,
  gallery_enabled INTEGER NOT NULL DEFAULT 1, theme_color TEXT NOT NULL DEFAULT '#3b82f6',
  webp_quality INTEGER NOT NULL DEFAULT 82
);
INSERT OR IGNORE INTO settings(id) VALUES(1);
CREATE TABLE IF NOT EXISTS storages (
  id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, type TEXT NOT NULL,
  config_json TEXT NOT NULL DEFAULT '{}', is_default INTEGER NOT NULL DEFAULT 0,
  enabled INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_storage_default ON storages(is_default) WHERE is_default = 1;
CREATE TABLE IF NOT EXISTS images (
  id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
  storage_id INTEGER NOT NULL REFERENCES storages(id), original_name TEXT NOT NULL,
  mime_type TEXT NOT NULL, size INTEGER NOT NULL, width INTEGER NOT NULL, height INTEGER NOT NULL,
  is_private INTEGER NOT NULL DEFAULT 0, share_token TEXT NOT NULL UNIQUE, storage_key TEXT NOT NULL UNIQUE,
  expires_at TEXT, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_images_public ON images(is_private, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_images_user ON images(user_id, created_at DESC);
CREATE TABLE IF NOT EXISTS sessions (
  token_hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_sessions_expiry ON sessions(expires_at);
INSERT OR IGNORE INTO storages(id,name,type,config_json,is_default,enabled)
VALUES(1,'本机存储','local','{}',1,1);`
	if _, err := db.Exec(schema); err != nil {
		return "", err
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM users WHERE is_admin=1").Scan(&count); err != nil {
		return "", err
	}
	if count > 0 {
		return "", nil
	}
	password := cfg.AdminPass
	generated := ""
	if password == "" {
		password = randomToken(18)
		generated = password
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	_, err = db.Exec(`INSERT INTO users(username,password_hash,is_admin,max_file_size,allowed_types,quota_bytes)
VALUES(?,?,1,52428800,'image/jpeg,image/png,image/gif,image/webp',10737418240)`, cfg.AdminUser, hash)
	return generated, err
}

func randomToken(bytesLen int) string {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("secure random: %w", err))
	}
	return strings.TrimRight(base64.RawURLEncoding.EncodeToString(b), "=")
}
