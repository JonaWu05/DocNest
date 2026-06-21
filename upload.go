package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// maxUploadSize 為單一上傳檔案大小上限（20 MB）
const maxUploadSize = 20 << 20

// uploadHandler 處理 POST /api/upload：接收 multipart 表單上傳的圖片或附件，
// 將檔案存放到「文件所在目錄底下的 assets 資料夾」，並回傳可供 Markdown 使用的相對路徑。
//
// 表單欄位：
//   - file    ：上傳的檔案（必填）
//   - docPath ：目前文件的相對路徑（選填，用來決定 assets 要放在哪個目錄）
func uploadHandler(c *gin.Context) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少上傳檔案"})
		return
	}

	// 檔案大小檢查
	if fileHeader.Size > maxUploadSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "檔案過大，上限為 20 MB"})
		return
	}

	// 副檔名白名單檢查
	ext := strings.ToLower(filepath.Ext(fileHeader.Filename))
	if !isAllowedUpload(ext) {
		c.JSON(http.StatusForbidden, gin.H{"error": "不支援的檔案類型"})
		return
	}

	// 決定附件要存到哪個資料夾（一律限制在 assets 目錄樹底下）：
	//   - dir 為附件庫所選的目的地（assets 或其子資料夾）
	//   - 未指定時（例如拖放/貼上）預設存到根 assets
	targetDir := "assets"
	if v, ok := c.GetPostForm("dir"); ok {
		if t := strings.Trim(strings.ReplaceAll(v, "\\", "/"), "/"); t != "" {
			targetDir = t
		}
	}
	// 強制附件只能存放在 assets 目錄底下
	if targetDir != "assets" && !strings.HasPrefix(targetDir, "assets/") {
		c.JSON(http.StatusForbidden, gin.H{"error": "附件只能存放在 assets 目錄底下"})
		return
	}

	// 產生不重複的檔名（時間戳記 + 原始檔名），filepath.Base 可去除任何路徑成分
	origName := filepath.Base(fileHeader.Filename)
	unique := fmt.Sprintf("%d_%s", time.Now().UnixNano(), origName)

	// storeRel 為「相對於 DOC_ROOT」的存放路徑
	storeRel := targetDir + "/" + unique

	absTarget, err := safeResolve(storeRel)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "非法的儲存路徑"})
		return
	}

	// 確保 assets 目錄存在
	if err := os.MkdirAll(filepath.Dir(absTarget), 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "建立目錄失敗：" + err.Error()})
		return
	}

	// 儲存上傳的檔案
	if err := c.SaveUploadedFile(fileHeader, absTarget); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "儲存檔案失敗：" + err.Error()})
		return
	}

	// 回傳「相對於 DOC_ROOT」的路徑，由前端換算成相對於目前文件的連結
	c.JSON(http.StatusOK, gin.H{
		"path":    filepath.ToSlash(storeRel),
		"name":    origName,
		"isImage": isImageExt(ext),
	})
}
