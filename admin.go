package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

func (a *application) adminListStorages(w http.ResponseWriter, r *http.Request) {
	if requireAdmin(w, r) == nil {
		return
	}
	storages, err := a.loadStorages(r)
	if err != nil {
		writeError(w, 500, "读取存储失败")
		return
	}
	writeJSON(w, 200, storages)
}

func (a *application) loadStorages(r *http.Request) ([]StorageRecord, error) {
	rows, err := a.db.Query(`SELECT s.id,s.name,s.type,s.config_json,s.is_default,s.enabled,COALESCE(SUM(i.size),0)
FROM storages s LEFT JOIN images i ON i.storage_id=s.id GROUP BY s.id ORDER BY s.is_default DESC,s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []StorageRecord{}
	for rows.Next() {
		var rec StorageRecord
		if err := rows.Scan(&rec.ID, &rec.Name, &rec.Type, &rec.ConfigJSON, &rec.IsDefault, &rec.Enabled, &rec.UsedBytes); err != nil {
			return nil, err
		}
		if store, err := a.storeFor(rec); err == nil {
			rec.TotalBytes, rec.UsedBytes, rec.FreeBytes = store.Capacity(r.Context(), rec.UsedBytes)
		}
		rec.ConfigJSON = maskStorageConfig(rec.Type, rec.ConfigJSON)
		items = append(items, rec)
	}
	return items, rows.Err()
}

func (a *application) adminCreateStorage(w http.ResponseWriter, r *http.Request) {
	if requireAdmin(w, r) == nil {
		return
	}
	var input StorageRecord
	if !decodeJSON(w, r, &input, 128<<10) {
		return
	}
	if err := validateStorageInput(input); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if _, err := a.storeFor(StorageRecord{Type: input.Type, ConfigJSON: input.ConfigJSON}); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		writeError(w, 500, "创建存储失败")
		return
	}
	defer tx.Rollback()
	if input.IsDefault {
		if _, err = tx.Exec("UPDATE storages SET is_default=0"); err != nil {
			writeError(w, 500, "创建存储失败")
			return
		}
	}
	result, err := tx.Exec("INSERT INTO storages(name,type,config_json,is_default,enabled) VALUES(?,?,?,?,?)", input.Name, input.Type, input.ConfigJSON, input.IsDefault, input.Enabled)
	if err != nil {
		writeError(w, 500, "创建存储失败")
		return
	}
	if err = tx.Commit(); err != nil {
		writeError(w, 500, "创建存储失败")
		return
	}
	id, _ := result.LastInsertId()
	writeJSON(w, 201, map[string]any{"id": id, "ok": true})
}

func (a *application) adminUpdateStorage(w http.ResponseWriter, r *http.Request) {
	if requireAdmin(w, r) == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "存储 ID 无效")
		return
	}
	var oldType, oldConfig string
	var oldDefault bool
	if err = a.db.QueryRow("SELECT type,config_json,is_default FROM storages WHERE id=?", id).Scan(&oldType, &oldConfig, &oldDefault); err != nil {
		writeError(w, 404, "存储不存在")
		return
	}
	var input StorageRecord
	if !decodeJSON(w, r, &input, 128<<10) {
		return
	}
	if input.Type != oldType {
		writeError(w, 400, "不能修改已有存储的类型")
		return
	}
	input.ConfigJSON = restoreMaskedConfig(input.Type, input.ConfigJSON, oldConfig)
	if err = validateStorageInput(input); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if _, err = a.storeFor(StorageRecord{Type: input.Type, ConfigJSON: input.ConfigJSON}); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if oldDefault && !input.IsDefault {
		writeError(w, 400, "请先将其他存储设为默认")
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		writeError(w, 500, "更新存储失败")
		return
	}
	defer tx.Rollback()
	if input.IsDefault {
		if _, err = tx.Exec("UPDATE storages SET is_default=0 WHERE id<>?", id); err != nil {
			writeError(w, 500, "更新存储失败")
			return
		}
	}
	_, err = tx.Exec("UPDATE storages SET name=?,config_json=?,is_default=?,enabled=? WHERE id=?", input.Name, input.ConfigJSON, input.IsDefault, input.Enabled, id)
	if err != nil || tx.Commit() != nil {
		writeError(w, 500, "更新存储失败")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *application) adminDeleteStorage(w http.ResponseWriter, r *http.Request) {
	if requireAdmin(w, r) == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "存储 ID 无效")
		return
	}
	var isDefault bool
	var count int
	if err = a.db.QueryRow("SELECT is_default FROM storages WHERE id=?", id).Scan(&isDefault); err != nil {
		writeError(w, 404, "存储不存在")
		return
	}
	if isDefault {
		writeError(w, 409, "默认存储不能删除")
		return
	}
	_ = a.db.QueryRow("SELECT COUNT(*) FROM images WHERE storage_id=?", id).Scan(&count)
	if count > 0 {
		writeError(w, 409, "该存储仍有图片，不能删除")
		return
	}
	_, err = a.db.Exec("DELETE FROM storages WHERE id=?", id)
	if err != nil {
		writeError(w, 500, "删除存储失败")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *application) adminStats(w http.ResponseWriter, r *http.Request) {
	if requireAdmin(w, r) == nil {
		return
	}
	var users, images int64
	var bytes int64
	_ = a.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&users)
	_ = a.db.QueryRow("SELECT COUNT(*),COALESCE(SUM(size),0) FROM images").Scan(&images, &bytes)
	storages, err := a.loadStorages(r)
	if err != nil {
		writeError(w, 500, "读取统计失败")
		return
	}
	writeJSON(w, 200, map[string]any{"users": users, "images": images, "bytes": bytes, "storages": storages})
}

func validateStorageInput(s StorageRecord) error {
	if s.Name == "" {
		return errors.New("存储名称不能为空")
	}
	if s.IsDefault && !s.Enabled {
		return errors.New("默认存储必须保持启用")
	}
	if s.Type != "local" && s.Type != "s3" && s.Type != "webdav" {
		return errors.New("存储类型无效")
	}
	if !json.Valid([]byte(s.ConfigJSON)) {
		return errors.New("存储配置必须是有效 JSON")
	}
	if s.Type == "local" && s.ConfigJSON != "{}" {
		var v map[string]any
		if json.Unmarshal([]byte(s.ConfigJSON), &v) != nil || len(v) > 0 {
			return errors.New("本机存储无需额外配置")
		}
	}
	return nil
}

func maskStorageConfig(kind, raw string) string {
	var v map[string]any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return "{}"
	}
	if kind == "s3" {
		if _, ok := v["secret_key"]; ok {
			v["secret_key"] = "••••••••"
		}
	}
	if kind == "webdav" {
		if _, ok := v["password"]; ok {
			v["password"] = "••••••••"
		}
	}
	b, _ := json.Marshal(v)
	return string(b)
}
func restoreMaskedConfig(kind, raw, oldRaw string) string {
	var next, old map[string]any
	if json.Unmarshal([]byte(raw), &next) != nil || json.Unmarshal([]byte(oldRaw), &old) != nil {
		return raw
	}
	key := ""
	if kind == "s3" {
		key = "secret_key"
	} else if kind == "webdav" {
		key = "password"
	}
	if key != "" && next[key] == "••••••••" {
		next[key] = old[key]
	}
	b, _ := json.Marshal(next)
	return string(b)
}

var _ sql.Result
