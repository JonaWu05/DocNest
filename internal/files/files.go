// Package files 提供文件的 CRUD 與原始檔案（圖片/附件）服務，並依權限過濾檔案樹。
package files

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"markdownEditor/internal/authz"
	"markdownEditor/internal/httpx"
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
	fsync bool // 存檔是否強制刷盤（由設定注入）
}

// New 建立 Files handler 集合。
func New(st *store.Store, az *authz.Authz, h *hub.Hub, fsync bool) *Files {
	return &Files{store: st, az: az, hub: h, fsync: fsync}
}

// fileVersion 由檔案的大小與修改時間（奈秒）組出版本識別，作為樂觀鎖的版本（ETag 風格）。
// 不需讀取整檔內容即可比對，省去每次存檔的整檔重讀（沿用 nginx/Apache 產生 ETag 的做法）。
func fileVersion(info os.FileInfo) string {
	return fmt.Sprintf("%x-%x", info.Size(), info.ModTime().UnixNano())
}

// invalidateFor 依相對路徑讓對應的快取失效：assets 底下 → 附件快取；其餘 → 檔案樹快取。
// （assets 目錄不在主檔案樹內，兩者為互斥子樹。）
func (f *Files) invalidateFor(rel string) {
	if rel == "assets" || strings.HasPrefix(rel, "assets/") {
		f.store.InvalidateAssets()
	} else {
		f.store.InvalidateTree()
	}
}

// filterTree 依 subject 的讀取權過濾檔案樹：
//   - 檔案：可讀才保留
//   - 資料夾：自身可讀、或含有可讀後代時保留（讓使用者能逐層導覽到可讀檔案）
//
// 同時標記每個保留節點的 Writable，供前端隱藏無權限的編輯／刪除操作。
//
// 產生「新節點」而非就地修改輸入：輸入來自共用的快取樹（見 store.CachedTree），
// 不可被各請求的權限過濾汙染。
func (f *Files) filterTree(nodes []*store.FileNode, subject string) []*store.FileNode {
	out := []*store.FileNode{}
	for _, n := range nodes {
		if n.IsDir {
			children := f.filterTree(n.Children, subject)
			if len(children) > 0 || f.az.Can(subject, n.Path, authz.AccessRead) {
				out = append(out, &store.FileNode{
					Name:     n.Name,
					Path:     n.Path,
					IsDir:    true,
					Writable: f.az.Can(subject, n.Path, authz.AccessWrite),
					Children: children,
				})
			}
		} else if f.az.Can(subject, n.Path, authz.AccessRead) {
			out = append(out, &store.FileNode{
				Name:     n.Name,
				Path:     n.Path,
				IsDir:    false,
				Writable: f.az.Can(subject, n.Path, authz.AccessWrite),
			})
		}
	}
	return out
}

// ListFiles 處理 GET /api/files：取得檔案樹（快取），依使用者讀取權過濾後回傳。
func (f *Files) ListFiles(c *gin.Context) {
	tree, err := f.store.CachedTree()
	if err != nil {
		httpx.ServerError(c, "無法讀取文件目錄", err)
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

	// 以同一開啟句柄取得內容與版本（size+mtime），避免讀取與 stat 之間檔案被改而不一致。
	file, err := os.Open(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "檔案不存在"})
			return
		}
		httpx.ServerError(c, "讀取檔案失敗", err)
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		httpx.ServerError(c, "讀取狀態失敗", err)
		return
	}
	content, err := io.ReadAll(file)
	if err != nil {
		httpx.ServerError(c, "讀取檔案失敗", err)
		return
	}

	// 帶上版本識別，供前端做樂觀鎖
	c.Header("X-File-Version", fileVersion(info))
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

	// 取現有檔案狀態一次，供樂觀鎖比對與「是否新檔」判斷共用（僅 metadata，不讀內容）。
	info, statErr := os.Stat(absPath)
	isNew := os.IsNotExist(statErr)

	// 樂觀鎖：用戶端帶了基準版本且與磁碟現況（size+mtime）不符 → 回 409，交由前端讓使用者決定。
	// force=1 表示使用者明確選擇覆蓋，略過檢查；新檔（磁碟上不存在）也無從比對，直接建立。
	if base := c.GetHeader("X-File-Version"); base != "" && c.Query("force") != "1" && statErr == nil {
		if cur := fileVersion(info); cur != base {
			c.JSON(http.StatusConflict, gin.H{
				"error":           "檔案已被其他人更新，請先載入最新版本",
				"current_version": cur,
			})
			return
		}
	}

	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		httpx.ServerError(c, "建立目錄失敗", err)
		return
	}

	// 原子寫入：先寫暫存檔再 rename，避免並發讀者讀到半寫入內容。fsync 由設定控制。
	if err := store.AtomicWrite(absPath, body, 0o644, f.fsync); err != nil {
		httpx.ServerError(c, "寫入檔案失敗", err)
		return
	}
	if isNew {
		// 只有新增檔案才改變結構，使對應快取失效；覆寫既有檔不必。
		f.invalidateFor(f.store.RelOf(absPath))
	}

	// 廣播 file_updated 通知（僅 path + savedBy，不含內容）。savedBy 取自 JWT，不採信前端。
	// 路徑用 RelOf 正規化，與 hub 廣播時的讀取權過濾、前端的 currentFile 比對保持一致。
	f.hub.BroadcastFileUpdated(f.store.RelOf(absPath), c.GetString("username"))

	// 回傳寫入後的新版本（size+mtime），供前端更新樂觀鎖基準。
	if newInfo, err := os.Stat(absPath); err == nil {
		c.Header("X-File-Version", fileVersion(newInfo))
	}
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
		httpx.ServerError(c, "讀取狀態失敗", err)
		return
	}

	if info.IsDir() {
		if err := os.RemoveAll(absPath); err != nil {
			httpx.ServerError(c, "刪除資料夾失敗", err)
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
			httpx.ServerError(c, "刪除檔案失敗", err)
			return
		}
	}

	f.invalidateFor(f.store.RelOf(absPath))
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

	if err := store.ValidateRelPath(pathParam); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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
			httpx.ServerError(c, "建立資料夾失敗", err)
			return
		}
	} else {
		if !store.IsAllowedFile(absPath) {
			c.JSON(http.StatusForbidden, gin.H{"error": "僅能建立 .md 或 .txt 檔案"})
			return
		}
		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			httpx.ServerError(c, "建立目錄失敗", err)
			return
		}
		if err := os.WriteFile(absPath, []byte{}, 0o644); err != nil {
			httpx.ServerError(c, "建立檔案失敗", err)
			return
		}
	}

	f.invalidateFor(f.store.RelOf(absPath))
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

	// 目標路徑為使用者新提供的名稱，須驗證合法性（來源已存在、不再重驗）。
	if err := store.ValidateRelPath(newParam); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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
		httpx.ServerError(c, "讀取狀態失敗", err)
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
		httpx.ServerError(c, "建立目錄失敗", err)
		return
	}

	if err := os.Rename(oldAbs, newAbs); err != nil {
		httpx.ServerError(c, "重新命名失敗", err)
		return
	}

	// 改名/移動可能跨越 doc 樹與 assets 樹，兩端各自讓對應快取失效。
	f.invalidateFor(f.store.RelOf(oldAbs))
	f.invalidateFor(f.store.RelOf(newAbs))
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
		httpx.ServerError(c, "讀取狀態失敗", err)
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
