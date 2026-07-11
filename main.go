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

	"github.com/JonaWu05/DocNest/internal/auth"
	"github.com/JonaWu05/DocNest/internal/authz"
	"github.com/JonaWu05/DocNest/internal/collab"
	"github.com/JonaWu05/DocNest/internal/config"
	"github.com/JonaWu05/DocNest/internal/files"
	"github.com/JonaWu05/DocNest/internal/filewatch"
	"github.com/JonaWu05/DocNest/internal/hub"
	"github.com/JonaWu05/DocNest/internal/logx"
	"github.com/JonaWu05/DocNest/internal/store"
	"github.com/JonaWu05/DocNest/internal/upload"
)

// tokenQueryRE 比對 URL query 中的 token 參數值（供存取紀錄遮罩用）。
// /ws 與 /api/raw 以 ?token=<JWT> 夾帶權杖，不遮罩會把 JWT 寫進 access log。
var tokenQueryRE = regexp.MustCompile(`([?&]token=)[^&]+`)

// redactToken 將路徑中的 token 值換成 REDACTED，避免 JWT 落入日誌。
func redactToken(path string) string {
	return tokenQueryRE.ReplaceAllString(path, "${1}REDACTED")
}

// accessLogger 為以 slog 輸出的存取紀錄中介層（取代 gin 內建文字格式），並遮罩 query 中的 token。
// 依回應狀態分級以提升可讀性：5xx→Error、4xx→Warn；其餘成功請求中，靜態資源（/static/*）
// 降為 Debug（預設等級 Info 下隱藏，避免一次頁面載入噴出大量 304），API / 認證 / WebSocket
// 等有意義的請求維持 Info。要排查靜態資源時把 log level 調成 Debug 即可全數顯示。
func accessLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		// 健康檢查探活頻繁，不記錄以免灌爆日誌。
		if c.Request.URL.Path == "/healthz" {
			return
		}
		status := c.Writer.Status()
		fullPath := c.Request.URL.Path
		if raw := c.Request.URL.RawQuery; raw != "" {
			fullPath += "?" + raw
		}

		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelWarn
		case strings.HasPrefix(c.Request.URL.Path, "/static/"):
			level = slog.LevelDebug
		}
		slog.Log(c.Request.Context(), level, "request",
			"status", status,
			"method", c.Request.Method,
			"path", redactToken(fullPath),
			"ip", c.ClientIP(),
			"latency", time.Since(start).String(),
		)
	}
}

func main() {
	// 結構化日誌：slog 文字格式輸出到 stderr（是否落檔由部署環境決定，程式不自管 log 檔）。
	// 終端輸出時把 level 欄位上色以利閱讀；非終端 / NO_COLOR 時自動退回原生無色格式。
	slog.SetDefault(slog.New(logx.New(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// 載入 .env（不存在則略過，改用系統環境變數）
	_ = godotenv.Load()

	// ===== 載入設定並建立各服務（依賴注入）=====
	cfg := config.Load()

	var az *authz.Authz
	var accounts *auth.Accounts
	if cfg.AuthMode == config.AuthModePortal {
		az = authz.NewPortalFallback()
		accounts = auth.NewEmptyAccounts()
	} else {
		var err error
		az, err = authz.Load(cfg.PermissionsFile)
		if err != nil {
			panic("載入權限設定檔失敗：" + err.Error())
		}
		accounts, err = auth.LoadAccounts(cfg.AccountsFile)
		if err != nil {
			panic("載入帳號設定檔失敗：" + err.Error())
		}
	}

	st := store.New(cfg.DocRoot)
	au := auth.New(cfg, accounts, az)
	h := hub.New(au, az, cfg)
	go h.Run()

	collabH := collab.New(au, az, cfg) // 即時共編房間層（/ws/collab）

	// 外部改檔偵測：只輪詢「目前有人開著」的檔（presence 開檔 ∪ 共編房間），
	// 偵測到非經本程式的改寫時，通知非共編開檔者（file_updated）與共編房間（停自動落檔 + 橫幅）。
	watcher := filewatch.New(3*time.Second,
		func() []string { // 要輪詢的檔：合併 presence 開檔與共編房間路徑（去重交由 filewatch 的 known map）
			return append(h.OpenPaths(), collabH.RoomPaths()...)
		},
		func(rel string) (string, bool) { // 取得檔案目前版本（size+mtime）
			abs, err := st.SafeResolve(rel)
			if err != nil {
				return "", false
			}
			info, err := os.Stat(abs)
			if err != nil {
				return "", false
			}
			return store.FileVersion(info), true
		},
		func(rel string) { // 外部改檔：兩路通知
			collabH.NotifyExternalChange(rel) // 共編房間：橫幅 + 停 saver 自動落檔
			h.BroadcastFileUpdated(rel, "")   // 非共編開檔者：file_updated（savedBy="" 表外部變更）
		},
	)
	go watcher.Run()

	fileH := files.New(st, az, h, watcher, cfg.FsyncOnSave)
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
	corsConfig.AllowHeaders = []string{"Origin", "Content-Type", "Accept", "Authorization", "X-File-Version", "X-Requested-With"}
	corsConfig.ExposeHeaders = []string{"X-File-Version"}
	if len(cfg.AllowedOrigins) == 0 {
		corsConfig.AllowAllOrigins = true
		slog.Warn("未設定 ALLOWED_ORIGINS：CORS 與 WebSocket 允許所有來源，僅適用於開發環境")
	} else {
		corsConfig.AllowOrigins = cfg.AllowedOrigins
		if cfg.AuthMode == config.AuthModePortal {
			corsConfig.AllowCredentials = true
		}
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
		c.HTML(http.StatusOK, "index.html", gin.H{
			"Title":     cfg.AppTitle,
			"LoginBg":   loginBgStyle,
			"AuthMode":  cfg.AuthMode,  // 前端據此切換 standalone / portal 行為
			"PortalURL": cfg.PortalURL, // portal 模式的登入導向目標
		})
	}
	if cfg.AuthMode == config.AuthModePortal {
		// portal 模式：未帶有效認證 cookie 的頁面請求 302 至 Portal 登入頁
		r.GET("/", au.PortalPageGate(), indexHandler)
		r.GET("/index.html", au.PortalPageGate(), indexHandler)
	} else {
		r.GET("/", indexHandler)
		r.GET("/index.html", indexHandler)
	}
	r.Static("/static", "./web")

	r.POST("/api/logout", au.LogoutHandler) // 清除認證 cookie（公開：過期 session 也能清；portal 模式登出亦用此清 cookie）
	if cfg.AuthMode == config.AuthModeStandalone {
		// 簽發相關端點僅 standalone 提供；portal 模式登入走 Portal，這些路由不註冊（404）
		r.POST("/api/login", au.LoginHandler)
		r.GET("/auth/discord", au.DiscordAuthHandler)
		r.GET("/auth/discord/callback", au.DiscordCallbackHandler)
	}

	if cfg.AuthMode == config.AuthModeStandalone {
		// 本地帳號與群組管理只屬於 standalone；portal 一律回 UniEntry 管理。
		r.GET("/admin", au.RequireAdminPage(), func(c *gin.Context) { c.File("./web/admin.html") })
	}

	// WebSocket：自行用 query 參數 token 驗證後升級（瀏覽器無法為 WS 帶 Authorization 標頭）
	r.GET("/ws", h.ServeWs)
	r.GET("/ws/collab", collabH.ServeWs) // 即時共編（Yjs update / awareness 中繼）

	// ===== 受保護路由（需 JWT）=====
	api := r.Group("/api")
	api.Use(au.Middleware())
	if cfg.AuthMode == config.AuthModePortal {
		// portal 模式 cookie 會授權寫入，需擋跨站偽造請求（standalone 的 cookie 不授權，毋須此層）
		api.Use(auth.RequireWriteHeader())
	}
	{
		api.GET("/me", au.MeHandler)
		if cfg.AuthMode == config.AuthModeStandalone {
			// 帳號管理僅 standalone 提供；portal 模式帳號在 Portal 管理，這些路由不註冊（404）
			api.POST("/me/password", au.ChangeOwnPasswordHandler)       // 使用者自助改密碼（僅本地帳號）
			api.POST("/admin/user/create", au.CreateUserHandler)        // 新增本地帳號
			api.POST("/admin/user/password", au.SetUserPasswordHandler) // 重設帳號密碼
			api.POST("/admin/user/delete", au.DeleteUserHandler)        // 刪除本地帳號
			api.POST("/admin/discord/add", au.AddDiscordHandler)        // Discord 白名單新增
			api.POST("/admin/discord/label", au.SetDiscordLabelHandler) // Discord 顯示備註更新
			api.POST("/admin/discord/remove", au.RemoveDiscordHandler)  // Discord 白名單移除
			api.POST("/admin/reload", au.ReloadConfigHandler)
			api.GET("/admin/overview", au.OverviewHandler)
			api.POST("/admin/group/member", au.SetGroupMemberHandler)
		}
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
		api.POST("/asset/rename", uploadH.RenameAsset)
	}

	// ===== 啟動服務並支援優雅關閉 =====
	// 收到 SIGINT/SIGTERM 時停止接收新連線，給既有請求一段時間收尾，再結束程序。
	srv := &http.Server{
		// 綁定位址：cfg.Host 為空時為 ":port"（綁所有介面）；指定 HOST 則只綁該位址（如 127.0.0.1 僅本機）。
		Addr:    cfg.Host + ":" + cfg.Port,
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
	slog.Info("伺服器啟動", "addr", srv.Addr)

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
	watcher.Stop()
	h.Close()
	slog.Info("已關閉所有連線，正常結束")
}
