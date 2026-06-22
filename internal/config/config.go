// Package config 載入並保存應用程式的所有設定（由環境變數讀取一次後注入各服務）。
package config

import (
	"fmt"
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

// Config 保存全部設定。由 Load() 一次建立並注入各服務，取代原本散落的 package 級全域變數。
type Config struct {
	DocRoot         string   // 文件根目錄（絕對路徑）
	AppTitle        string   // 瀏覽器分頁標題與登入頁大標
	LoginBg         string   // 登入頁自訂背景圖（CSS url 或 /static 路徑）
	Port            string   // 服務埠號
	PermissionsFile string   // 權限設定檔路徑
	TrustedProxies  []string // 信任的反向代理（IP/CIDR）
	AllowedOrigins  []string // CORS / WebSocket 允許來源；空＝開發模式全放行

	Users          map[string]string // username -> bcrypt hash
	JWTSecret      []byte            // 簽發 / 驗證 JWT 用的密鑰
	JWTExpire      time.Duration     // JWT 有效期間
	Discord        *oauth2.Config    // Discord OAuth 設定（nil 代表停用）
	DiscordAllowed map[string]bool   // 允許登入的 Discord User ID 白名單
	DefaultDoc     string            // 登入後自動開啟的首頁文件（相對 DOC_ROOT）
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

	// Local accounts
	c.Users = parseUsers(os.Getenv("USERS"))

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
	c.DiscordAllowed = map[string]bool{}
	for _, id := range strings.Split(os.Getenv("DISCORD_ALLOWED_IDS"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			c.DiscordAllowed[id] = true
		}
	}

	// 標題 / 背景 / 埠號 / 權限檔
	c.AppTitle = strings.TrimSpace(os.Getenv("APP_TITLE"))
	if c.AppTitle == "" {
		c.AppTitle = "Markdown 編輯器"
	}
	c.LoginBg = strings.TrimSpace(os.Getenv("LOGIN_BG"))
	c.Port = os.Getenv("PORT")
	if c.Port == "" {
		c.Port = "8080"
	}
	c.PermissionsFile = strings.TrimSpace(os.Getenv("PERMISSIONS_FILE"))
	if c.PermissionsFile == "" {
		c.PermissionsFile = "./permissions.json"
	}

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

// parseUsers 解析 USERS 設定（格式：username:bcryptHash，多組以逗號分隔）。
// 安全性考量：一律要求 bcrypt hash（以 $2 開頭）；填入明文者會被忽略並警告。
func parseUsers(raw string) map[string]string {
	users := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		// bcrypt hash 內含 $ 但不含 :，username 也不含 :，故以「第一個冒號」切割
		i := strings.Index(pair, ":")
		if i < 0 {
			continue
		}
		name := strings.TrimSpace(pair[:i])
		hash := strings.TrimSpace(pair[i+1:])
		if name == "" || hash == "" {
			continue
		}
		if !strings.HasPrefix(hash, "$2") {
			fmt.Fprintf(os.Stderr,
				"[警告] 使用者 %q 的密碼不是 bcrypt hash，已忽略。請用 `go run ./cmd/hashpw '密碼'` 產生 hash 後填入 USERS。\n",
				name)
			continue
		}
		users[name] = hash
	}
	return users
}
