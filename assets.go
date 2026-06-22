package main

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

// AssetItem 代表一個已上傳的附件
type AssetItem struct {
	Path    string `json:"path"`    // 相對於 DOC_ROOT 的路徑
	Name    string `json:"name"`    // 檔名（含時間戳記前綴）
	IsImage bool   `json:"isImage"` // 是否為圖片
	Size    int64  `json:"size"`    // 檔案大小（位元組）
}

// listAssetsHandler 處理 GET /api/assets：掃描根目錄 assets 樹底下的所有檔案，
// 回傳清單供前端「從已上傳的附件中挑選」重複使用。
func listAssetsHandler(c *gin.Context) {
	items := []AssetItem{}
	subject := subjectOf(c)
	assetsRoot := filepath.Join(docRoot, "assets")

	// assets 目錄存在才掃描（尚未上傳任何附件時即為空清單）
	if info, err := os.Stat(assetsRoot); err == nil && info.IsDir() {
		_ = filepath.WalkDir(assetsRoot, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if d.IsDir() {
				// 略過隱藏目錄
				if p != assetsRoot && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(docRoot, p)
			if err != nil {
				return nil
			}
			// 權限過濾：只列出使用者有讀取權的附件
			if !canAccess(subject, filepath.ToSlash(rel), accessRead) {
				return nil
			}
			var size int64
			if fi, err := d.Info(); err == nil {
				size = fi.Size()
			}
			items = append(items, AssetItem{
				Path:    filepath.ToSlash(rel), // 統一使用 / 分隔
				Name:    d.Name(),
				IsImage: isImageExt(filepath.Ext(p)),
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

// listAssetFoldersHandler 處理 GET /api/asset-folders：列出 assets 樹底下的所有資料夾，
// 供前端的「上傳目的地」下拉選單與新增資料夾使用（不會列出文件資料夾如 notes）。
func listAssetFoldersHandler(c *gin.Context) {
	subject := subjectOf(c)
	folders := []string{}
	// 根 assets 至少在使用者可寫時提供為預設目的地
	if canAccess(subject, "assets", accessWrite) {
		folders = append(folders, "assets")
	}
	assetsRoot := filepath.Join(docRoot, "assets")

	if info, err := os.Stat(assetsRoot); err == nil && info.IsDir() {
		_ = filepath.WalkDir(assetsRoot, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if !d.IsDir() {
				return nil
			}
			if p == assetsRoot {
				return nil // 根已加入
			}
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			// 只列出使用者有寫入權的資料夾（作為上傳目的地）
			if rel, err := filepath.Rel(docRoot, p); err == nil {
				slash := filepath.ToSlash(rel)
				if canAccess(subject, slash, accessWrite) {
					folders = append(folders, slash)
				}
			}
			return nil
		})
	}

	sort.Strings(folders)
	c.JSON(http.StatusOK, gin.H{"folders": folders})
}
