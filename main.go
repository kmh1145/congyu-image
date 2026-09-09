package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

//go:embed web/*
var webFS embed.FS

type Config struct {
	Addr       string
	DataDir    string
	BaseURL    string
	AdminUser  string
	AdminPass  string
	CookieSafe bool
}

func loadConfig() Config {
	dataDir := envOr("DATA_DIR", "/data")
	return Config{
		Addr:       envOr("LISTEN_ADDR", ":8080"),
		DataDir:    dataDir,
		BaseURL:    strings.TrimRight(os.Getenv("BASE_URL"), "/"),
		AdminUser:  envOr("ADMIN_USERNAME", "admin"),
		AdminPass:  os.Getenv("ADMIN_PASSWORD"),
		CookieSafe: strings.EqualFold(os.Getenv("COOKIE_SECURE"), "true"),
	}
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func main() {
	cfg := loadConfig()
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "images"), 0o750); err != nil {
		slog.Error("无法创建数据目录，请检查挂载卷权限", "path", cfg.DataDir, "error", err)
		os.Exit(1)
	}
	db, err := openDatabase(filepath.Join(cfg.DataDir, "congyu.db"))
	if err != nil {
		panic(err)
	}
	defer db.Close()

	generatedPass, err := migrateAndBootstrap(db, cfg)
	if err != nil {
		panic(err)
	}
	if generatedPass != "" {
		slog.Warn("ADMIN_PASSWORD 未设置，已生成一次性初始密码", "username", cfg.AdminUser, "password", generatedPass)
	}

	staticFS, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	app := newApplication(cfg, db, staticFS)
	rootCtx, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	go app.cleanupLoop(rootCtx)
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       90 * time.Second,
	}

	go func() {
		slog.Info("丛云图床已启动", "address", cfg.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("服务异常退出", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	cancelRoot()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
