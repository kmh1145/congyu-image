package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type objectStore interface {
	Put(context.Context, string, io.Reader, int64, string) error
	Open(context.Context, string) (io.ReadCloser, error)
	Delete(context.Context, string) error
	Capacity(context.Context, int64) (total, used, free int64)
}

type localStore struct{ root string }

func (s *localStore) fullPath(key string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(key))
	if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return "", errors.New("invalid object key")
	}
	full := filepath.Join(s.root, clean)
	rel, err := filepath.Rel(s.root, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", errors.New("object key escaped storage root")
	}
	return full, nil
}

func (s *localStore) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	full, err := s.fullPath(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".upload-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, full)
}

func (s *localStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	full, err := s.fullPath(key)
	if err != nil {
		return nil, err
	}
	return os.Open(full)
}

func (s *localStore) Delete(_ context.Context, key string) error {
	full, err := s.fullPath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *localStore) Capacity(_ context.Context, used int64) (int64, int64, int64) {
	return diskCapacity(s.root, used)
}

type s3Config struct {
	Endpoint      string `json:"endpoint"`
	AccessKey     string `json:"access_key"`
	SecretKey     string `json:"secret_key"`
	Bucket        string `json:"bucket"`
	Region        string `json:"region"`
	Prefix        string `json:"prefix"`
	UseSSL        bool   `json:"use_ssl"`
	CapacityBytes int64  `json:"capacity_bytes"`
}

type s3Store struct {
	client *minio.Client
	cfg    s3Config
}

func newS3Store(raw string) (*s3Store, error) {
	var cfg s3Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, err
	}
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("S3 endpoint、bucket、access_key、secret_key 均为必填")
	}
	client, err := minio.New(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(cfg.Endpoint, "https://"), "http://"), "/"), &minio.Options{
		Creds: credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""), Secure: cfg.UseSSL, Region: cfg.Region,
	})
	if err != nil {
		return nil, err
	}
	return &s3Store{client: client, cfg: cfg}, nil
}

func (s *s3Store) objectKey(key string) string {
	return path.Join(strings.Trim(s.cfg.Prefix, "/"), key)
}
func (s *s3Store) Put(ctx context.Context, key string, r io.Reader, size int64, mime string) error {
	_, err := s.client.PutObject(ctx, s.cfg.Bucket, s.objectKey(key), r, size, minio.PutObjectOptions{ContentType: mime})
	return err
}
func (s *s3Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.cfg.Bucket, s.objectKey(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	if _, err = obj.Stat(); err != nil {
		obj.Close()
		return nil, err
	}
	return obj, nil
}
func (s *s3Store) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.cfg.Bucket, s.objectKey(key), minio.RemoveObjectOptions{})
}
func (s *s3Store) Capacity(_ context.Context, used int64) (int64, int64, int64) {
	if s.cfg.CapacityBytes <= 0 {
		return 0, used, 0
	}
	return s.cfg.CapacityBytes, used, max(0, s.cfg.CapacityBytes-used)
}

type webDAVConfig struct {
	URL           string `json:"url"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	Prefix        string `json:"prefix"`
	CapacityBytes int64  `json:"capacity_bytes"`
}

type webDAVStore struct {
	cfg    webDAVConfig
	client *http.Client
}

func newWebDAVStore(raw string) (*webDAVStore, error) {
	var cfg webDAVConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, err
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("WebDAV URL 无效")
	}
	return &webDAVStore{cfg: cfg, client: &http.Client{Timeout: 2 * time.Minute}}, nil
}

func (s *webDAVStore) objectURL(key string) string {
	parts := strings.Split(path.Join(s.cfg.Prefix, key), "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.TrimRight(s.cfg.URL, "/") + "/" + strings.Join(parts, "/")
}

func (s *webDAVStore) request(ctx context.Context, method, key string, body io.Reader) (*http.Response, error) {
	return s.requestURL(ctx, method, s.objectURL(key), body)
}

func (s *webDAVStore) requestURL(ctx context.Context, method, target string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if s.cfg.Username != "" {
		req.SetBasicAuth(s.cfg.Username, s.cfg.Password)
	}
	return s.client.Do(req)
}

func (s *webDAVStore) ensureDirs(ctx context.Context, key string) error {
	dir := path.Dir(path.Join(s.cfg.Prefix, key))
	if dir == "." || dir == "/" {
		return nil
	}
	parts := strings.Split(strings.Trim(dir, "/"), "/")
	for i := range parts {
		escaped := make([]string, i+1)
		for j := range escaped {
			escaped[j] = url.PathEscape(parts[j])
		}
		target := strings.TrimRight(s.cfg.URL, "/") + "/" + strings.Join(escaped, "/")
		resp, err := s.requestURL(ctx, "MKCOL", target, nil)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 && resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusConflict {
			return fmt.Errorf("WebDAV MKCOL: %s", resp.Status)
		}
	}
	return nil
}

func (s *webDAVStore) Put(ctx context.Context, key string, r io.Reader, _ int64, mime string) error {
	if err := s.ensureDirs(ctx, key); err != nil {
		return err
	}
	resp, err := s.request(ctx, http.MethodPut, key, r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("WebDAV PUT: %s", resp.Status)
	}
	return nil
}

func (s *webDAVStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := s.request(ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("WebDAV GET: %s", resp.Status)
	}
	return resp.Body, nil
}

func (s *webDAVStore) Delete(ctx context.Context, key string) error {
	resp, err := s.request(ctx, http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("WebDAV DELETE: %s", resp.Status)
	}
	return nil
}
func (s *webDAVStore) Capacity(_ context.Context, used int64) (int64, int64, int64) {
	if s.cfg.CapacityBytes <= 0 {
		return 0, used, 0
	}
	return s.cfg.CapacityBytes, used, max(0, s.cfg.CapacityBytes-used)
}

func validateStorageConfig(kind, raw string) error {
	switch kind {
	case "local":
		return nil
	case "s3":
		_, err := newS3Store(raw)
		return err
	case "webdav":
		_, err := newWebDAVStore(raw)
		return err
	default:
		return errors.New("不支持的存储类型")
	}
}

func redactStorageConfig(kind, raw string) string {
	var values map[string]any
	if json.Unmarshal([]byte(raw), &values) != nil {
		return "{}"
	}
	for _, key := range []string{"secret_key", "password"} {
		if _, ok := values[key]; ok {
			values[key] = "••••••••"
		}
	}
	b, _ := json.Marshal(values)
	return string(b)
}

func restoreStorageSecrets(kind, incoming, existing string) string {
	var next, old map[string]any
	if json.Unmarshal([]byte(incoming), &next) != nil || json.Unmarshal([]byte(existing), &old) != nil {
		return incoming
	}
	for _, key := range []string{"secret_key", "password"} {
		if value, ok := next[key].(string); ok && value == "••••••••" {
			next[key] = old[key]
		}
	}
	b, _ := json.Marshal(next)
	return string(b)
}
