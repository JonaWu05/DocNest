package main

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
)

// tsPrefix 比對上傳檔名的「時間戳記_」前綴（對應前端 assetDisplayName 的清理規則）
var tsPrefix = regexp.MustCompile(`^\d+_`)

// fileVersion 由內容算出短雜湊，作為樂觀鎖的版本識別。
// 用內容雜湊而非 mtime：不受作業系統時間解析度影響，相同內容必得相同版本。
func fileVersion(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8]) // 16 個十六進位字元，足以區辨版本
}

// maxWriteSize 為單次寫入檔案的 request body 上限（10 MB）。
// 避免已登入者送出超大 body 經 io.ReadAll 一次讀進記憶體而撐爆服務。
const maxWriteSize = 10 << 20

// listFilesHandler 處理 GET /api/files：遞迴掃描 DOC_ROOT，建立並回傳樹狀結構
func listFilesHandler(c *gin.Context) {
	tree, err := buildTree(docRoot, "")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "無法讀取文件目錄：" + err.Error()})
		return
	}
	// 直接回傳根目錄底下的子節點清單
	c.JSON(http.StatusOK, gin.H{"files": tree.Children})
}

// readFileHandler 處理 GET /api/file?path=xxx：讀取檔案內容並以純文字回傳
func readFileHandler(c *gin.Context) {
	pathParam := c.Query("path")
	if pathParam == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 path 參數"})
		return
	}

	// 路徑安全檢查
	absPath, err := safeResolve(pathParam)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的檔案路徑"})
		return
	}

	// 僅允許讀取 .md / .txt 檔案
	if !isAllowedFile(absPath) {
		c.JSON(http.StatusForbidden, gin.H{"error": "不支援的檔案類型"})
		return
	}

	// 讀取檔案內容
	content, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "檔案不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "讀取檔案失敗：" + err.Error()})
		return
	}

	// 帶上版本識別，供前端做樂觀鎖（儲存時回傳此版本，後端比對是否被他人覆蓋過）
	c.Header("X-File-Version", fileVersion(content))
	// 以純文字（UTF-8）回傳內容
	c.Data(http.StatusOK, "text/plain; charset=utf-8", content)
}

// writeFileHandler 處理 POST /api/file?path=xxx：將 request body 寫入指定檔案
func writeFileHandler(c *gin.Context) {
	pathParam := c.Query("path")
	if pathParam == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 path 參數"})
		return
	}

	// 路徑安全檢查
	absPath, err := safeResolve(pathParam)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的檔案路徑"})
		return
	}

	// 僅允許寫入 .md / .txt 檔案
	if !isAllowedFile(absPath) {
		c.JSON(http.StatusForbidden, gin.H{"error": "不支援的檔案類型"})
		return
	}

	// 讀取 request body 作為檔案內容（限制大小，避免超大 body 撐爆記憶體）
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxWriteSize))
	if err != nil {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "內容過大或讀取失敗（單檔上限 10 MB）"})
		return
	}

	// 樂觀鎖：用戶端帶了基準版本（X-File-Version）且與磁碟現況不符，代表編輯期間
	// 已被他人儲存過，直接覆蓋會吃掉對方的變更 → 回 409，交由前端讓使用者決定。
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

	// 確保目標檔案所在的資料夾存在（允許寫入新建子目錄的檔案）
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "建立目錄失敗：" + err.Error()})
		return
	}

	// 寫入檔案（覆寫既有內容）
	if err := os.WriteFile(absPath, body, 0o644); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "寫入檔案失敗：" + err.Error()})
		return
	}

	// 廣播 file_updated 通知給所有 WebSocket 連線（僅 path + savedBy，不含內容）。
	// path 用前端送來的相對路徑（統一為 / 分隔），與前端比對 currentPath 一致；
	// savedBy 取自 JWT（經 AuthMiddleware 存入 context），不採信前端身份。
	relPath := strings.ReplaceAll(pathParam, "\\", "/")
	broadcastFileUpdated(relPath, c.GetString("username"))

	// 回傳寫入後的新版本，前端據此更新基準（避免自己存完又被自己的廣播判定成衝突）
	c.Header("X-File-Version", fileVersion(body))
	c.JSON(http.StatusOK, gin.H{"message": "儲存成功"})
}

// deleteFileHandler 處理 DELETE /api/file?path=xxx：刪除檔案或資料夾
func deleteFileHandler(c *gin.Context) {
	pathParam := c.Query("path")
	if pathParam == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 path 參數"})
		return
	}

	// 路徑安全檢查
	absPath, err := safeResolve(pathParam)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的檔案路徑"})
		return
	}

	// 不允許刪除根目錄本身
	if absPath == docRoot {
		c.JSON(http.StatusForbidden, gin.H{"error": "不可刪除文件根目錄"})
		return
	}

	// 確認目標存在
	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "檔案或資料夾不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "讀取狀態失敗：" + err.Error()})
		return
	}

	// 資料夾用 RemoveAll（含底下內容），檔案則限制副檔名後刪除
	if info.IsDir() {
		if err := os.RemoveAll(absPath); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "刪除資料夾失敗：" + err.Error()})
			return
		}
	} else {
		// 允許刪除文件（.md/.txt）以及已上傳的圖片/附件
		ext := strings.ToLower(filepath.Ext(absPath))
		if !isAllowedFile(absPath) && !isAllowedUpload(ext) {
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

// createHandler 處理 POST /api/create?path=xxx&type=file|dir：新增檔案或資料夾
func createHandler(c *gin.Context) {
	pathParam := c.Query("path")
	itemType := c.Query("type") // "file" 或 "dir"
	if pathParam == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 path 參數"})
		return
	}
	if itemType != "file" && itemType != "dir" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "type 參數必須為 file 或 dir"})
		return
	}

	// 路徑安全檢查
	absPath, err := safeResolve(pathParam)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的檔案路徑"})
		return
	}

	// 目標已存在則拒絕，避免覆蓋既有資料
	if _, err := os.Stat(absPath); err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "同名的檔案或資料夾已存在"})
		return
	}

	if itemType == "dir" {
		// 建立資料夾
		if err := os.MkdirAll(absPath, 0o755); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "建立資料夾失敗：" + err.Error()})
			return
		}
	} else {
		// 建立檔案：限制副檔名，並確保上層目錄存在
		if !isAllowedFile(absPath) {
			c.JSON(http.StatusForbidden, gin.H{"error": "僅能建立 .md 或 .txt 檔案"})
			return
		}
		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "建立目錄失敗：" + err.Error()})
			return
		}
		// 以空內容建立新檔
		if err := os.WriteFile(absPath, []byte{}, 0o644); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "建立檔案失敗：" + err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"message": "建立成功"})
}

// renameHandler 處理 POST /api/rename?path=old&newPath=new：重新命名或移動檔案/資料夾
func renameHandler(c *gin.Context) {
	oldParam := c.Query("path")
	newParam := c.Query("newPath")
	if oldParam == "" || newParam == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 path 或 newPath 參數"})
		return
	}

	// 來源與目標路徑都要做安全檢查
	oldAbs, err := safeResolve(oldParam)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的來源路徑"})
		return
	}
	newAbs, err := safeResolve(newParam)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的目標路徑"})
		return
	}

	// 不允許移動根目錄本身
	if oldAbs == docRoot {
		c.JSON(http.StatusForbidden, gin.H{"error": "不可移動文件根目錄"})
		return
	}

	// 確認來源存在
	info, err := os.Stat(oldAbs)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "來源不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "讀取狀態失敗：" + err.Error()})
		return
	}

	// 若來源是檔案，目標也必須是允許的副檔名
	if !info.IsDir() && !isAllowedFile(newAbs) {
		c.JSON(http.StatusForbidden, gin.H{"error": "目標檔名必須為 .md 或 .txt"})
		return
	}

	// 目標已存在則拒絕
	if _, err := os.Stat(newAbs); err == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "目標已存在"})
		return
	}

	// 確保目標上層目錄存在
	if err := os.MkdirAll(filepath.Dir(newAbs), 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "建立目錄失敗：" + err.Error()})
		return
	}

	// 執行更名 / 移動
	if err := os.Rename(oldAbs, newAbs); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "重新命名失敗：" + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "重新命名成功"})
}

// rawFileHandler 處理 GET /api/raw?path=xxx：直接提供原始檔案內容（供圖片/附件顯示與下載）。
// 與 readFileHandler 不同，這裡不限制副檔名，但仍受 DOC_ROOT 路徑安全檢查保護。
func rawFileHandler(c *gin.Context) {
	pathParam := c.Query("path")
	if pathParam == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 path 參數"})
		return
	}

	// 路徑安全檢查，確保只能存取 DOC_ROOT 底下的檔案
	absPath, err := safeResolve(pathParam)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的檔案路徑"})
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
	//   - X-Content-Type-Options: nosniff 禁止 MIME 嗅探，瀏覽器只認回應的 Content-Type
	//   - 僅圖片與 PDF 允許 inline（供 <img> 顯示與瀏覽器內預覽），其餘一律強制下載（attachment）
	ext := strings.ToLower(filepath.Ext(absPath))
	c.Header("X-Content-Type-Options", "nosniff")
	if isImageExt(ext) || ext == ".pdf" {
		c.Header("Content-Disposition", "inline")
	} else {
		// filename* 採 RFC 5987 編碼以保留非 ASCII 檔名；去掉上傳時的時間戳記前綴作為友善下載名
		dlName := tsPrefix.ReplaceAllString(filepath.Base(absPath), "")
		c.Header("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(dlName))
	}

	// 由 gin 依副檔名自動帶入適當的 Content-Type 並回傳檔案
	c.File(absPath)
}
