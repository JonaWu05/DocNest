// DocNest — 協作式 Markdown 編輯器。
// 本檔僅負責「組裝」：載入設定、建立各服務（依賴注入）、設定 gin 與路由、啟動服務。
// 功能實作分散於 internal/ 各套件：
//   - internal/config ：設定載入
//   - internal/store  ：路徑安全檢查、副檔名白名單、檔案樹
//   - internal/authz  ：權限分組判斷
//   - internal/auth   ：JWT、登入限流、Local/Discord 登入、/api/me
//   - internal/hub    ：WebSocket（presence / 即時同步）
//   - internal/files  ：檔案 CRUD 與原始檔案服務
//   - internal/upload ：附件上傳與清單
package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-contrib/gzip"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"

	"markdownEditor/internal/auth"
	"markdownEditor/internal/authz"
	"markdownEditor/internal/collab"
	"markdownEditor/internal/config"
	"markdownEditor/internal/files"
	"markdownEditor/internal/hub"
	"markdownEditor/internal/store"
	"markdownEditor/internal/upload"
)

// tokenQueryRE 比對 URL query 中的 token 參數值（供存取紀錄遮罩用）。
// /ws 與 /api/raw 以 ?token=<JWT> 夾帶權杖，不遮罩會把 JWT 寫進 access log。
var tokenQueryRE = regexp.MustCompile(`([?&]token=)[^&]+`)

// redactToken 將路徑中的 token 值換成 REDACTED，避免 JWT 落入日誌。
func redactToken(path string) string {
	return tokenQueryRE.ReplaceAllString(path, "${1}REDACTED")
}

// accessLogger 為以 slog 輸出的存取紀錄中介層（取代 gin 內建文字格式），並遮罩 query 中的 token。
func accessLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		// 健康檢查探活頻繁，不記錄以免灌爆日誌。
		if c.Request.URL.Path == "/healthz" {
			return
		}
		path := c.Request.URL.Path
		if raw := c.Request.URL.RawQuery; raw != "" {
			path += "?" + raw
		}
		slog.Info("request",
			"status", c.Writer.Status(),
			"method", c.Request.Method,
			"path", redactToken(path),
			"ip", c.ClientIP(),
			"latency", time.Since(start).String(),
		)
	}
}

func main() {
	// 結構化日誌：以 slog 文字格式輸出到 stderr（是否落檔由部署環境決定，程式不自管 log 檔）。
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// 載入 .env（不存在則略過，改用系統環境變數）
	_ = godotenv.Load()

	// ===== 載入設定並建立各服務（依賴注入）=====
	cfg := config.Load()

	az, err := authz.Load(cfg.PermissionsFile)
	if err != nil {
		panic("載入權限設定檔失敗：" + err.Error())
	}

	st := store.New(cfg.DocRoot)
	au := auth.New(cfg, az)
	h := hub.New(au, az, cfg)
	go h.Run()

	collabH := collab.New(au, az, cfg) // 即時共編房間層（/ws/collab）

	fileH := files.New(st, az, h, cfg.FsyncOnSave)
	fileH.StartTrashCleaner(cfg.TrashRetentionDays) // 背景定期清除過期的回收筒項目
	uploadH := upload.New(st, az)

	// ===== gin 路由器 =====
	r := gin.New()
	r.Use(accessLogger(), gin.Recovery())

	// 信任的反向代理來源：預設不信任任何代理標頭（避免 X-Forwarded-For 被偽造、繞過登入限流）。
	if err := r.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		panic("設定 trusted proxies 失敗：" + err.Error())
	}

	r.MaxMultipartMemory = 8 << 20

	// CORS：有設定 ALLOWED_ORIGINS 就只允許清單內的來源，否則維持開發模式（全部允許）
	corsConfig := cors.DefaultConfig()
	corsConfig.AllowMethods = []string{"GET", "POST", "DELETE", "OPTIONS"}
	corsConfig.AllowHeaders = []string{"Origin", "Content-Type", "Accept", "Authorization", "X-File-Version"}
	corsConfig.ExposeHeaders = []string{"X-File-Version"}
	if len(cfg.AllowedOrigins) == 0 {
		corsConfig.AllowAllOrigins = true
		slog.Warn("未設定 ALLOWED_ORIGINS：CORS 與 WebSocket 允許所有來源，僅適用於開發環境")
	} else {
		corsConfig.AllowOrigins = cfg.AllowedOrigins
	}
	r.Use(cors.New(corsConfig))

	// 回應壓縮：壓 HTML / JS / CSS / JSON，大幅減少傳輸量（vendor JS 如 highlight.js、EasyMDE 特別有感）。
	//   - 排除 /ws：WebSocket 升級不可被壓縮包裝。
	//   - 排除 /api/raw：服務圖片 / PDF / 附件等二進位（多已壓縮），且需支援 Range 請求。
	//   - 排除已壓縮的副檔名：圖片、字型、壓縮檔。
	r.Use(gzip.Gzip(gzip.DefaultCompression,
		gzip.WithExcludedPaths([]string{"/ws", "/api/raw"}),
		gzip.WithExcludedExtensions([]string{
			".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp",
			".woff", ".woff2", ".ttf", ".eot", ".otf",
			".zip", ".gz", ".pdf",
		}),
	))

	// 靜態資源快取策略：
	//   1. /static/vendor/*（pinned 版本的第三方函式庫）：長快取 + immutable。
	//      ⚠ 升級 vendor 時務必改檔名或在引用網址加版本 query，否則 immutable 會供應舊檔。
	//   2. 自家 HTML/JS/CSS：no-cache（瀏覽器仍可快取，但每次使用前都會向伺服器驗證）。
	r.Use(func(c *gin.Context) {
		p := c.Request.URL.Path
		switch {
		case strings.HasPrefix(p, "/static/vendor/"):
			c.Header("Cache-Control", "public, max-age=31536000, immutable")
		case p == "/" || p == "/index.html" || strings.HasPrefix(p, "/static/"):
			c.Header("Cache-Control", "no-cache")
		}
		c.Next()
	})

	// ===== 公開路由（無需登入）=====
	// 健康檢查：供反向代理 / 容器 / k8s 探活（liveness）。僅回報行程存活，不檢查相依資源。
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	r.LoadHTMLFiles("./web/index.html")
	// 預先組好登入背景的 CSS 覆寫規則。LOGIN_BG 由營運者經環境變數設定（可信來源），
	// 故以 template.CSS 型別注入；否則 html/template 會把 url() 值濾成 ZgotmplZ。
	var loginBgStyle template.CSS
	if cfg.LoginBg != "" {
		loginBgStyle = template.CSS(fmt.Sprintf(
			`#login-view{background:url(%q) center / cover no-repeat !important;}`, cfg.LoginBg))
	}
	indexHandler := func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.html", gin.H{"Title": cfg.AppTitle, "LoginBg": loginBgStyle})
	}
	r.GET("/", indexHandler)
	r.GET("/index.html", indexHandler)
	r.Static("/static", "./web")

	r.POST("/api/login", au.LoginHandler)
	r.GET("/auth/discord", au.DiscordAuthHandler)
	r.GET("/auth/discord/callback", au.DiscordCallbackHandler)

	// WebSocket：自行用 query 參數 token 驗證後升級（瀏覽器無法為 WS 帶 Authorization 標頭）
	r.GET("/ws", h.ServeWs)
	r.GET("/ws/collab", collabH.ServeWs) // 即時共編（Yjs update / awareness 中繼）

	// ===== 受保護路由（需 JWT）=====
	api := r.Group("/api")
	api.Use(au.Middleware())
	{
		api.GET("/me", au.MeHandler)
		api.GET("/online-count", h.OnlineCountHandler)
		api.GET("/files", fileH.ListFiles)
		api.GET("/file", fileH.ReadFile)
		api.POST("/file", fileH.WriteFile)
		api.DELETE("/file", fileH.DeleteFile)
		api.POST("/create", fileH.Create)
		api.POST("/rename", fileH.Rename)
		// 資源回收筒：列出 / 還原 / 永久刪除
		api.GET("/trash", fileH.ListTrash)
		api.POST("/trash/restore", fileH.RestoreTrash)
		api.DELETE("/trash", fileH.PurgeTrash)
		api.GET("/raw", fileH.Raw)
		api.POST("/upload", uploadH.UploadFile)
		api.GET("/assets", uploadH.ListAssets)
		api.GET("/asset-folders", uploadH.ListAssetFolders)
	}

	// ===== 啟動服務並支援優雅關閉 =====
	// 收到 SIGINT/SIGTERM 時停止接收新連線，給既有請求一段時間收尾，再結束程序。
	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: r,
		// ReadHeaderTimeout 限制讀取請求標頭的時間，是 slowloris（慢速送標頭佔住連線）的主要防線。
		ReadHeaderTimeout: 10 * time.Second,
		// IdleTimeout 回收長時間閒置的 keep-alive 連線。
		IdleTimeout: 120 * time.Second,
		// 刻意不設 ReadTimeout / WriteTimeout：附件上傳/下載可達 20 MB，慢速連線需較長時間；
		// 且 /ws 升級後為長連線。整體請求逾時若需要，應由前方反向代理依路由分別設定。
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			panic("伺服器啟動失敗：" + err.Error())
		}
	}()
	slog.Info("伺服器啟動", "port", cfg.Port)

	<-ctx.Done()
	stop() // 還原預設訊號處理，讓再按一次 Ctrl-C 可強制結束
	slog.Info("收到結束訊號，停止接收新連線並等待既有請求收尾")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// 先停 HTTP（不再接收新請求，也就不會再產生 file_updated 廣播）；
	// 被劫持的 WebSocket 連線不受 http.Server.Shutdown 管轄，故隨後由 Hub.Close 主動收尾。
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP 關閉逾時，強制結束", "err", err)
	}
	h.Close()
	slog.Info("已關閉所有連線，正常結束")
}
