// Package files 提供文件的 CRUD 與原始檔案（圖片/附件）服務，並依權限過濾檔案樹。
package files

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"markdownEditor/internal/authz"
	"markdownEditor/internal/hub"
	"markdownEditor/internal/store"
)

// tsPrefix 比對上傳檔名的「時間戳記_」前綴（對應前端 assetDisplayName 的清理規則）
var tsPrefix = regexp.MustCompile(`^\d+_`)

// maxWriteSize 為單次寫入檔案的 request body 上限（10 MB）。
const maxWriteSize = 10 << 20

// Files 綁定檔案儲存、授權判斷與 WebSocket Hub（儲存後廣播通知）。
type Files struct {
	store *store.Store
	az    *authz.Authz
	hub   *hub.Hub
}

// New 建立 Files handler 集合。
func New(st *store.Store, az *authz.Authz, h *hub.Hub) *Files {
	return &Files{store: st, az: az, hub: h}
}

// fileVersion 由內容算出短雜湊，作為樂觀鎖的版本識別。
func fileVersion(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// filterTree 依 subject 的讀取權過濾檔案樹：
//   - 檔案：可讀才保留
//   - 資料夾：自身可讀、或含有可讀後代時保留（讓使用者能逐層導覽到可讀檔案）
//
// 同時標記每個保留節點的 Writable，供前端隱藏無權限的編輯／刪除操作。
func (f *Files) filterTree(nodes []*store.FileNode, subject string) []*store.FileNode {
	out := []*store.FileNode{}
	for _, n := range nodes {
		if n.IsDir {
			n.Children = f.filterTree(n.Children, subject)
			if len(n.Children) > 0 || f.az.Can(subject, n.Path, authz.AccessRead) {
				n.Writable = f.az.Can(subject, n.Path, authz.AccessWrite)
				out = append(out, n)
			}
		} else if f.az.Can(subject, n.Path, authz.AccessRead) {
			n.Writable = f.az.Can(subject, n.Path, authz.AccessWrite)
			out = append(out, n)
		}
	}
	return out
}

// ListFiles 處理 GET /api/files：建立檔案樹，依使用者讀取權過濾後回傳。
func (f *Files) ListFiles(c *gin.Context) {
	tree, err := f.store.BuildTree()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "無法讀取文件目錄：" + err.Error()})
		return
	}
	children := f.filterTree(tree.Children, authz.SubjectOf(c))
	c.JSON(http.StatusOK, gin.H{"files": children})
}

// ReadFile 處理 GET /api/file?path=xxx：讀取檔案內容並以純文字回傳。
func (f *Files) ReadFile(c *gin.Context) {
	pathParam := c.Query("path")
	if pathParam == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 path 參數"})
		return
	}

	absPath, err := f.store.SafeResolve(pathParam)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的檔案路徑"})
		return
	}

	if !store.IsAllowedFile(absPath) {
		c.JSON(http.StatusForbidden, gin.H{"error": "不支援的檔案類型"})
		return
	}

	if !f.az.RequireAccess(c, f.store.RelOf(absPath), authz.AccessRead) {
		return
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "檔案不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "讀取檔案失敗：" + err.Error()})
		return
	}

	// 帶上版本識別，供前端做樂觀鎖
	c.Header("X-File-Version", fileVersion(content))
	c.Data(http.StatusOK, "text/plain; charset=utf-8", content)
}

// WriteFile 處理 POST /api/file?path=xxx：將 request body 寫入指定檔案。
func (f *Files) WriteFile(c *gin.Context) {
	pathParam := c.Query("path")
	if pathParam == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 path 參數"})
		return
	}

	absPath, err := f.store.SafeResolve(pathParam)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的檔案路徑"})
		return
	}

	if !store.IsAllowedFile(absPath) {
		c.JSON(http.StatusForbidden, gin.H{"error": "不支援的檔案類型"})
		return
	}

	if !f.az.RequireAccess(c, f.store.RelOf(absPath), authz.AccessWrite) {
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxWriteSize))
	if err != nil {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "內容過大或讀取失敗（單檔上限 10 MB）"})
		return
	}

	// 同一檔的「讀版本 → 比對 → 寫入」需序列化，否則兩個並發寫入的樂觀鎖檢查可能都通過而互相覆蓋。
	mu := f.store.Lock(absPath)
	defer mu.Unlock()

	// 樂觀鎖：用戶端帶了基準版本且與磁碟現況不符 → 回 409，交由前端讓使用者決定。
	// force=1 表示使用者明確選擇覆蓋，略過檢查。
	if base := c.GetHeader("X-File-Version"); base != "" && c.Query("force") != "1" {
		if existing, rerr := os.ReadFile(absPath); rerr == nil {
			if cur := fileVersion(existing); cur != base {
				c.JSON(http.StatusConflict, gin.H{
					"error":           "檔案已被其他人更新，請先載入最新版本",
					"current_version": cur,
				})
				return
			}
		}
	}

	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "建立目錄失敗：" + err.Error()})
		return
	}

	// 原子寫入：先寫暫存檔再 rename，避免並發讀者讀到半寫入內容。
	if err := store.AtomicWrite(absPath, body, 0o644); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "寫入檔案失敗：" + err.Error()})
		return
	}

	// 廣播 file_updated 通知（僅 path + savedBy，不含內容）。savedBy 取自 JWT，不採信前端。
	// 路徑用 RelOf 正規化，與 hub 廣播時的讀取權過濾、前端的 currentFile 比對保持一致。
	f.hub.BroadcastFileUpdated(f.store.RelOf(absPath), c.GetString("username"))

	c.Header("X-File-Version", fileVersion(body))
	c.JSON(http.StatusOK, gin.H{"message": "儲存成功"})
}

// DeleteFile 處理 DELETE /api/file?path=xxx：刪除檔案或資料夾。
func (f *Files) DeleteFile(c *gin.Context) {
	pathParam := c.Query("path")
	if pathParam == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 path 參數"})
		return
	}

	absPath, err := f.store.SafeResolve(pathParam)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的檔案路徑"})
		return
	}

	if absPath == f.store.Root {
		c.JSON(http.StatusForbidden, gin.H{"error": "不可刪除文件根目錄"})
		return
	}

	if !f.az.RequireAccess(c, f.store.RelOf(absPath), authz.AccessWrite) {
		return
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "檔案或資料夾不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "讀取狀態失敗：" + err.Error()})
		return
	}

	if info.IsDir() {
		if err := os.RemoveAll(absPath); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "刪除資料夾失敗：" + err.Error()})
			return
		}
	} else {
		// 允許刪除文件（.md/.txt）以及已上傳的圖片/附件
		ext := strings.ToLower(filepath.Ext(absPath))
		if !store.IsAllowedFile(absPath) && !store.IsAllowedUpload(ext) {
			c.JSON(http.StatusForbidden, gin.H{"error": "不支援的檔案類型"})
			return
		}
		if err := os.Remove(absPath); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "刪除檔案失敗：" + err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": "刪除成功"})
}

// Create 處理 POST /api/create?path=xxx&type=file|dir：新增檔案或資料夾。
func (f *Files) Create(c *gin.Context) {
	pathParam := c.Query("path")
	itemType := c.Query("type")
	if pathParam == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 path 參數"})
		return
	}
	if itemType != "file" && itemType != "dir" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "type 參數必須為 file 或 dir"})
		return
	}

	absPath, err := f.store.SafeResolve(pathParam)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的檔案路徑"})
		return
	}

	if !f.az.RequireAccess(c, f.store.RelOf(absPath), authz.AccessWrite) {
		return
	}

	if _, err := os.Stat(absPath); err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "同名的檔案或資料夾已存在"})
		return
	}

	if itemType == "dir" {
		if err := os.MkdirAll(absPath, 0o755); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "建立資料夾失敗：" + err.Error()})
			return
		}
	} else {
		if !store.IsAllowedFile(absPath) {
			c.JSON(http.StatusForbidden, gin.H{"error": "僅能建立 .md 或 .txt 檔案"})
			return
		}
		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "建立目錄失敗：" + err.Error()})
			return
		}
		if err := os.WriteFile(absPath, []byte{}, 0o644); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "建立檔案失敗：" + err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": "建立成功"})
}

// Rename 處理 POST /api/rename?path=old&newPath=new：重新命名或移動檔案/資料夾。
func (f *Files) Rename(c *gin.Context) {
	oldParam := c.Query("path")
	newParam := c.Query("newPath")
	if oldParam == "" || newParam == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 path 或 newPath 參數"})
		return
	}

	oldAbs, err := f.store.SafeResolve(oldParam)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的來源路徑"})
		return
	}
	newAbs, err := f.store.SafeResolve(newParam)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的目標路徑"})
		return
	}

	// 改名/移動同時影響來源與目的地，兩端都需具備寫入權
	if !f.az.RequireAccess(c, f.store.RelOf(oldAbs), authz.AccessWrite) || !f.az.RequireAccess(c, f.store.RelOf(newAbs), authz.AccessWrite) {
		return
	}

	if oldAbs == f.store.Root {
		c.JSON(http.StatusForbidden, gin.H{"error": "不可移動文件根目錄"})
		return
	}

	info, err := os.Stat(oldAbs)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "來源不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "讀取狀態失敗：" + err.Error()})
		return
	}

	if !info.IsDir() && !store.IsAllowedFile(newAbs) {
		c.JSON(http.StatusForbidden, gin.H{"error": "目標檔名必須為 .md 或 .txt"})
		return
	}

	if _, err := os.Stat(newAbs); err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "目標已存在"})
		return
	}

	if err := os.MkdirAll(filepath.Dir(newAbs), 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "建立目錄失敗：" + err.Error()})
		return
	}

	if err := os.Rename(oldAbs, newAbs); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "重新命名失敗：" + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "重新命名成功"})
}

// Raw 處理 GET /api/raw?path=xxx：直接提供原始檔案內容（供圖片/附件顯示與下載）。
func (f *Files) Raw(c *gin.Context) {
	pathParam := c.Query("path")
	if pathParam == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 path 參數"})
		return
	}

	absPath, err := f.store.SafeResolve(pathParam)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的檔案路徑"})
		return
	}

	// 原始檔案服務同樣需讀取權，避免繞過樹過濾直接讀取
	if !f.az.RequireAccess(c, f.store.RelOf(absPath), authz.AccessRead) {
		return
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "檔案不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "讀取狀態失敗：" + err.Error()})
		return
	}
	if info.IsDir() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不可直接存取資料夾"})
		return
	}

	// 安全標頭：避免瀏覽器把非預期檔案以同源 inline 方式渲染（HTML/SVG 等）而造成 XSS。
	ext := strings.ToLower(filepath.Ext(absPath))
	c.Header("X-Content-Type-Options", "nosniff")
	if store.IsImageExt(ext) || ext == ".pdf" {
		c.Header("Content-Disposition", "inline")
	} else {
		dlName := tsPrefix.ReplaceAllString(filepath.Base(absPath), "")
		c.Header("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(dlName))
	}

	c.File(absPath)
}
