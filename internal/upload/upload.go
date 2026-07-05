// Package upload 處理附件上傳，以及 assets 目錄底下的附件清單、資料夾列舉與附件改名。
package upload

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/JonaWu05/DocNest/internal/authz"
	"github.com/JonaWu05/DocNest/internal/httpx"
	"github.com/JonaWu05/DocNest/internal/store"
)

// maxUploadSize 為單一上傳檔案大小上限（20 MB）
const maxUploadSize = 20 << 20

// AssetItem 代表一個已上傳的附件
type AssetItem struct {
	Path    string `json:"path"`    // 相對於 DOC_ROOT 的路徑
	Name    string `json:"name"`    // 檔名（含時間戳記前綴）
	IsImage bool   `json:"isImage"` // 是否為圖片
	Size    int64  `json:"size"`    // 檔案大小（位元組）
}

// AssetFolder 代表附件庫資料夾樹的一個節點（扁平清單，前端依路徑組成巢狀樹）。
type AssetFolder struct {
	Path     string `json:"path"`     // 相對於 DOC_ROOT 的路徑
	Writable bool   `json:"writable"` // 目前使用者是否可寫（上傳 / 改名 / 新增子資料夾）
}

// Upload 綁定檔案儲存與授權判斷。
type Upload struct {
	store *store.Store
	az    *authz.Authz
}

// New 建立 Upload handler 集合。
func New(st *store.Store, az *authz.Authz) *Upload {
	return &Upload{store: st, az: az}
}

// resolveAssetDir 驗證並正規化 dir 參數：必須落在 assets 樹底下，回傳正規化後的相對路徑。
// 先經 SafeResolve 消解 ..、. 等成分再判斷前綴，避免以 assets/../xxx 跳出 assets 樹。
// 失敗時已寫好錯誤回應，回傳 ok=false 供呼叫端提早返回。
func (u *Upload) resolveAssetDir(c *gin.Context, dir string) (string, bool) {
	dir = strings.Trim(strings.ReplaceAll(dir, "\\", "/"), "/")
	if dir == "" {
		dir = "assets"
	}
	abs, err := u.store.SafeResolve(dir)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的資料夾路徑"})
		return "", false
	}
	rel := u.store.RelOf(abs)
	if rel != "assets" && !strings.HasPrefix(rel, "assets/") {
		c.JSON(http.StatusForbidden, gin.H{"error": "附件只能存放在 assets 目錄底下"})
		return "", false
	}
	return rel, true
}

// UploadFile 處理 POST /api/upload：接收 multipart 上傳的圖片或附件，
// 存放到 assets 目錄底下，並回傳可供 Markdown 使用的相對路徑。
// 原始檔名含不合法字元（如中文、空白）時自動淨化，不直接拒絕。
func (u *Upload) UploadFile(c *gin.Context) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少上傳檔案"})
		return
	}

	if fileHeader.Size > maxUploadSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "檔案過大，上限為 20 MB"})
		return
	}

	ext := strings.ToLower(filepath.Ext(fileHeader.Filename))
	if !store.IsAllowedUpload(ext) {
		c.JSON(http.StatusForbidden, gin.H{"error": "不支援的檔案類型"})
		return
	}

	// 決定附件要存到哪個資料夾（一律限制在 assets 目錄樹底下）
	targetDir, ok := u.resolveAssetDir(c, c.PostForm("dir"))
	if !ok {
		return
	}

	// 存放名 = 時間戳記 + 淨化後檔名；filepath.Base 先去除客戶端夾帶的路徑成分
	origName := store.SanitizeName(filepath.Base(fileHeader.Filename), "file")
	if err := store.ValidateRelPath(origName); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "檔名不合法：" + err.Error()})
		return
	}
	unique := fmt.Sprintf("%d_%s", time.Now().UnixNano(), origName)
	storeRel := targetDir + "/" + unique

	absTarget, err := u.store.SafeResolve(storeRel)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的儲存路徑"})
		return
	}

	if !u.az.RequireAccess(c, u.store.RelOf(absTarget), authz.AccessWrite) {
		return
	}

	if err := os.MkdirAll(filepath.Dir(absTarget), 0o755); err != nil {
		httpx.ServerError(c, "建立目錄失敗", err)
		return
	}

	if err := c.SaveUploadedFile(fileHeader, absTarget); err != nil {
		httpx.ServerError(c, "儲存檔案失敗", err)
		return
	}

	u.store.InvalidateAssets() // 新增附件，使附件清單快取失效

	slog.Info("上傳附件", "by", c.GetString("username"), "path", filepath.ToSlash(storeRel), "size", fileHeader.Size)
	c.JSON(http.StatusOK, gin.H{
		"path":    filepath.ToSlash(storeRel),
		"name":    origName,
		"isImage": store.IsImageExt(ext),
	})
}

// ListAssets 處理 GET /api/assets?dir=xxx：自快取取得指定資料夾（預設 assets 根）
// 的直層附件，過濾使用者可讀的檔案後回傳。
func (u *Upload) ListAssets(c *gin.Context) {
	dir, ok := u.resolveAssetDir(c, c.Query("dir"))
	if !ok {
		return
	}

	subject := authz.SubjectOf(c)
	entries, err := u.store.ScanAssets()
	if err != nil {
		httpx.ServerError(c, "讀取附件失敗", err)
		return
	}

	// 只取指定資料夾的直層檔案（路徑去掉 dir 前綴後不再含 /）
	prefix := dir + "/"
	items := []AssetItem{}
	for _, e := range entries {
		if e.IsDir || !strings.HasPrefix(e.Path, prefix) || strings.Contains(e.Path[len(prefix):], "/") {
			continue
		}
		// 權限過濾：只列出使用者有讀取權的附件
		if !u.az.Can(subject, e.Path, authz.AccessRead) {
			continue
		}
		items = append(items, AssetItem{
			Path:    e.Path,
			Name:    e.Name,
			IsImage: e.IsImage,
			Size:    e.Size,
		})
	}

	// 依路徑由新到舊排序（檔名含時間戳記前綴，反向排序即最新在前）
	sort.Slice(items, func(i, j int) bool {
		return items[i].Path > items[j].Path
	})

	c.JSON(http.StatusOK, gin.H{"dir": dir, "assets": items})
}

// ListAssetFolders 處理 GET /api/asset-folders：列出 assets 樹底下使用者可見的資料夾。
// 可見規則比照主檔案樹 filterTree：自身可讀或可寫的資料夾保留；
// 另讓可見項目（含可讀檔案）的祖先資料夾穿透保留，使用者才能逐層導覽。
func (u *Upload) ListAssetFolders(c *gin.Context) {
	subject := authz.SubjectOf(c)
	entries, err := u.store.ScanAssets()
	if err != nil {
		httpx.ServerError(c, "讀取附件資料夾失敗", err)
		return
	}

	visible := map[string]bool{}
	if u.az.Can(subject, "assets", authz.AccessRead) || u.az.Can(subject, "assets", authz.AccessWrite) {
		visible["assets"] = true
	}
	// addWithAncestors 將資料夾與其所有上層（直到 assets 根）標記為可見
	addWithAncestors := func(p string) {
		for p != "assets" && !visible[p] {
			visible[p] = true
			p = path.Dir(p)
		}
		visible["assets"] = true
	}
	for _, e := range entries {
		canSee := u.az.Can(subject, e.Path, authz.AccessRead) ||
			(e.IsDir && u.az.Can(subject, e.Path, authz.AccessWrite))
		if !canSee {
			continue
		}
		if e.IsDir {
			addWithAncestors(e.Path)
		} else {
			addWithAncestors(path.Dir(e.Path))
		}
	}

	folders := make([]AssetFolder, 0, len(visible))
	for p := range visible {
		folders = append(folders, AssetFolder{
			Path:     p,
			Writable: u.az.Can(subject, p, authz.AccessWrite),
		})
	}
	sort.Slice(folders, func(i, j int) bool { return folders[i].Path < folders[j].Path })
	c.JSON(http.StatusOK, gin.H{"folders": folders})
}

// RenameAsset 處理 POST /api/asset/rename?path=old&newName=name：重新命名單一附件檔案。
// 只更動使用者可見的檔名部分：既有「時間戳記_」前綴保留、副檔名不可變更，
// 新名稱僅限單一路徑分段（不可含路徑分隔，即不能藉改名移動檔案）。
func (u *Upload) RenameAsset(c *gin.Context) {
	rel := c.Query("path")
	newName := c.Query("newName")
	if rel == "" || newName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 path 或 newName 參數"})
		return
	}

	oldAbs, err := u.store.SafeResolve(rel)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的檔案路徑"})
		return
	}
	oldRel := u.store.RelOf(oldAbs)
	if !strings.HasPrefix(oldRel, "assets/") {
		c.JSON(http.StatusForbidden, gin.H{"error": "只能重新命名 assets 底下的附件"})
		return
	}

	if strings.ContainsAny(newName, "/\\") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "新檔名不可包含路徑分隔"})
		return
	}
	if err := store.ValidateRelPath(newName); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "檔名不合法：" + err.Error()})
		return
	}

	info, err := os.Stat(oldAbs)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "附件不存在"})
			return
		}
		httpx.ServerError(c, "讀取狀態失敗", err)
		return
	}
	if info.IsDir() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "只能重新命名附件檔案"})
		return
	}

	// 副檔名不可變更——改副檔名等同繞過上傳時的類型允許清單
	oldBase := path.Base(oldRel)
	if !strings.EqualFold(filepath.Ext(newName), filepath.Ext(oldBase)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不可變更副檔名"})
		return
	}

	// 保留既有時間戳記前綴（手動放入而無前綴的檔案則不加）
	prefix := store.TimestampPrefix.FindString(oldBase)
	newRel := path.Dir(oldRel) + "/" + prefix + newName
	if newRel == oldRel {
		c.JSON(http.StatusOK, gin.H{"path": oldRel, "name": oldBase})
		return
	}

	newAbs, err := u.store.SafeResolve(newRel)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的目標路徑"})
		return
	}

	// 改名同時影響來源與目的地，兩端都需具備寫入權
	if !u.az.RequireAccess(c, oldRel, authz.AccessWrite) || !u.az.RequireAccess(c, u.store.RelOf(newAbs), authz.AccessWrite) {
		return
	}

	if err := store.RenameFile(oldAbs, newAbs); err != nil {
		if errors.Is(err, os.ErrExist) {
			c.JSON(http.StatusConflict, gin.H{"error": "目標檔名已存在"})
			return
		}
		httpx.ServerError(c, "重新命名失敗", err)
		return
	}

	u.store.InvalidateAssets()
	slog.Info("重新命名附件", "by", c.GetString("username"), "from", oldRel, "to", newRel)
	c.JSON(http.StatusOK, gin.H{"path": newRel, "name": prefix + newName})
}
