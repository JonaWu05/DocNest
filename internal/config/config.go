// Package config 載入並保存應用程式的所有設定（由環境變數讀取一次後注入各服務）。
package config

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// discordEndpoint 為 Discord 的 OAuth2 授權與換 token 端點。
// （golang.org/x/oauth2 未內建 Discord，故手動指定）
var discordEndpoint = oauth2.Endpoint{
	AuthURL:  "https://discord.com/api/oauth2/authorize",
	TokenURL: "https://discord.com/api/oauth2/token",
}

// 認證模式：standalone 為自簽 JWT 與本地帳號（預設）；portal 為接入 UniEntry 登入中心，
// 本服務只驗證不簽發，登入頁與帳號管理都在 Portal。
const (
	AuthModeStandalone = "standalone"
	AuthModePortal     = "portal"
)

// Config 保存全部設定。由 Load() 一次建立並注入各服務，取代原本散落的 package 級全域變數。
type Config struct {
	DocRoot         string   // 文件根目錄（絕對路徑）
	AppTitle        string   // 瀏覽器分頁標題與登入頁大標
	LoginBg         string   // 登入頁自訂背景圖（CSS url 或 /static 路徑）
	Host            string   // 服務綁定位址（空＝所有介面 0.0.0.0；可設 127.0.0.1 僅本機、或指定本機 IP）
	Port            string   // 服務埠號
	PermissionsFile string   // 權限設定檔路徑
	AccountsFile    string   // 帳號設定檔路徑（本地帳號 + Discord 白名單，可熱重載）
	TrustedProxies  []string // 信任的反向代理（IP/CIDR）
	AllowedOrigins  []string // CORS / WebSocket 允許來源；空＝開發模式全放行

	JWTSecret  []byte         // 簽發 / 驗證 JWT 用的密鑰（portal 模式須與 Portal 同一組，程式無法代為驗證）
	JWTExpire  time.Duration  // JWT 有效期間
	Discord    *oauth2.Config // Discord OAuth 設定（nil 代表停用）
	DefaultDoc string         // 登入後自動開啟的首頁文件（相對 DOC_ROOT）

	AuthMode  string // 認證模式：AuthModeStandalone / AuthModePortal
	PortalURL string // portal 模式的 UniEntry 網址（無尾斜線）；standalone 模式為空

	// FsyncOnSave：存檔時是否 fsync 強制刷盤。預設關（多數檔案型應用的做法）。
	// 關閉不影響原子性（仍靠 temp+rename，讀者不會讀到半檔），僅放棄「斷電當下那一存」的耐久保證。
	// 對耐久要求高的部署可設 FSYNC_ON_SAVE=true。
	FsyncOnSave bool

	// CookieSecure：OAuth state 等 cookie 是否帶 Secure 旗標。正式環境（https）應設 true。
	// 反向代理終止 TLS 時 c.Request.TLS 為 nil，無法自動判斷，故以設定驅動。
	CookieSecure bool

	// TrashRetentionDays：資源回收筒項目保留天數，超過則由背景排程自動永久刪除。
	// 預設 15；設為 0（或負數）代表停用自動清除（永久保留，由使用者自行清理）。
	TrashRetentionDays int
}

// Load 從環境變數建立設定。JWT_SECRET 為必填，缺少則直接中止啟動。
// 呼叫端應先載入 .env（godotenv）再呼叫本函式。
func Load() *Config {
	c := &Config{}

	// 文件根目錄：未設定時預設 ./docs；轉絕對路徑作為後續路徑安全檢查的基準，並確保存在。
	docRoot := os.Getenv("DOC_ROOT")
	if docRoot == "" {
		docRoot = "./docs"
	}
	absRoot, err := filepath.Abs(docRoot)
	if err != nil {
		panic("無法解析 DOC_ROOT 路徑：" + err.Error())
	}
	if err := os.MkdirAll(absRoot, 0o755); err != nil {
		panic("無法建立 DOC_ROOT 目錄：" + err.Error())
	}
	c.DocRoot = absRoot

	// JWT
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		panic("未設定 JWT_SECRET，請於 .env 設定（可用 `openssl rand -base64 32` 產生隨機密鑰）")
	}
	c.JWTSecret = []byte(secret)
	hours := 24
	if v := os.Getenv("JWT_EXPIRE_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			hours = n
		}
	}
	c.JWTExpire = time.Duration(hours) * time.Hour

	c.DefaultDoc = strings.TrimSpace(os.Getenv("DEFAULT_DOC"))

	// 認證模式：未設定即 standalone（向後相容）；portal 模式必須同時給 PORTAL_URL
	mode, portalURL, err := parseAuthMode(os.Getenv("AUTH_MODE"), os.Getenv("PORTAL_URL"))
	if err != nil {
		panic(err.Error())
	}
	c.AuthMode = mode
	c.PortalURL = portalURL

	// Discord OAuth（選填）：三個必要欄位都齊全才啟用
	cid := os.Getenv("DISCORD_CLIENT_ID")
	csecret := os.Getenv("DISCORD_CLIENT_SECRET")
	redirect := os.Getenv("DISCORD_REDIRECT_URI")
	if cid != "" && csecret != "" && redirect != "" {
		c.Discord = &oauth2.Config{
			ClientID:     cid,
			ClientSecret: csecret,
			RedirectURL:  redirect,
			Scopes:       []string{"identify"},
			Endpoint:     discordEndpoint,
		}
	}
	// 標題 / 背景 / 埠號 / 權限檔
	c.AppTitle = strings.TrimSpace(os.Getenv("APP_TITLE"))
	if c.AppTitle == "" {
		c.AppTitle = "Markdown 編輯器"
	}
	c.LoginBg = strings.TrimSpace(os.Getenv("LOGIN_BG"))
	// 綁定位址：留空＝綁所有介面（0.0.0.0，可由區網其他裝置連入）；
	// 開發時可設 127.0.0.1 僅本機可連，部署時可指定特定本機 IP。
	c.Host = strings.TrimSpace(os.Getenv("HOST"))
	c.Port = os.Getenv("PORT")
	if c.Port == "" {
		c.Port = "8080"
	}
	c.PermissionsFile = strings.TrimSpace(os.Getenv("PERMISSIONS_FILE"))
	if c.PermissionsFile == "" {
		c.PermissionsFile = "./permissions.json"
	}
	c.AccountsFile = strings.TrimSpace(os.Getenv("ACCOUNTS_FILE"))
	if c.AccountsFile == "" {
		c.AccountsFile = "./accounts.json"
	}

	// 資源回收筒保留天數：預設 15；明確設為 0 可停用自動清除。非法值維持預設。
	c.TrashRetentionDays = 15
	if v := os.Getenv("TRASH_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			c.TrashRetentionDays = n
		}
	}

	// 存檔刷盤：預設關閉，僅在明確設為 true/1 時開啟。
	c.FsyncOnSave = parseBoolEnv("FSYNC_ON_SAVE")
	// Cookie Secure：預設關閉（本機 http 開發），正式 https 部署應設為 true。
	c.CookieSecure = parseBoolEnv("COOKIE_SECURE")

	c.TrustedProxies = parseTrustedProxies(os.Getenv("TRUSTED_PROXIES"))
	for _, o := range strings.Split(os.Getenv("ALLOWED_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.AllowedOrigins = append(c.AllowedOrigins, o)
		}
	}

	return c
}

// OriginAllowed 判斷某 Origin 是否獲准（供 WebSocket CheckOrigin 使用）。
// 未設定 ALLOWED_ORIGINS 時一律放行（開發模式）。
func (c *Config) OriginAllowed(origin string) bool {
	if len(c.AllowedOrigins) == 0 {
		return true
	}
	for _, o := range c.AllowedOrigins {
		if o == origin {
			return true
		}
	}
	return false
}

// IsTrustedProxy reports whether the direct peer IP is covered by
// TRUSTED_PROXIES. It intentionally checks the socket peer, not Gin's ClientIP,
// because ClientIP may already have been replaced by X-Forwarded-For.
func (c *Config) IsTrustedProxy(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, raw := range c.TrustedProxies {
		raw = strings.TrimSpace(raw)
		if trustedIP := net.ParseIP(raw); trustedIP != nil {
			if trustedIP.Equal(ip) {
				return true
			}
			continue
		}
		if _, network, err := net.ParseCIDR(raw); err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

// parseAuthMode 解析 AUTH_MODE 與 PORTAL_URL：
//   - 空值 / standalone → standalone（PORTAL_URL 忽略）
//   - portal → PORTAL_URL 必填，去除尾斜線後回傳
//   - 其他值 → 錯誤（避免打錯字時靜默退回 standalone、放行未預期的存取模型）
func parseAuthMode(mode, portalURL string) (string, string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", AuthModeStandalone:
		return AuthModeStandalone, "", nil
	case AuthModePortal:
		u := strings.TrimRight(strings.TrimSpace(portalURL), "/")
		if u == "" {
			return "", "", errors.New("AUTH_MODE=portal 時必須設定 PORTAL_URL（UniEntry 的網址，如 http://172.24.15.21:8081）")
		}
		return AuthModePortal, u, nil
	default:
		return "", "", errors.New("AUTH_MODE 僅接受 standalone 或 portal，得到：" + mode)
	}
}

// parseBoolEnv 解析布林環境變數：1/true/yes/on（不分大小寫）視為 true，其餘（含未設定）為 false。
func parseBoolEnv(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// parseTrustedProxies 解析 TRUSTED_PROXIES（逗號分隔的 IP 或 CIDR）。
// 留空回傳 nil，等同不信任任何代理。
func parseTrustedProxies(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
