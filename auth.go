package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"
)

// authConfig 保存所有與登入驗證相關的設定，於程式啟動時由環境變數載入一次。
type authConfig struct {
	users          map[string]string // username -> bcrypt hash（一律要求 bcrypt，不存明文）
	jwtSecret      []byte            // 簽發 / 驗證 JWT 用的密鑰
	jwtExpire      time.Duration     // JWT 有效期間
	discord        *oauth2.Config    // Discord OAuth 設定（未設定時為 nil，代表停用）
	discordAllowed map[string]bool   // 允許登入的 Discord User ID 白名單
	defaultDoc     string            // 登入後自動開啟的首頁文件（相對 DOC_ROOT），空字串代表不指定
}

// auth 為全域驗證設定（與 docRoot 相同的單例做法）
var auth authConfig

// discordEndpoint 為 Discord 的 OAuth2 授權與換 token 端點。
// （golang.org/x/oauth2 未內建 Discord，故手動指定）
var discordEndpoint = oauth2.Endpoint{
	AuthURL:  "https://discord.com/api/oauth2/authorize",
	TokenURL: "https://discord.com/api/oauth2/token",
}

// Claims 為 JWT 的內容，格式與規格一致：username、login_type，加上標準的 exp/iat。
type Claims struct {
	Username  string `json:"username"`
	LoginType string `json:"login_type"` // "local" 或 "discord"
	jwt.RegisteredClaims
}

// loadAuthConfig 從環境變數載入驗證設定。JWT_SECRET 為必填，缺少則直接中止啟動。
func loadAuthConfig() {
	auth.users = parseUsers(os.Getenv("USERS"))

	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		panic("未設定 JWT_SECRET，請於 .env 設定（可用 `openssl rand -base64 32` 產生隨機密鑰）")
	}
	auth.jwtSecret = []byte(secret)

	// JWT 有效時數（預設 24 小時）
	hours := 24
	if v := os.Getenv("JWT_EXPIRE_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			hours = n
		}
	}
	auth.jwtExpire = time.Duration(hours) * time.Hour

	// 登入後自動開啟的首頁文件（選填）
	auth.defaultDoc = strings.TrimSpace(os.Getenv("DEFAULT_DOC"))

	// Discord OAuth（選填）：三個必要欄位都齊全才啟用
	cid := os.Getenv("DISCORD_CLIENT_ID")
	csecret := os.Getenv("DISCORD_CLIENT_SECRET")
	redirect := os.Getenv("DISCORD_REDIRECT_URI")
	if cid != "" && csecret != "" && redirect != "" {
		auth.discord = &oauth2.Config{
			ClientID:     cid,
			ClientSecret: csecret,
			RedirectURL:  redirect,
			Scopes:       []string{"identify"}, // 僅需取得基本身分資料
			Endpoint:     discordEndpoint,
		}
	}

	// Discord 允許登入的 User ID 白名單
	auth.discordAllowed = map[string]bool{}
	for _, id := range strings.Split(os.Getenv("DISCORD_ALLOWED_IDS"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			auth.discordAllowed[id] = true
		}
	}
}

// parseUsers 解析 USERS 設定（格式：username:bcryptHash，多組以逗號分隔）。
// 安全性考量：一律要求 bcrypt hash（以 $2 開頭）；填入明文者會被忽略並警告，
// 以避免不小心把明文密碼當成可用設定。請用 `go run ./cmd/hashpw '密碼'` 產生 hash。
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

// signJWT 以指定的使用者名稱與登入方式簽發一組 HS256 JWT。
func signJWT(username, loginType string) (string, error) {
	now := time.Now()
	claims := Claims{
		Username:  username,
		LoginType: loginType,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(auth.jwtExpire)),
			IssuedAt:  jwt.NewNumericDate(now),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(auth.jwtSecret)
}

// parseJWT 驗證 token 的簽章與有效期，並回傳解析後的 Claims。
func parseJWT(tokenStr string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		// 僅接受 HMAC 簽章，防止 alg=none 之類的攻擊
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("非預期的簽章方法")
		}
		return auth.jwtSecret, nil
	})
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, errors.New("token 無效")
	}
	return claims, nil
}

// extractToken 從請求取出 JWT：優先讀 Authorization: Bearer <token>，
// 其次讀 query 參數 ?token=（供 <img src="/api/raw?...&token=">　這類無法帶標頭的情境使用）。
func extractToken(c *gin.Context) string {
	const prefix = "Bearer "
	if h := c.GetHeader("Authorization"); strings.HasPrefix(h, prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return c.Query("token")
}

// AuthMiddleware 為可重複使用的 JWT 驗證中介層（第二階段的 /ws 亦可直接套用）。
// 驗證通過時把 username 與 login_type 存入 gin.Context；失敗則回 401 並中止請求。
func AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenStr := extractToken(c)
		if tokenStr == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "缺少認證 token"})
			return
		}
		claims, err := parseJWT(tokenStr)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token 無效或已過期"})
			return
		}
		c.Set("username", claims.Username)
		c.Set("login_type", claims.LoginType)
		c.Next()
	}
}
