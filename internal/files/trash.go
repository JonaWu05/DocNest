// 軟刪除 / 資源回收筒：刪除改為移到 DOC_ROOT/.trash（以 . 開頭，buildTree 會自動排除，
// 不會出現在檔案樹），保留原始路徑等中繼資料供還原。每個回收項目存成一個以時間戳記為名的
// 子目錄：.trash/<id>/<原始檔名> 為內容，.trash/<id>/meta.json 為中繼資料。
package files

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"markdownEditor/internal/authz"
	"markdownEditor/internal/httpx"
)

// trashCleanInterval 為背景清除的執行頻率。
const trashCleanInterval = 6 * time.Hour

const trashDirName = ".trash"

// trashIDRe 限制回收項目 id 僅由數字組成（時間戳記），避免 query 帶入路徑穿越。
var trashIDRe = regexp.MustCompile(`^[0-9]+$`)

// trashMeta 為回收項目寫入 meta.json 的中繼資料。
type trashMeta struct {
	Original  string `json:"original"`  // 原始 DOC_ROOT 相對路徑
	Name      string `json:"name"`      // 原始 basename
	IsDir     bool   `json:"isDir"`     //
	DeletedAt string `json:"deletedAt"` // RFC3339
	DeletedBy string `json:"deletedBy"` // 刪除者身分鍵
}

// TrashItem 為回傳給前端的回收項目（不含 deletedBy）。
type TrashItem struct {
	ID        string `json:"id"`
	Original  string `json:"original"`
	Name      string `json:"name"`
	IsDir     bool   `json:"isDir"`
	DeletedAt string `json:"deletedAt"`
}

func (f *Files) trashDir() string { return filepath.Join(f.store.Root, trashDirName) }

// underTrash 判斷某 DOC_ROOT 相對路徑是否位於 .trash 內。
func underTrash(rel string) bool {
	return rel == trashDirName || strings.HasPrefix(rel, trashDirName+"/")
}

// moveToTrash 把目標移入 .trash/<id>/，並寫入 meta.json。
func (f *Files) moveToTrash(absPath, originalRel, subject string, isDir bool) error {
	id := strconv.FormatInt(time.Now().UnixNano(), 10)
	entryDir := filepath.Join(f.trashDir(), id)
	if err := os.MkdirAll(entryDir, 0o755); err != nil {
		return err
	}
	name := filepath.Base(absPath)
	if err := os.Rename(absPath, filepath.Join(entryDir, name)); err != nil {
		return err
	}
	meta := trashMeta{
		Original:  originalRel,
		Name:      name,
		IsDir:     isDir,
		DeletedAt: time.Now().Format(time.RFC3339),
		DeletedBy: subject,
	}
	data, _ := json.MarshalIndent(meta, "", "  ")
	return os.WriteFile(filepath.Join(entryDir, "meta.json"), data, 0o644)
}

func (f *Files) readTrashMeta(id string) (trashMeta, error) {
	var m trashMeta
	data, err := os.ReadFile(filepath.Join(f.trashDir(), id, "meta.json"))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(data, &m)
	return m, err
}

// ListTrash 處理 GET /api/trash：列出使用者對其「原始路徑」有寫入權的回收項目（與權限一致，避免名稱外洩）。
func (f *Files) ListTrash(c *gin.Context) {
	subject := authz.SubjectOf(c)
	entries, err := os.ReadDir(f.trashDir())
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusOK, gin.H{"items": []TrashItem{}})
			return
		}
		httpx.ServerError(c, "讀取資源回收筒失敗", err)
		return
	}

	items := []TrashItem{}
	for _, e := range entries {
		if !e.IsDir() || !trashIDRe.MatchString(e.Name()) {
			continue
		}
		m, err := f.readTrashMeta(e.Name())
		if err != nil {
			continue
		}
		if !f.az.Can(subject, m.Original, authz.AccessWrite) {
			continue
		}
		items = append(items, TrashItem{
			ID: e.Name(), Original: m.Original, Name: m.Name, IsDir: m.IsDir, DeletedAt: m.DeletedAt,
		})
	}
	// id 為 unixnano，反向排序即新到舊
	sort.Slice(items, func(i, j int) bool { return items[i].ID > items[j].ID })
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// RestoreTrash 處理 POST /api/trash/restore?id=xxx：把回收項目移回原始路徑。
func (f *Files) RestoreTrash(c *gin.Context) {
	id := c.Query("id")
	if !trashIDRe.MatchString(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "無效的 id"})
		return
	}
	m, err := f.readTrashMeta(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "找不到回收項目"})
		return
	}
	if !f.az.RequireAccess(c, m.Original, authz.AccessWrite) {
		return
	}
	targetAbs, err := f.store.SafeResolve(m.Original)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的還原路徑"})
		return
	}
	if _, err := os.Stat(targetAbs); err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "原位置已存在同名項目，無法還原"})
		return
	}
	if err := os.MkdirAll(filepath.Dir(targetAbs), 0o755); err != nil {
		httpx.ServerError(c, "建立目錄失敗", err)
		return
	}
	if err := os.Rename(filepath.Join(f.trashDir(), id, m.Name), targetAbs); err != nil {
		httpx.ServerError(c, "還原失敗", err)
		return
	}
	os.RemoveAll(filepath.Join(f.trashDir(), id)) // 清掉殘留的 meta 與空目錄
	f.invalidateFor(m.Original)
	c.JSON(http.StatusOK, gin.H{"message": "已還原", "path": m.Original})
}

// PurgeTrash 處理 DELETE /api/trash?id=xxx：永久刪除單一回收項目。
func (f *Files) PurgeTrash(c *gin.Context) {
	id := c.Query("id")
	if !trashIDRe.MatchString(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "無效的 id"})
		return
	}
	m, err := f.readTrashMeta(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "找不到回收項目"})
		return
	}
	if !f.az.RequireAccess(c, m.Original, authz.AccessWrite) {
		return
	}
	if err := os.RemoveAll(filepath.Join(f.trashDir(), id)); err != nil {
		httpx.ServerError(c, "永久刪除失敗", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "已永久刪除"})
}

// StartTrashCleaner 啟動背景排程，定期永久刪除過期的回收筒項目。
// retentionDays <= 0 代表停用（永久保留）。啟動時先清一次，之後每 trashCleanInterval 清一次。
func (f *Files) StartTrashCleaner(retentionDays int) {
	if retentionDays <= 0 {
		slog.Info("資源回收筒自動清除已停用（TRASH_RETENTION_DAYS<=0）")
		return
	}
	maxAge := time.Duration(retentionDays) * 24 * time.Hour
	slog.Info("資源回收筒自動清除已啟用", "retentionDays", retentionDays)
	go func() {
		for {
			if n := f.purgeExpiredTrash(maxAge); n > 0 {
				slog.Info("已自動清除過期的回收筒項目", "count", n, "olderThanDays", retentionDays)
			}
			time.Sleep(trashCleanInterval)
		}
	}()
}

// purgeExpiredTrash 永久刪除 .trash 中刪除時間早於 maxAge 的項目，回傳清除筆數。
func (f *Files) purgeExpiredTrash(maxAge time.Duration) int {
	entries, err := os.ReadDir(f.trashDir())
	if err != nil {
		return 0 // 尚無 .trash 或讀取失敗：無事可清
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || !trashIDRe.MatchString(e.Name()) {
			continue
		}
		if !trashEntryExpired(f, e, cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(f.trashDir(), e.Name())); err == nil {
			removed++
		}
	}
	return removed
}

// trashEntryExpired 依 meta.json 的 deletedAt 判斷是否過期；解析失敗則退回用目錄修改時間。
func trashEntryExpired(f *Files, e os.DirEntry, cutoff time.Time) bool {
	if m, err := f.readTrashMeta(e.Name()); err == nil {
		if t, err := time.Parse(time.RFC3339, m.DeletedAt); err == nil {
			return t.Before(cutoff)
		}
	}
	if info, err := e.Info(); err == nil {
		return info.ModTime().Before(cutoff)
	}
	return false
}
