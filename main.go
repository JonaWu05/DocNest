package main

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

// tokenQueryRE 比對 URL query 中的 token 參數值（供存取紀錄遮罩用）。
// /ws 與 /api/raw 以 ?token=<JWT> 夾帶權杖，不遮罩會把 JWT 寫進 access log。
var tokenQueryRE = regexp.MustCompile(`([?&]token=)[^&]+`)

// redactToken 將路徑中的 token 值換成 REDACTED，避免 JWT 落入日誌
func redactToken(path string) string {
	return tokenQueryRE.ReplaceAllString(path, "${1}REDACTED")
}

// accessLogFormatter 為自訂的存取紀錄格式，與 gin 預設相近但會遮罩 token
func accessLogFormatter(p gin.LogFormatterParams) string {
	return fmt.Sprintf("[GIN] %s | %3d | %13v | %15s | %-7s %s\n",
		p.TimeStamp.Format("2006/01/02 - 15:04:05"),
		p.StatusCode, p.Latency, p.ClientIP, p.Method,
		redactToken(p.Path),
	)
}

// allowedOrigins 由 ALLOWED_ORIGINS 環境變數解析（逗號分隔）。
// 留空代表「開發模式」：CORS 與 WebSocket 皆允許所有來源。
var allowedOrigins []string

// parseTrustedProxies 解析 TRUSTED_PROXIES（逗號分隔的 IP 或 CIDR）。
// 留空回傳 nil，等同不信任任何代理（與原本 SetTrustedProxies(nil) 行為一致）。
func parseTrustedProxies(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadAllowedOrigins 解析 ALLOWED_ORIGINS（如 "https://a.com,https://b.com"）
func loadAllowedOrigins() {
	for _, o := range strings.Split(os.Getenv("ALLOWED_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			allowedOrigins = append(allowedOrigins, o)
		}
	}
}

// isOriginAllowed 判斷某 Origin 是否獲准（供 WebSocket CheckOrigin 使用）。
// 未設定 ALLOWED_ORIGINS 時一律放行（開發模式）。
func isOriginAllowed(origin string) bool {
	if len(allowedOrigins) == 0 {
		return true
	}
	for _, o := range allowedOrigins {
		if o == origin {
			return true
		}
	}
	return false
}

// docRoot 為文件根目錄的「絕對路徑」，所有檔案存取都必須限制在此目錄底下。
// 其餘檔案處理邏輯依功能拆分於：
//   - fs.go     ：路徑安全檢查、副檔名白名單、檔案樹建立
//   - files.go  ：檔案 CRUD 與原始檔案服務
//   - upload.go ：附件上傳
//   - assets.go ：附件清單與資料夾列舉
var docRoot string

// appTitle 為瀏覽器分頁標題與登入頁大標，可由環境變數 APP_TITLE 覆寫（供自架者改品牌名）。
var appTitle string

// loginBg 為登入頁的自訂背景圖（環境變數 LOGIN_BG），留空則用 CSS 預設的暗色漸層。
var loginBg string

func main() {
	// 載入 .env 設定檔（若不存在則略過，改用系統環境變數）
	_ = godotenv.Load()

	// 讀取文件根目錄設定
	docRoot = os.Getenv("DOC_ROOT")
	if docRoot == "" {
		// 未設定時，預設使用程式所在目錄下的 docs 資料夾
		docRoot = "./docs"
	}

	// 轉成絕對路徑，後續路徑安全檢查都以此為基準
	absRoot, err := filepath.Abs(docRoot)
	if err != nil {
		panic("無法解析 DOC_ROOT 路徑：" + err.Error())
	}
	docRoot = absRoot

	// 若根目錄不存在則自動建立，避免啟動後找不到目錄
	if err := os.MkdirAll(docRoot, 0o755); err != nil {
		panic("無法建立 DOC_ROOT 目錄：" + err.Error())
	}

	// 載入登入驗證設定（USERS / JWT / Discord OAuth）；缺少 JWT_SECRET 會直接中止
	loadAuthConfig()

	// 解析允許的跨來源網域（CORS 與 WebSocket 共用）
	loadAllowedOrigins()

	// 網頁標題（瀏覽器分頁與登入頁大標），未設定時用預設名稱
	appTitle = strings.TrimSpace(os.Getenv("APP_TITLE"))
	if appTitle == "" {
		appTitle = "Markdown 編輯器"
	}

	// 自訂登入背景圖（選填）：設定後覆蓋預設的暗色漸層，可為 /static/... 路徑或外部 URL
	loginBg = strings.TrimSpace(os.Getenv("LOGIN_BG"))

	// 建立並啟動 WebSocket Hub（單一 goroutine 管理所有連線）
	hub = newHub()
	go hub.run()

	// 讀取服務埠號設定
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// 建立 gin 路由器：自帶 Recovery，並用會遮罩 token 的自訂 access logger（取代 gin.Default）
	r := gin.New()
	r.Use(gin.LoggerWithFormatter(accessLogFormatter), gin.Recovery())

	// 信任的反向代理來源：預設不信任任何代理標頭（避免 X-Forwarded-For 被偽造、繞過登入限流）。
	// 部署在反向代理後方時，用 TRUSTED_PROXIES 指定代理主機（逗號分隔，支援 IP 或 CIDR），
	// gin 才會讀 X-Forwarded-For 還原真實客戶端 IP。例：TRUSTED_PROXIES=172.24.15.23
	if err := r.SetTrustedProxies(parseTrustedProxies(os.Getenv("TRUSTED_PROXIES"))); err != nil {
		panic("設定 trusted proxies 失敗：" + err.Error())
	}

	// 設定 multipart 表單在記憶體中暫存的上限（超過則寫入暫存檔）
	r.MaxMultipartMemory = 8 << 20

	// 設定 CORS：有設定 ALLOWED_ORIGINS 就只允許清單內的來源，否則維持開發模式（全部允許）
	corsConfig := cors.DefaultConfig()
	corsConfig.AllowMethods = []string{"GET", "POST", "DELETE", "OPTIONS"}                                    // 允許的 HTTP 方法
	corsConfig.AllowHeaders = []string{"Origin", "Content-Type", "Accept", "Authorization", "X-File-Version"} // 允許的標頭（含 JWT 與樂觀鎖版本）
	corsConfig.ExposeHeaders = []string{"X-File-Version"}                                                     // 讓前端 JS 能讀到回應的版本標頭
	if len(allowedOrigins) == 0 {
		corsConfig.AllowAllOrigins = true
		log.Println("[警告] 未設定 ALLOWED_ORIGINS：CORS 與 WebSocket 允許所有來源，僅適用於開發環境")
	} else {
		corsConfig.AllowOrigins = allowedOrigins
	}
	r.Use(cors.New(corsConfig))

	// 靜態資源帶 Cache-Control: no-cache：瀏覽器仍可快取，但每次使用前都會向伺服器
	// 驗證（未變更回 304，很快），改版後不會再沿用舊的 JS/CSS 模組而需手動強制重整。
	r.Use(func(c *gin.Context) {
		p := c.Request.URL.Path
		if p == "/" || p == "/index.html" || strings.HasPrefix(p, "/static/") {
			c.Header("Cache-Control", "no-cache")
		}
		c.Next()
	})

	// ===== 公開路由（無需登入）=====
	// 首頁以模板渲染，注入可由 APP_TITLE 覆寫的標題（其餘為靜態內容）
	r.LoadHTMLFiles("./web/index.html")
	// 預先組好登入背景的 CSS 覆寫規則。LOGIN_BG 由營運者經環境變數設定（可信來源），
	// 故以 template.CSS 型別注入；否則 html/template 會把 url() 值當不可信內容濾成 ZgotmplZ。
	var loginBgStyle template.CSS
	if loginBg != "" {
		loginBgStyle = template.CSS(fmt.Sprintf(
			`#login-view{background:url(%q) center / cover no-repeat !important;}`, loginBg))
	}
	indexHandler := func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.html", gin.H{"Title": appTitle, "LoginBg": loginBgStyle})
	}
	r.GET("/", indexHandler)
	r.GET("/index.html", indexHandler)
	r.Static("/static", "./web") // styles.css 與 js/ 模組（index.html 以 /static/... 引用）

	// 登入相關端點
	r.POST("/api/login", loginHandler)                      // Local Account 登入
	r.GET("/auth/discord", discordAuthHandler)              // 導向 Discord 授權頁
	r.GET("/auth/discord/callback", discordCallbackHandler) // Discord 授權回呼

	// WebSocket 端點：自行用 query 參數 token 驗證後升級（瀏覽器無法為 WS 帶 Authorization 標頭）
	r.GET("/ws", serveWs)

	// ===== 受保護路由（需 JWT，套用可重複使用的 AuthMiddleware）=====
	api := r.Group("/api")
	api.Use(AuthMiddleware())
	{
		api.GET("/me", meHandler)                          // 取得目前登入者資訊
		api.GET("/online-count", onlineCountHandler)       // 目前 WebSocket 線上人數
		api.GET("/files", listFilesHandler)                // 列出所有 .md / .txt 檔案的樹狀結構
		api.GET("/file", readFileHandler)                  // 讀取單一檔案內容
		api.POST("/file", writeFileHandler)                // 寫入單一檔案內容
		api.DELETE("/file", deleteFileHandler)             // 刪除檔案或資料夾
		api.POST("/create", createHandler)                 // 新增檔案或資料夾
		api.POST("/rename", renameHandler)                 // 重新命名 / 移動檔案或資料夾
		api.GET("/raw", rawFileHandler)                    // 提供原始檔案（圖片/附件）服務
		api.POST("/upload", uploadHandler)                 // 上傳圖片或附件
		api.GET("/assets", listAssetsHandler)              // 列出 assets 樹底下所有已上傳的附件
		api.GET("/asset-folders", listAssetFoldersHandler) // 列出 assets 樹底下的資料夾（上傳目的地用）
	}

	// 啟動服務
	if err := r.Run(":" + port); err != nil {
		panic("伺服器啟動失敗：" + err.Error())
	}
}
