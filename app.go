package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type application struct {
	cfg       Config
	db        *sql.DB
	static    fs.FS
	startTime time.Time
	limiter   *ipLimiter
}

type contextKey string

const userContextKey contextKey = "user"

func newApplication(cfg Config, db *sql.DB, static fs.FS) *application {
	return &application{cfg: cfg, db: db, static: static, startTime: time.Now(), limiter: newIPLimiter(30, time.Minute)}
}

func (a *application) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /api/bootstrap", a.bootstrap)
	mux.HandleFunc("POST /api/auth/login", a.login)
	mux.HandleFunc("POST /api/auth/register", a.register)
	mux.HandleFunc("POST /api/auth/logout", a.logout)
	mux.HandleFunc("GET /api/images", a.listImages)
	mux.HandleFunc("POST /api/upload", a.uploadFiles)
	mux.HandleFunc("POST /api/upload/url", a.uploadURL)
	mux.HandleFunc("DELETE /api/images/{id}", a.deleteImage)
	mux.HandleFunc("GET /media/{id}/{token}", a.serveImage)
	mux.HandleFunc("GET /media/{id}", a.serveImage)
	mux.HandleFunc("GET /api/admin/users", a.adminListUsers)
	mux.HandleFunc("PUT /api/admin/users/{id}", a.adminUpdateUser)
	mux.HandleFunc("DELETE /api/admin/users/{id}", a.adminDeleteUser)
	mux.HandleFunc("GET /api/admin/settings", a.adminGetSettings)
	mux.HandleFunc("PUT /api/admin/settings", a.adminUpdateSettings)
	mux.HandleFunc("GET /api/admin/storages", a.adminListStorages)
	mux.HandleFunc("POST /api/admin/storages", a.adminCreateStorage)
	mux.HandleFunc("PUT /api/admin/storages/{id}", a.adminUpdateStorage)
	mux.HandleFunc("DELETE /api/admin/storages/{id}", a.adminDeleteStorage)
	mux.HandleFunc("GET /api/admin/stats", a.adminStats)
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(a.static))))
	mux.HandleFunc("GET /", a.index)
	return a.middleware(mux)
}

func (a *application) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob: https: http:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")

		if isMutation(r.Method) && !validSameOrigin(r) {
			writeError(w, http.StatusForbidden, "请求来源校验失败")
			return
		}
		if user := a.userFromRequest(r); user != nil {
			r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
		}
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			slog.Debug("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(started))
		}
	})
}

func isMutation(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func validSameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Header.Get("X-Congyu-Request") == "1" || !strings.Contains(strings.ToLower(r.Header.Get("User-Agent")), "mozilla")
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func (a *application) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(a.static, "index.html")
	if err != nil {
		http.Error(w, "frontend unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

func (a *application) health(w http.ResponseWriter, _ *http.Request) {
	if err := a.db.Ping(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "uptime_seconds": int(time.Since(a.startTime).Seconds())})
}

func (a *application) bootstrap(w http.ResponseWriter, r *http.Request) {
	settings, err := a.loadSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "无法读取网站设置")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings, "user": currentUser(r)})
}

func (a *application) login(w http.ResponseWriter, r *http.Request) {
	if !a.limiter.Allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "尝试次数过多，请稍后再试")
		return
	}
	var input struct{ Username, Password string }
	if !decodeJSON(w, r, &input, 32<<10) {
		return
	}
	var user User
	var hash string
	err := a.db.QueryRow(`SELECT id,username,password_hash,is_admin,max_file_size,allowed_types,quota_bytes,used_bytes,created_at
FROM users WHERE username=?`, strings.TrimSpace(input.Username)).Scan(&user.ID, &user.Username, &hash, &user.IsAdmin, &user.MaxFileSize, &user.AllowedTypes, &user.QuotaBytes, &user.UsedBytes, &user.CreatedAt)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(input.Password)) != nil {
		time.Sleep(250 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	if err := a.createSession(w, user.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "登录失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
}

func (a *application) register(w http.ResponseWriter, r *http.Request) {
	if !a.limiter.Allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "尝试次数过多，请稍后再试")
		return
	}
	settings, err := a.loadSettings()
	if err != nil || !settings.RegistrationEnabled {
		writeError(w, http.StatusForbidden, "当前未开放注册")
		return
	}
	var input struct{ Username, Password string }
	if !decodeJSON(w, r, &input, 32<<10) {
		return
	}
	input.Username = strings.TrimSpace(input.Username)
	if len([]rune(input.Username)) < 3 || len([]rune(input.Username)) > 32 || strings.ContainsAny(input.Username, " /\\\t\r\n") {
		writeError(w, http.StatusBadRequest, "用户名应为 3–32 个字符且不能包含空白或斜杠")
		return
	}
	if len(input.Password) < 8 || len(input.Password) > 128 {
		writeError(w, http.StatusBadRequest, "密码长度应为 8–128 个字符")
		return
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
	result, err := a.db.Exec(`INSERT INTO users(username,password_hash,is_admin,max_file_size,allowed_types,quota_bytes)
VALUES(?,?,0,10485760,'image/jpeg,image/png,image/gif,image/webp',1073741824)`, input.Username, hash)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			writeError(w, http.StatusConflict, "用户名已存在")
			return
		}
		writeError(w, http.StatusInternalServerError, "注册失败")
		return
	}
	id, _ := result.LastInsertId()
	if err := a.createSession(w, id); err != nil {
		writeError(w, http.StatusInternalServerError, "注册成功，但自动登录失败")
		return
	}
	var user User
	_ = a.db.QueryRow(`SELECT id,username,is_admin,max_file_size,allowed_types,quota_bytes,used_bytes,created_at FROM users WHERE id=?`, id).
		Scan(&user.ID, &user.Username, &user.IsAdmin, &user.MaxFileSize, &user.AllowedTypes, &user.QuotaBytes, &user.UsedBytes, &user.CreatedAt)
	writeJSON(w, http.StatusCreated, map[string]any{"user": user})
}

func (a *application) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("congyu_session"); err == nil {
		_, _ = a.db.Exec("DELETE FROM sessions WHERE token_hash=?", tokenHash(cookie.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: "congyu_session", Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.cfg.CookieSafe, MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *application) createSession(w http.ResponseWriter, userID int64) error {
	token := randomToken(32)
	expires := time.Now().UTC().Add(30 * 24 * time.Hour)
	_, err := a.db.Exec("INSERT INTO sessions(token_hash,user_id,expires_at) VALUES(?,?,?)", tokenHash(token), userID, expires.Format(time.RFC3339))
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: "congyu_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.cfg.CookieSafe, Expires: expires, MaxAge: 30 * 86400})
	return nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (a *application) userFromRequest(r *http.Request) *User {
	cookie, err := r.Cookie("congyu_session")
	if err != nil || len(cookie.Value) < 32 {
		return nil
	}
	var user User
	err = a.db.QueryRow(`SELECT u.id,u.username,u.is_admin,u.max_file_size,u.allowed_types,u.quota_bytes,u.used_bytes,u.created_at
FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=? AND s.expires_at>?`, tokenHash(cookie.Value), time.Now().UTC().Format(time.RFC3339)).
		Scan(&user.ID, &user.Username, &user.IsAdmin, &user.MaxFileSize, &user.AllowedTypes, &user.QuotaBytes, &user.UsedBytes, &user.CreatedAt)
	if err != nil {
		return nil
	}
	return &user
}

func currentUser(r *http.Request) *User {
	user, _ := r.Context().Value(userContextKey).(*User)
	return user
}

func requireUser(w http.ResponseWriter, r *http.Request) *User {
	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "请先登录")
	}
	return user
}

func requireAdmin(w http.ResponseWriter, r *http.Request) *User {
	user := requireUser(w, r)
	if user != nil && !user.IsAdmin {
		writeError(w, http.StatusForbidden, "需要管理员权限")
		return nil
	}
	return user
}

func (a *application) loadSettings() (Settings, error) {
	var s Settings
	err := a.db.QueryRow(`SELECT site_name,site_domain,background_url,background_opacity,background_position,
registration_enabled,guest_upload_enabled,guest_allowed_types,guest_max_file_size,guest_expiry_hours,
gallery_enabled,theme_color,webp_quality FROM settings WHERE id=1`).Scan(&s.SiteName, &s.SiteDomain, &s.BackgroundURL,
		&s.BackgroundOpacity, &s.BackgroundPosition, &s.RegistrationEnabled, &s.GuestUploadEnabled,
		&s.GuestAllowedTypes, &s.GuestMaxFileSize, &s.GuestExpiryHours, &s.GalleryEnabled, &s.ThemeColor, &s.WebPQuality)
	return s, err
}

func (a *application) adminGetSettings(w http.ResponseWriter, r *http.Request) {
	if requireAdmin(w, r) == nil {
		return
	}
	s, err := a.loadSettings()
	if err != nil {
		writeError(w, 500, "读取设置失败")
		return
	}
	writeJSON(w, 200, s)
}

func (a *application) adminUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if requireAdmin(w, r) == nil {
		return
	}
	var s Settings
	if !decodeJSON(w, r, &s, 128<<10) {
		return
	}
	if s.SiteName == "" || s.WebPQuality < 1 || s.WebPQuality > 100 || s.BackgroundOpacity < 0 || s.BackgroundOpacity > 100 ||
		s.GuestMaxFileSize < 1024 || s.GuestExpiryHours < 1 || !validHexColor(s.ThemeColor) || !validPosition(s.BackgroundPosition) {
		writeError(w, 400, "设置中存在无效值")
		return
	}
	if !validTypeList(s.GuestAllowedTypes) {
		writeError(w, 400, "游客文件类型设置无效")
		return
	}
	_, err := a.db.Exec(`UPDATE settings SET site_name=?,site_domain=?,background_url=?,background_opacity=?,background_position=?,
registration_enabled=?,guest_upload_enabled=?,guest_allowed_types=?,guest_max_file_size=?,guest_expiry_hours=?,gallery_enabled=?,theme_color=?,webp_quality=? WHERE id=1`,
		s.SiteName, strings.TrimRight(s.SiteDomain, "/"), s.BackgroundURL, s.BackgroundOpacity, s.BackgroundPosition, s.RegistrationEnabled,
		s.GuestUploadEnabled, s.GuestAllowedTypes, s.GuestMaxFileSize, s.GuestExpiryHours, s.GalleryEnabled, s.ThemeColor, s.WebPQuality)
	if err != nil {
		writeError(w, 500, "保存设置失败")
		return
	}
	writeJSON(w, 200, s)
}

func (a *application) adminListUsers(w http.ResponseWriter, r *http.Request) {
	if requireAdmin(w, r) == nil {
		return
	}
	rows, err := a.db.Query(`SELECT id,username,is_admin,max_file_size,allowed_types,quota_bytes,used_bytes,created_at FROM users ORDER BY is_admin DESC,id`)
	if err != nil {
		writeError(w, 500, "读取用户失败")
		return
	}
	defer rows.Close()
	users := []User{}
	for rows.Next() {
		var u User
		if rows.Scan(&u.ID, &u.Username, &u.IsAdmin, &u.MaxFileSize, &u.AllowedTypes, &u.QuotaBytes, &u.UsedBytes, &u.CreatedAt) == nil {
			users = append(users, u)
		}
	}
	writeJSON(w, 200, users)
}

func (a *application) adminUpdateUser(w http.ResponseWriter, r *http.Request) {
	admin := requireAdmin(w, r)
	if admin == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "用户 ID 无效")
		return
	}
	var input struct {
		IsAdmin      bool   `json:"is_admin"`
		MaxFileSize  int64  `json:"max_file_size"`
		AllowedTypes string `json:"allowed_types"`
		QuotaBytes   int64  `json:"quota_bytes"`
	}
	if !decodeJSON(w, r, &input, 32<<10) {
		return
	}
	if input.MaxFileSize < 1024 || input.QuotaBytes < 0 || !validTypeList(input.AllowedTypes) {
		writeError(w, 400, "用户权限设置无效")
		return
	}
	if id == admin.ID && !input.IsAdmin {
		writeError(w, 400, "不能解除自己的管理员权限")
		return
	}
	result, err := a.db.Exec("UPDATE users SET is_admin=?,max_file_size=?,allowed_types=?,quota_bytes=? WHERE id=?", input.IsAdmin, input.MaxFileSize, input.AllowedTypes, input.QuotaBytes, id)
	if err != nil {
		writeError(w, 500, "更新用户失败")
		return
	}
	if n, _ := result.RowsAffected(); n == 0 {
		writeError(w, 404, "用户不存在")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *application) adminDeleteUser(w http.ResponseWriter, r *http.Request) {
	admin := requireAdmin(w, r)
	if admin == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "用户 ID 无效")
		return
	}
	if id == admin.ID {
		writeError(w, 400, "不能删除当前登录用户")
		return
	}
	var count int
	_ = a.db.QueryRow("SELECT COUNT(*) FROM images WHERE user_id=?", id).Scan(&count)
	if count > 0 {
		writeError(w, 409, "请先删除或迁移该用户的图片")
		return
	}
	result, err := a.db.Exec("DELETE FROM users WHERE id=?", id)
	if err != nil {
		writeError(w, 500, "删除用户失败")
		return
	}
	if n, _ := result.RowsAffected(); n == 0 {
		writeError(w, 404, "用户不存在")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func validHexColor(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	_, err := hex.DecodeString(s[1:])
	return err == nil
}
func validPosition(s string) bool {
	return s == "center" || s == "top" || s == "bottom" || s == "left" || s == "right"
}
func validTypeList(s string) bool {
	allowed := map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true}
	parts := strings.Split(s, ",")
	if len(parts) == 0 {
		return false
	}
	for _, p := range parts {
		if !allowed[strings.TrimSpace(p)] {
			return false
		}
	}
	return true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, 400, "请求数据格式错误")
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

type ipLimiter struct {
	mu      sync.Mutex
	entries map[string]*limitEntry
	limit   int
	window  time.Duration
}
type limitEntry struct {
	count int
	reset time.Time
}

func newIPLimiter(limit int, window time.Duration) *ipLimiter {
	return &ipLimiter{entries: map[string]*limitEntry{}, limit: limit, window: window}
}
func (l *ipLimiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	e := l.entries[ip]
	if e == nil || now.After(e.reset) {
		l.entries[ip] = &limitEntry{count: 1, reset: now.Add(l.window)}
		return true
	}
	e.count++
	return e.count <= l.limit
}

var _ = subtle.ConstantTimeCompare
var _ = fmt.Sprint
var _ = errors.Is
