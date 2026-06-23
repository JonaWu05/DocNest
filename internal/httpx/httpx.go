// Package httpx 提供 HTTP handler 的共用回應輔助。
package httpx

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
)

// ServerError 記錄內部錯誤詳情於伺服器端，僅回傳通用訊息給用戶端。
//
// 目的：避免把 err.Error()（檔案系統錯誤常含絕對路徑等內部資訊）洩漏給前端，
// 同時保留可供排錯的伺服器日誌。用 c.FullPath()（路由樣板）而非完整 URL，避免記到 query 中的 token。
func ServerError(c *gin.Context, userMsg string, err error) {
	log.Printf("[錯誤] %s %s: %v", c.Request.Method, c.FullPath(), err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": userMsg})
}
