// Package auth 處理登入驗證：JWT 簽發/驗證、登入限流、Local/Discord 登入與 /api/me。
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"markdownEditor/internal/authz"
	"markdownEditor/internal/config"
)

// ===== 登入速率限制（per-IP，防暴力破解）=====
const (
	loginMaxFailures = 10
	loginWindow      = 15 * time.Minute
)

type loginAttempt struct {
	failures int
	resetAt  time.Time
}

// Claims 為 JWT 的內容：username（顯示名稱）、login_type，加上授權用的穩定身分鍵 Subject。
// Subject（local:<帳號> / discord:<ID>）與顯示用的 Username 分離，因為 Discord 顯示名稱
// 可被使用者更改、也可能與本地帳號撞名，不適合當權限對應的 key。
type Claims struct {
	Username  string `json:"username"`
	LoginType string `json:"login_type"`
	Subject   string `json:"sub"`
	jwt.RegisteredClaims
}

// Auth 綁定設定與授權判斷，並持有登入限流狀態。取代原本散落的全域。
type Auth struct {
	cfg *config.Config
	az  *authz.Authz

	loginMu       sync.Mutex
	loginAttempts map[string]*loginAttempt
}

// New 建立 Auth；需要設定（使用者/JWT/Discord）與授權器（供登入後判斷可見性）。
func New(cfg *config.Config, az *authz.Authz) *Auth {
	return &Auth{cfg: cfg, az: az, loginAttempts: map[string]*loginAttempt{}}
}

// ===== JWT =====

// SignJWT 以指定的顯示名稱、登入方式與穩定身分鍵簽發一組 HS256 JWT。
func (a *Auth) SignJWT(username, loginType, subject string) (string, error) {
	now := time.Now()
	claims := Claims{
		Username:  username,
		LoginType: loginType,
		Subject:   subject,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(a.cfg.JWTExpire)),
			IssuedAt:  jwt.NewNumericDate(now),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(a.cfg.JWTSecret)
}

// ParseJWT 驗證 token 的簽章與有效期，並回傳解析後的 Claims。
func (a *Auth) ParseJWT(tokenStr string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		// 僅接受 HMAC 簽章，防止 alg=none 之類的攻擊
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("非預期的簽章方法")
		}
		return a.cfg.JWTSecret, nil
	})
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, errors.New("token 無效")
	}
	return claims, nil
}

// ExtractToken 從請求取出 JWT：優先讀 Authorization: Bearer，其次讀 query 參數 ?token=。
func (a *Auth) ExtractToken(c *gin.Context) string {
	const prefix = "Bearer "
	if h := c.GetHeader("Authorization"); strings.HasPrefix(h, prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return c.Query("token")
}

// SubjectFromClaims 取出穩定身分鍵；舊 token 無 sub 時退而以 login_type:username 推導。
func SubjectFromClaims(claims *Claims) string {
	if claims.Subject != "" {
		return claims.Subject
	}
	return claims.LoginType + ":" + claims.Username
}

// Middleware 為 JWT 驗證中介層：驗證通過時把 username/login_type/subject 存入 context。
func (a *Auth) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenStr := a.ExtractToken(c)
		if tokenStr == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "缺少認證 token"})
			return
		}
		claims, err := a.ParseJWT(tokenStr)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token 無效或已過期"})
			return
		}
		c.Set("username", claims.Username)
		c.Set("login_type", claims.LoginType)
		c.Set("subject", SubjectFromClaims(claims))
		c.Next()
	}
}

// ===== 登入限流 =====

// loginBlocked 判斷某 IP 是否因連續登入失敗達上限、且仍在封鎖視窗內。
func (a *Auth) loginBlocked(ip string) bool {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	at := a.loginAttempts[ip]
	if at == nil || time.Now().After(at.resetAt) {
		return false
	}
	return at.failures >= loginMaxFailures
}

// recordLoginFailure 累計某 IP 的登入失敗次數（視窗過期則重新計）；紀錄偏多時機會式清掉已過期項目。
func (a *Auth) recordLoginFailure(ip string) {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	now := time.Now()
	at := a.loginAttempts[ip]
	if at == nil || now.After(at.resetAt) {
		at = &loginAttempt{resetAt: now.Add(loginWindow)}
		a.loginAttempts[ip] = at
	}
	at.failures++
	// 機會式清理：紀錄數偏多時掃掉所有已過期項目
	if len(a.loginAttempts) > 256 {
		for k, v := range a.loginAttempts {
			if now.After(v.resetAt) {
				delete(a.loginAttempts, k)
			}
		}
	}
}

// resetLoginFailures 清除某 IP 的登入失敗紀錄（登入成功後呼叫）。
func (a *Auth) resetLoginFailures(ip string) {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	delete(a.loginAttempts, ip)
}

// ===== Handlers =====

// LoginHandler 處理 POST /api/login：比對 Local Account（bcrypt），成功則簽發 JWT。
func (a *Auth) LoginHandler(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "請求格式錯誤"})
		return
	}

	ip := c.ClientIP()
	if a.loginBlocked(ip) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "登入嘗試次數過多，請稍後再試"})
		return
	}

	hash, ok := a.cfg.Users[req.Username]
	// 帳號不存在或密碼錯誤都回傳相同訊息，避免洩漏帳號是否存在
	if !ok {
		a.recordLoginFailure(ip)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "帳號或密碼錯誤"})
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		a.recordLoginFailure(ip)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "帳號或密碼錯誤"})
		return
	}

	token, err := a.SignJWT(req.Username, "local", "local:"+req.Username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "簽發 token 失敗"})
		return
	}

	a.resetLoginFailures(ip)
	c.JSON(http.StatusOK, gin.H{
		"token":      token,
		"username":   req.Username,
		"login_type": "local",
	})
}

// MeHandler 處理 GET /api/me：回傳登入者資訊與權限摘要。
func (a *Auth) MeHandler(c *gin.Context) {
	subject := authz.SubjectOf(c)
	c.JSON(http.StatusOK, gin.H{
		"username":    c.GetString("username"),
		"login_type":  c.GetString("login_type"),
		"default_doc": a.cfg.DefaultDoc,
		// 權限摘要：前端據此顯示歡迎頁、隱藏無權限的操作（伺服器端仍為真正防線）
		"has_access":     a.az.HasAnyRead(subject),
		"can_write_root": a.az.Can(subject, "", authz.AccessWrite),
	})
}

// randomState 產生一段隨機字串，作為 OAuth2 的 state 參數以防 CSRF。
func randomState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DiscordAuthHandler 處理 GET /auth/discord：產生帶 state 的授權 URL 並導向 Discord。
func (a *Auth) DiscordAuthHandler(c *gin.Context) {
	if a.cfg.Discord == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "尚未設定 Discord OAuth"})
		return
	}

	state, err := randomState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "產生 state 失敗"})
		return
	}

	// state 存入 cookie（HttpOnly、限定 callback 路徑、10 分鐘有效），供 callback 比對。
	// Secure 旗標由設定驅動：正式 https 部署應設 COOKIE_SECURE=true。
	c.SetCookie("oauth_state", state, 600, "/auth/discord/callback", "", a.cfg.CookieSecure, true)
	c.Redirect(http.StatusFound, a.cfg.Discord.AuthCodeURL(state))
}

// DiscordCallbackHandler 處理 GET /auth/discord/callback：
// 驗證 state → 用 code 換 token → 取使用者資料 → 白名單檢查 → 簽發 JWT 並導回前端。
func (a *Auth) DiscordCallbackHandler(c *gin.Context) {
	if a.cfg.Discord == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "尚未設定 Discord OAuth"})
		return
	}

	// 1) CSRF 防護：URL 的 state 必須與 cookie 中的 state 一致
	stateParam := c.Query("state")
	stateCookie, err := c.Cookie("oauth_state")
	if err != nil || stateParam == "" || stateParam != stateCookie {
		c.JSON(http.StatusBadRequest, gin.H{"error": "state 驗證失敗（可能為 CSRF 攻擊或授權逾時）"})
		return
	}
	c.SetCookie("oauth_state", "", -1, "/auth/discord/callback", "", a.cfg.CookieSecure, true)

	code := c.Query("code")
	if code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 code 參數"})
		return
	}

	// 2) 用 code 向 Discord 換取 access token（限時 10 秒）
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	oauthToken, err := a.cfg.Discord.Exchange(ctx, code)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "向 Discord 換取 token 失敗：" + err.Error()})
		return
	}

	// 3) 用 access token 取得使用者資料
	client := a.cfg.Discord.Client(ctx, oauthToken)
	resp, err := client.Get("https://discord.com/api/users/@me")
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "呼叫 Discord API 失敗：" + err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Discord API 回應異常（狀態碼 " + resp.Status + "）"})
		return
	}

	var du struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&du); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "解析 Discord 使用者資料失敗"})
		return
	}

	// 4) 白名單檢查：只有名單內的 Discord User ID 可登入
	if !a.cfg.DiscordAllowed[du.ID] {
		c.JSON(http.StatusForbidden, gin.H{"error": "此 Discord 帳號未被授權使用本系統"})
		return
	}

	// 5) 簽發 JWT：顯示名稱用 username，但授權身分鍵用穩定且唯一的 Discord ID
	jwtStr, err := a.SignJWT(du.Username, "discord", "discord:"+du.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "簽發 token 失敗"})
		return
	}
	c.Redirect(http.StatusFound, "/index.html#token="+url.QueryEscape(jwtStr))
}
