package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "golang.org/x/image/webp"
)

func (a *application) uploadFiles(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	settings, err := a.loadSettings()
	if err != nil {
		writeError(w, 500, "无法读取上传设置")
		return
	}
	limit, types, expiry, ok := uploadPolicy(w, user, settings)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, min(limit*20+1<<20, 512<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, 400, "上传内容过大或格式错误")
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		if f := r.MultipartForm.File["file"]; len(f) > 0 {
			files = f
		}
	}
	if len(files) == 0 {
		writeError(w, 400, "请选择图片")
		return
	}
	if len(files) > 20 {
		writeError(w, 400, "每次最多上传 20 张图片")
		return
	}
	private := r.FormValue("private") == "true"
	results := make([]ImageRecord, 0, len(files))
	failures := []map[string]string{}
	for _, header := range files {
		if header.Size > limit {
			failures = append(failures, map[string]string{"name": header.Filename, "error": "文件超过大小限制"})
			continue
		}
		file, err := header.Open()
		if err != nil {
			failures = append(failures, map[string]string{"name": header.Filename, "error": "读取文件失败"})
			continue
		}
		record, err := a.ingest(r.Context(), r, user, file, header.Filename, limit, types, expiry, private)
		file.Close()
		if err != nil {
			failures = append(failures, map[string]string{"name": header.Filename, "error": safeUploadError(err)})
			continue
		}
		results = append(results, record)
	}
	status := http.StatusCreated
	if len(results) == 0 {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{"images": results, "failures": failures})
}

func (a *application) uploadURL(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	settings, err := a.loadSettings()
	if err != nil {
		writeError(w, 500, "无法读取上传设置")
		return
	}
	limit, types, expiry, ok := uploadPolicy(w, user, settings)
	if !ok {
		return
	}
	if !a.limiter.Allow(clientIP(r)) {
		writeError(w, 429, "请求过于频繁")
		return
	}
	var input struct {
		URL     string `json:"url"`
		Private bool   `json:"private"`
	}
	if !decodeJSON(w, r, &input, 32<<10) {
		return
	}
	u, err := url.Parse(input.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		writeError(w, 400, "图片 URL 无效")
		return
	}
	client := &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("重定向次数过多")
		}
		return validateRemoteHost(req.Context(), req.URL.Hostname())
	}}
	if err := validateRemoteHost(r.Context(), u.Hostname()); err != nil {
		writeError(w, 400, "不允许访问该地址")
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	req.Header.Set("User-Agent", "Congyu-Image/1.0")
	resp, err := client.Do(req)
	if err != nil {
		writeError(w, 400, "无法下载远程图片")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeError(w, 400, "远程服务器返回了错误状态")
		return
	}
	if resp.ContentLength > limit {
		writeError(w, 400, "远程图片超过大小限制")
		return
	}
	name := filepath.Base(u.Path)
	if name == "." || name == "/" || name == "" {
		name = "remote-image"
	}
	record, err := a.ingest(r.Context(), r, user, io.LimitReader(resp.Body, limit+1), name, limit, types, expiry, input.Private)
	if err != nil {
		writeError(w, 400, safeUploadError(err))
		return
	}
	writeJSON(w, 201, map[string]any{"images": []ImageRecord{record}, "failures": []any{}})
}

func uploadPolicy(w http.ResponseWriter, user *User, s Settings) (int64, string, *time.Time, bool) {
	if user != nil {
		return user.MaxFileSize, user.AllowedTypes, nil, true
	}
	if !s.GuestUploadEnabled {
		writeError(w, 401, "请先登录后上传")
		return 0, "", nil, false
	}
	exp := time.Now().UTC().Add(time.Duration(s.GuestExpiryHours) * time.Hour)
	return s.GuestMaxFileSize, s.GuestAllowedTypes, &exp, true
}

func (a *application) ingest(ctx context.Context, r *http.Request, user *User, src io.Reader, name string, limit int64, allowed string, expiry *time.Time, private bool) (ImageRecord, error) {
	in, err := os.CreateTemp("", "congyu-input-*")
	if err != nil {
		return ImageRecord{}, err
	}
	inName := in.Name()
	defer os.Remove(inName)
	written, err := io.Copy(in, io.LimitReader(src, limit+1))
	if err != nil {
		in.Close()
		return ImageRecord{}, err
	}
	if written > limit {
		in.Close()
		return ImageRecord{}, errors.New("文件超过大小限制")
	}
	if _, err = in.Seek(0, 0); err != nil {
		in.Close()
		return ImageRecord{}, err
	}
	reader := bufio.NewReader(in)
	head, err := reader.Peek(min(512, int(written)))
	if err != nil && err != io.EOF {
		in.Close()
		return ImageRecord{}, err
	}
	mime := http.DetectContentType(head)
	if !typeAllowed(mime, allowed) {
		in.Close()
		return ImageRecord{}, fmt.Errorf("不支持的图片类型：%s", mime)
	}
	if _, err = in.Seek(0, 0); err != nil {
		in.Close()
		return ImageRecord{}, err
	}
	cfg, format, err := image.DecodeConfig(in)
	if err != nil || cfg.Width < 1 || cfg.Height < 1 {
		in.Close()
		return ImageRecord{}, errors.New("无法解析图片内容")
	}
	if int64(cfg.Width)*int64(cfg.Height) > 100_000_000 {
		in.Close()
		return ImageRecord{}, errors.New("图片像素尺寸过大")
	}
	inputPath := inName
	if format == "gif" || format == "webp" {
		if _, err = in.Seek(0, 0); err != nil {
			in.Close()
			return ImageRecord{}, err
		}
		decoded, _, err := image.Decode(in)
		if err != nil {
			in.Close()
			return ImageRecord{}, errors.New("图片解码失败")
		}
		pngFile, err := os.CreateTemp("", "congyu-decoded-*.png")
		if err != nil {
			in.Close()
			return ImageRecord{}, err
		}
		pngName := pngFile.Name()
		defer os.Remove(pngName)
		if err = png.Encode(pngFile, decoded); err != nil {
			pngFile.Close()
			in.Close()
			return ImageRecord{}, err
		}
		pngFile.Close()
		inputPath = pngName
	}
	in.Close()
	settings, err := a.loadSettings()
	if err != nil {
		return ImageRecord{}, err
	}
	out, err := os.CreateTemp("", "congyu-output-*.webp")
	if err != nil {
		return ImageRecord{}, err
	}
	outName := out.Name()
	out.Close()
	defer os.Remove(outName)
	cmd := exec.CommandContext(ctx, "cwebp", "-quiet", "-mt", "-q", strconv.Itoa(settings.WebPQuality), inputPath, "-o", outName)
	if output, err := cmd.CombinedOutput(); err != nil {
		return ImageRecord{}, fmt.Errorf("图片压缩失败：%s", strings.TrimSpace(string(output)))
	}
	stat, err := os.Stat(outName)
	if err != nil || stat.Size() == 0 {
		return ImageRecord{}, errors.New("图片压缩失败")
	}
	storageRec, store, err := a.defaultStorage(ctx)
	if err != nil {
		return ImageRecord{}, err
	}
	key := fmt.Sprintf("%s/%s.webp", time.Now().UTC().Format("2006/01/02"), randomToken(18))
	f, err := os.Open(outName)
	if err != nil {
		return ImageRecord{}, err
	}
	err = store.Put(ctx, key, f, stat.Size(), "image/webp")
	f.Close()
	if err != nil {
		return ImageRecord{}, fmt.Errorf("写入存储失败：%w", err)
	}
	rollbackObject := true
	defer func() {
		if rollbackObject {
			_ = store.Delete(context.Background(), key)
		}
	}()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return ImageRecord{}, err
	}
	defer tx.Rollback()
	var uid any
	if user != nil {
		uid = user.ID
		result, err := tx.Exec(`UPDATE users SET used_bytes=used_bytes+? WHERE id=? AND (quota_bytes=0 OR used_bytes+?<=quota_bytes)`, stat.Size(), user.ID, stat.Size())
		if err != nil {
			return ImageRecord{}, err
		}
		affected, _ := result.RowsAffected()
		if affected == 0 {
			return ImageRecord{}, errors.New("用户存储空间不足")
		}
	}
	share := randomToken(24)
	var expiryValue any
	if expiry != nil {
		expiryValue = expiry.Format(time.RFC3339)
	}
	result, err := tx.Exec(`INSERT INTO images(user_id,storage_id,original_name,mime_type,size,width,height,is_private,share_token,storage_key,expires_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, uid, storageRec.ID, sanitizeFilename(name), "image/webp", stat.Size(), cfg.Width, cfg.Height, private, share, key, expiryValue)
	if err != nil {
		return ImageRecord{}, err
	}
	id, _ := result.LastInsertId()
	if err = tx.Commit(); err != nil {
		return ImageRecord{}, err
	}
	rollbackObject = false
	record := ImageRecord{ID: id, StorageID: storageRec.ID, OriginalName: sanitizeFilename(name), MIMEType: "image/webp", Size: stat.Size(), Width: cfg.Width, Height: cfg.Height, Private: private, ShareToken: share, StorageKey: key, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if user != nil {
		record.UserID = &user.ID
		record.Username = user.Username
	}
	if expiry != nil {
		s := expiry.Format(time.RFC3339)
		record.ExpiresAt = &s
	}
	a.addLinks(r, &record, settings)
	return record, nil
}

func typeAllowed(mime, allowed string) bool {
	if mime == "image/jpg" {
		mime = "image/jpeg"
	}
	for _, v := range strings.Split(allowed, ",") {
		if strings.TrimSpace(v) == mime {
			return true
		}
	}
	return false
}
func sanitizeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 32 {
			return -1
		}
		return r
	}, name)
	if len([]rune(name)) > 180 {
		name = string([]rune(name)[:180])
	}
	if name == "" || name == "." {
		return "image"
	}
	return name
}
func safeUploadError(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "cwebp") || strings.Contains(msg, "exec:") {
		return "图片压缩服务暂不可用"
	}
	if len([]rune(msg)) > 160 {
		return "上传失败"
	}
	return msg
}

func validateRemoteHost(ctx context.Context, host string) error {
	if strings.EqualFold(host, "localhost") {
		return errors.New("private host")
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return err
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return errors.New("private address")
		}
	}
	return nil
}

func (a *application) defaultStorage(ctx context.Context) (StorageRecord, objectStore, error) {
	var rec StorageRecord
	err := a.db.QueryRow("SELECT id,name,type,config_json,is_default,enabled FROM storages WHERE is_default=1 AND enabled=1").Scan(&rec.ID, &rec.Name, &rec.Type, &rec.ConfigJSON, &rec.IsDefault, &rec.Enabled)
	if err != nil {
		return rec, nil, errors.New("没有可用的默认存储")
	}
	store, err := a.storeFor(rec)
	return rec, store, err
}
func (a *application) storeFor(rec StorageRecord) (objectStore, error) {
	switch rec.Type {
	case "local":
		return &localStore{root: filepath.Join(a.cfg.DataDir, "images")}, nil
	case "s3":
		return newS3Store(rec.ConfigJSON)
	case "webdav":
		return newWebDAVStore(rec.ConfigJSON)
	default:
		return nil, errors.New("未知存储类型")
	}
}

func (a *application) listImages(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	user := currentUser(r)
	settings, err := a.loadSettings()
	if err != nil {
		writeError(w, 500, "读取设置失败")
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	limit := 30
	offset := (page - 1) * limit
	query := `SELECT i.id,i.user_id,COALESCE(u.username,''),i.storage_id,i.original_name,i.mime_type,i.size,i.width,i.height,i.is_private,i.share_token,i.storage_key,i.expires_at,i.created_at
FROM images i LEFT JOIN users u ON u.id=i.user_id `
	args := []any{}
	if scope == "mine" {
		if user == nil {
			writeError(w, 401, "请先登录")
			return
		}
		query += "WHERE i.user_id=? "
		args = append(args, user.ID)
	} else {
		if !settings.GalleryEnabled {
			writeJSON(w, 200, map[string]any{"images": []any{}, "page": page, "has_more": false})
			return
		}
		query += "WHERE i.is_private=0 AND (i.expires_at IS NULL OR i.expires_at>?) "
		args = append(args, time.Now().UTC().Format(time.RFC3339))
	}
	query += "ORDER BY i.created_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit+1, offset)
	rows, err := a.db.Query(query, args...)
	if err != nil {
		writeError(w, 500, "读取图片失败")
		return
	}
	defer rows.Close()
	items := []ImageRecord{}
	for rows.Next() {
		var item ImageRecord
		var expires sql.NullString
		if rows.Scan(&item.ID, &item.UserID, &item.Username, &item.StorageID, &item.OriginalName, &item.MIMEType, &item.Size, &item.Width, &item.Height, &item.Private, &item.ShareToken, &item.StorageKey, &expires, &item.CreatedAt) == nil {
			if expires.Valid {
				item.ExpiresAt = &expires.String
			}
			a.addLinks(r, &item, settings)
			items = append(items, item)
		}
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	writeJSON(w, 200, map[string]any{"images": items, "page": page, "has_more": hasMore})
}

func (a *application) addLinks(r *http.Request, item *ImageRecord, s Settings) {
	base := strings.TrimRight(s.SiteDomain, "/")
	if base == "" {
		base = a.cfg.BaseURL
	}
	if base == "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	suffix := ""
	if item.Private {
		suffix = "/" + item.ShareToken
	}
	item.URL = fmt.Sprintf("%s/media/%d%s", base, item.ID, suffix)
	item.Markdown = fmt.Sprintf("![%s](%s)", strings.TrimSuffix(item.OriginalName, filepath.Ext(item.OriginalName)), item.URL)
}

func (a *application) serveImage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var item ImageRecord
	var expires sql.NullString
	err = a.db.QueryRow(`SELECT id,user_id,storage_id,original_name,mime_type,size,width,height,is_private,share_token,storage_key,expires_at,created_at FROM images WHERE id=?`, id).Scan(&item.ID, &item.UserID, &item.StorageID, &item.OriginalName, &item.MIMEType, &item.Size, &item.Width, &item.Height, &item.Private, &item.ShareToken, &item.StorageKey, &expires, &item.CreatedAt)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if expires.Valid {
		expiry, _ := time.Parse(time.RFC3339, expires.String)
		if time.Now().UTC().After(expiry) {
			http.Error(w, "图片已过期", http.StatusGone)
			return
		}
	}
	provided := r.PathValue("token")
	if item.Private && (len(provided) != len(item.ShareToken) || subtleCompare(provided, item.ShareToken) == false) {
		http.NotFound(w, r)
		return
	}
	var rec StorageRecord
	err = a.db.QueryRow("SELECT id,name,type,config_json,is_default,enabled FROM storages WHERE id=?", item.StorageID).Scan(&rec.ID, &rec.Name, &rec.Type, &rec.ConfigJSON, &rec.IsDefault, &rec.Enabled)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	store, err := a.storeFor(rec)
	if err != nil {
		http.Error(w, "存储不可用", 503)
		return
	}
	body, err := store.Open(r.Context(), item.StorageKey)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("Content-Length", strconv.FormatInt(item.Size, 10))
	w.Header().Set("Content-Disposition", "inline; filename=\"image.webp\"")
	if item.Private {
		w.Header().Set("Cache-Control", "private, max-age=3600")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	_, _ = io.Copy(w, body)
}
func subtleCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func (a *application) deleteImage(w http.ResponseWriter, r *http.Request) {
	user := requireUser(w, r)
	if user == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "图片 ID 无效")
		return
	}
	var item ImageRecord
	err = a.db.QueryRow("SELECT id,user_id,storage_id,size,storage_key FROM images WHERE id=?", id).Scan(&item.ID, &item.UserID, &item.StorageID, &item.Size, &item.StorageKey)
	if err != nil {
		writeError(w, 404, "图片不存在")
		return
	}
	if !user.IsAdmin && (item.UserID == nil || *item.UserID != user.ID) {
		writeError(w, 403, "无权删除该图片")
		return
	}
	var rec StorageRecord
	err = a.db.QueryRow("SELECT id,name,type,config_json,is_default,enabled FROM storages WHERE id=?", item.StorageID).Scan(&rec.ID, &rec.Name, &rec.Type, &rec.ConfigJSON, &rec.IsDefault, &rec.Enabled)
	if err != nil {
		writeError(w, 500, "存储配置缺失")
		return
	}
	store, err := a.storeFor(rec)
	if err != nil {
		writeError(w, 503, "存储不可用")
		return
	}
	if err = store.Delete(r.Context(), item.StorageKey); err != nil {
		writeError(w, 502, "删除存储对象失败")
		return
	}
	tx, _ := a.db.Begin()
	defer tx.Rollback()
	_, err = tx.Exec("DELETE FROM images WHERE id=?", id)
	if err == nil && item.UserID != nil {
		_, err = tx.Exec("UPDATE users SET used_bytes=max(0,used_bytes-?) WHERE id=?", item.Size, *item.UserID)
	}
	if err != nil || tx.Commit() != nil {
		writeError(w, 500, "更新数据库失败")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *application) cleanupLoop(ctx context.Context) {
	a.cleanupExpired(ctx)
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.cleanupExpired(ctx)
		}
	}
}

func (a *application) cleanupExpired(ctx context.Context) {
	_, _ = a.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at<=?", time.Now().UTC().Format(time.RFC3339))
	rows, err := a.db.QueryContext(ctx, `SELECT id,user_id,storage_id,size,storage_key FROM images
WHERE expires_at IS NOT NULL AND expires_at<=? LIMIT 100`, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return
	}
	items := []ImageRecord{}
	for rows.Next() {
		var item ImageRecord
		if rows.Scan(&item.ID, &item.UserID, &item.StorageID, &item.Size, &item.StorageKey) == nil {
			items = append(items, item)
		}
	}
	rows.Close()
	for _, item := range items {
		var rec StorageRecord
		if a.db.QueryRowContext(ctx, "SELECT id,name,type,config_json,is_default,enabled FROM storages WHERE id=?", item.StorageID).
			Scan(&rec.ID, &rec.Name, &rec.Type, &rec.ConfigJSON, &rec.IsDefault, &rec.Enabled) != nil {
			continue
		}
		store, err := a.storeFor(rec)
		if err != nil || store.Delete(ctx, item.StorageKey) != nil {
			continue
		}
		tx, err := a.db.BeginTx(ctx, nil)
		if err != nil {
			continue
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM images WHERE id=?", item.ID)
		if err == nil && item.UserID != nil {
			_, err = tx.ExecContext(ctx, "UPDATE users SET used_bytes=max(0,used_bytes-?) WHERE id=?", item.Size, *item.UserID)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			tx.Rollback()
		}
	}
}

var _ multipart.File
var _ = json.Valid
