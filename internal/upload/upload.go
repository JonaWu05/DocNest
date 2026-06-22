// Package upload 處理附件上傳，以及 assets 目錄底下的附件清單與資料夾列舉。
package upload

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"markdownEditor/internal/authz"
	"markdownEditor/internal/store"
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

// Upload 綁定檔案儲存與授權判斷。
type Upload struct {
	store *store.Store
	az    *authz.Authz
}

// New 建立 Upload handler 集合。
func New(st *store.Store, az *authz.Authz) *Upload {
	return &Upload{store: st, az: az}
}

// UploadFile 處理 POST /api/upload：接收 multipart 上傳的圖片或附件，
// 存放到 assets 目錄底下，並回傳可供 Markdown 使用的相對路徑。
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
	targetDir := "assets"
	if v, ok := c.GetPostForm("dir"); ok {
		if t := strings.Trim(strings.ReplaceAll(v, "\\", "/"), "/"); t != "" {
			targetDir = t
		}
	}
	if targetDir != "assets" && !strings.HasPrefix(targetDir, "assets/") {
		c.JSON(http.StatusForbidden, gin.H{"error": "附件只能存放在 assets 目錄底下"})
		return
	}

	// 產生不重複的檔名（時間戳記 + 原始檔名），filepath.Base 可去除任何路徑成分
	origName := filepath.Base(fileHeader.Filename)
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "建立目錄失敗：" + err.Error()})
		return
	}

	if err := c.SaveUploadedFile(fileHeader, absTarget); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "儲存檔案失敗：" + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"path":    filepath.ToSlash(storeRel),
		"name":    origName,
		"isImage": store.IsImageExt(ext),
	})
}

// ListAssets 處理 GET /api/assets：掃描 assets 樹底下所有檔案（過濾使用者可讀者）回傳。
func (u *Upload) ListAssets(c *gin.Context) {
	items := []AssetItem{}
	subject := authz.SubjectOf(c)
	assetsRoot := filepath.Join(u.store.Root, "assets")

	if info, err := os.Stat(assetsRoot); err == nil && info.IsDir() {
		_ = filepath.WalkDir(assetsRoot, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if d.IsDir() {
				if p != assetsRoot && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(u.store.Root, p)
			if err != nil {
				return nil
			}
			// 權限過濾：只列出使用者有讀取權的附件
			if !u.az.Can(subject, filepath.ToSlash(rel), authz.AccessRead) {
				return nil
			}
			var size int64
			if fi, err := d.Info(); err == nil {
				size = fi.Size()
			}
			items = append(items, AssetItem{
				Path:    filepath.ToSlash(rel),
				Name:    d.Name(),
				IsImage: store.IsImageExt(filepath.Ext(p)),
				Size:    size,
			})
			return nil
		})
	}

	// 依路徑由新到舊排序（檔名含時間戳記前綴，反向排序即最新在前）
	sort.Slice(items, func(i, j int) bool {
		return items[i].Path > items[j].Path
	})

	c.JSON(http.StatusOK, gin.H{"assets": items})
}

// ListAssetFolders 處理 GET /api/asset-folders：列出 assets 樹底下使用者可寫的資料夾（上傳目的地）。
func (u *Upload) ListAssetFolders(c *gin.Context) {
	subject := authz.SubjectOf(c)
	folders := []string{}
	if u.az.Can(subject, "assets", authz.AccessWrite) {
		folders = append(folders, "assets")
	}
	assetsRoot := filepath.Join(u.store.Root, "assets")

	if info, err := os.Stat(assetsRoot); err == nil && info.IsDir() {
		_ = filepath.WalkDir(assetsRoot, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if !d.IsDir() {
				return nil
			}
			if p == assetsRoot {
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			// 只列出使用者有寫入權的資料夾
			if rel, err := filepath.Rel(u.store.Root, p); err == nil {
				slash := filepath.ToSlash(rel)
				if u.az.Can(subject, slash, authz.AccessWrite) {
					folders = append(folders, slash)
				}
			}
			return nil
		})
	}

	sort.Strings(folders)
	c.JSON(http.StatusOK, gin.H{"folders": folders})
}
