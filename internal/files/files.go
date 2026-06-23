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
	"sync"

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

	// 來源文件 → 其引用的 asset 集合的解析快取（供 Raw 的「來源驗證」授權使用）。
	// 以 size+mtime 版本識別判斷是否過期，避免同一頁多張圖重複讀檔解析。
	refMu    sync.RWMutex
	refCache map[string]refCacheEntry
}

// refCacheEntry 為單一來源文件的解析結果。
type refCacheEntry struct {
	version string          // 解析當下來源文件的版本（size+mtime）
	refs    map[string]bool // 該文件引用的 asset（DOC_ROOT 相對路徑）集合
}

// New 建立 Files handler 集合。
func New(st *store.Store, az *authz.Authz, h *hub.Hub, fsync bool) *Files {
	return &Files{store: st, az: az, hub: h, fsync: fsync, refCache: map[string]refCacheEntry{}}
}

// fileVersion 由檔案的大小與修改時間（奈秒）組出版本識別，作為樂觀鎖的版本（ETag 風格）。
// 不需讀取整檔內容即可比對，省去每次存檔的整檔重讀（沿用 nginx/Apache 產生 ETag 的做法）。
func fileVersion(info os.FileInfo) string {
	return fmt.Sprintf("%x-%x", info.Size(), info.ModTime().UnixNano())
}

// resolvePathParam 取出指定 query 參數，並解析成 Root 內的安全絕對路徑。
// 失敗時已寫好對應錯誤回應（缺參數 → 400；非法/越界路徑 → 403），回傳 ok=false 供呼叫端提早返回。
// 同時回傳原始相對參數，供需要驗證檔名（ValidateRelPath）的呼叫端使用。
func (f *Files) resolvePathParam(c *gin.Context, key string) (param, absPath string, ok bool) {
	param = c.Query(key)
	if param == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 " + key + " 參數"})
		return "", "", false
	}
	absPath, err := f.store.SafeResolve(param)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的檔案路徑"})
		return "", "", false
	}
	return param, absPath, true
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
	_, absPath, ok := f.resolvePathParam(c, "path")
	if !ok {
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
	_, absPath, ok := f.resolvePathParam(c, "path")
	if !ok {
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
	_, absPath, ok := f.resolvePathParam(c, "path")
	if !ok {
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
	itemType := c.Query("type")
	if itemType != "file" && itemType != "dir" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "type 參數必須為 file 或 dir"})
		return
	}

	pathParam, absPath, ok := f.resolvePathParam(c, "path")
	if !ok {
		return
	}

	if err := store.ValidateRelPath(pathParam); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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
	_, oldAbs, ok := f.resolvePathParam(c, "path")
	if !ok {
		return
	}
	newParam, newAbs, ok := f.resolvePathParam(c, "newPath")
	if !ok {
		return
	}

	// 目標路徑為使用者新提供的名稱，須驗證合法性（來源已存在、不再重驗）。
	if err := store.ValidateRelPath(newParam); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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
	_, absPath, ok := f.resolvePathParam(c, "path")
	if !ok {
		return
	}

	// 原始檔案服務同樣需讀取權，避免繞過樹過濾直接讀取。
	// 直接權限不足時，再嘗試「來源文件驗證」：閱讀者可檢視自己有權讀、
	// 且該頁確實引用到的 asset（見 allowViaReferrer）。
	relPath := f.store.RelOf(absPath)
	if !f.az.Can(authz.SubjectOf(c), relPath, authz.AccessRead) && !f.allowViaReferrer(c, relPath) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "權限不足"})
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
		// 附件內容實質不可變（檔名含時間戳記，更新即換新檔），可讓瀏覽器長時間快取，
		// 避免同一頁/重複瀏覽反覆回源。private：含 token，不可由中介快取共用。
		c.Header("Cache-Control", "private, max-age=86400")
	} else {
		dlName := tsPrefix.ReplaceAllString(filepath.Base(absPath), "")
		c.Header("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(dlName))
	}

	c.File(absPath)
}

// mdLinkRe 擷取 Markdown 圖片/連結的目標：![alt](target) 與 [text](target)。
// htmlSrcRe 擷取原始 HTML 的 <img src> / <a href>，涵蓋 Markdown 內嵌 HTML 的引用。
var (
	mdLinkRe   = regexp.MustCompile(`!?\[[^\]]*\]\(([^)]+)\)`)
	htmlSrcRe  = regexp.MustCompile(`(?i)<(?:img|a)\b[^>]*?(?:src|href)\s*=\s*["']([^"']+)["']`)
	externalRe = regexp.MustCompile(`(?i)^(?:https?:|data:|mailto:|#|/)`)
)

// allowViareferrer 判斷閱讀者能否經由「來源文件」檢視某個自己沒有直接讀取權的 asset：
// 需同時滿足 (1) 對 from 文件有讀取權、(2) 該文件確實引用了這個 asset。
// 兩者皆成立，等同「使用者讀得到的頁面內嵌的圖」，不致被拿來撈未授權的其他附件。
func (f *Files) allowViaReferrer(c *gin.Context, assetRel string) bool {
	from := strings.TrimSpace(c.Query("from"))
	if from == "" {
		return false
	}
	fromAbs, err := f.store.SafeResolve(from)
	if err != nil {
		return false
	}
	fromRel := f.store.RelOf(fromAbs)
	// 來源必須是合法文件且使用者有讀取權
	if !store.IsAllowedFile(fromAbs) || !f.az.Can(authz.SubjectOf(c), fromRel, authz.AccessRead) {
		return false
	}
	refs, ok := f.docReferences(fromAbs, fromRel)
	return ok && refs[assetRel]
}

// docReferences 回傳來源文件引用的 asset 集合（DOC_ROOT 相對路徑），以 size+mtime 版本快取。
// 同一頁多張圖只需解析一次；文件內容變更（mtime 改變）後自動失效重算。
func (f *Files) docReferences(absPath, relPath string) (map[string]bool, bool) {
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, false
	}
	version := fileVersion(info)

	f.refMu.RLock()
	entry, hit := f.refCache[relPath]
	f.refMu.RUnlock()
	if hit && entry.version == version {
		return entry.refs, true
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, false
	}
	refs := extractAssetRefs(string(data), relPath)

	f.refMu.Lock()
	f.refCache[relPath] = refCacheEntry{version: version, refs: refs}
	f.refMu.Unlock()
	return refs, true
}

// extractAssetRefs 解析文件內容，回傳其引用的本機 asset（DOC_ROOT 相對路徑）集合。
// 外部連結（http、data、mailto、# 錨點、絕對路徑）一律略過；相對路徑以來源文件所在目錄解析，
// 與前端 util.js 的 resolveAssetPath 規則一致，確保授權判斷與實際渲染的連結對得上。
func extractAssetRefs(content, docRel string) map[string]bool {
	refs := map[string]bool{}
	collect := func(matches [][]string) {
		for _, m := range matches {
			raw := strings.TrimSpace(m[1])
			// 去掉 Markdown 連結可能帶的標題：](path "title") 取第一段；以及 <path> 角括號
			if i := strings.IndexAny(raw, " \t"); i >= 0 {
				raw = raw[:i]
			}
			raw = strings.Trim(raw, "<>")
			if raw == "" || externalRe.MatchString(raw) {
				continue
			}
			if dec, err := url.PathUnescape(raw); err == nil {
				raw = dec
			}
			if resolved := resolveAssetRel(docRel, raw); resolved != "" {
				refs[resolved] = true
			}
		}
	}
	collect(mdLinkRe.FindAllStringSubmatch(content, -1))
	collect(htmlSrcRe.FindAllStringSubmatch(content, -1))
	return refs
}

// resolveAssetRel 將「相對於來源文件」的連結換算成 DOC_ROOT 相對路徑（處理 . 與 ..）。
// 對應前端 util.js resolveAssetPath 的堆疊解析。
func resolveAssetRel(docRel, src string) string {
	parts := []string{}
	if i := strings.LastIndex(docRel, "/"); i >= 0 {
		parts = append(parts, strings.Split(docRel[:i], "/")...)
	}
	parts = append(parts, strings.Split(src, "/")...)
	stack := []string{}
	for _, p := range parts {
		switch p {
		case "", ".":
			// 略過
		case "..":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		default:
			stack = append(stack, p)
		}
	}
	return strings.Join(stack, "/")
}
