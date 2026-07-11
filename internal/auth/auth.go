// Package auth 處理登入驗證：JWT 簽發/驗證、登入限流、Local/Discord 登入與 /api/me。
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	entryauth "github.com/JonaWu05/UniEntry/entryauth"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/JonaWu05/DocNest/internal/authz"
	"github.com/JonaWu05/DocNest/internal/config"
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
	Username       string              `json:"username"`
	LoginType      string              `json:"login_type"`
	Subject        string              `json:"sub"`
	AppPermissions map[string][]string `json:"app_permissions,omitempty"`
	jwt.RegisteredClaims
}

// Auth 綁定設定、帳號設定與授權判斷，並持有登入限流狀態。取代原本散落的全域。
type Auth struct {
	cfg      *config.Config
	az       *authz.Authz
	accounts *Accounts

	loginMu       sync.Mutex
	loginAttempts map[string]*loginAttempt
}

// New 建立 Auth；需要設定（JWT/Discord）、帳號設定（本地帳號/Discord 白名單，可熱重載）
// 與授權器（供登入後判斷可見性）。
func New(cfg *config.Config, accounts *Accounts, az *authz.Authz) *Auth {
	return &Auth{cfg: cfg, az: az, accounts: accounts, loginAttempts: map[string]*loginAttempt{}}
}

func (a *Auth) IsPortal() bool { return a.cfg.AuthMode == config.AuthModePortal }

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
	if a.cfg.AuthMode == config.AuthModePortal {
		// entryauth 9ecdb5b accepts every HMAC variant although UniEntry signs
		// HS256. Enforce the documented contract at the service boundary too.
		if token, _, err := new(jwt.Parser).ParseUnverified(tokenStr, &entryauth.Claims{}); err != nil || token.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, errors.New("非預期的簽章方法")
		}
		claims, err := entryauth.ParseJWT(a.cfg.JWTSecret, tokenStr)
		if err != nil {
			return nil, err
		}
		return &Claims{
			Username: claims.Username, LoginType: claims.LoginType, Subject: entryauth.SubjectFromClaims(claims),
			AppPermissions:   claims.AppPermissions,
			RegisteredClaims: claims.RegisteredClaims,
		}, nil
	}
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		if t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
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

// TokenSource 標記 token 的取得來源。CSRF 防護需要這個區分：cookie 由瀏覽器自動附帶、
// 可能被跨站請求利用；Bearer 與 query 為呼叫端明確給值，不受 CSRF 影響。
type TokenSource string

const (
	SourceNone   TokenSource = ""
	SourceBearer TokenSource = "bearer"
	SourceQuery  TokenSource = "query"
	SourceCookie TokenSource = "cookie"
)

// ExtractToken 依認證模式從請求取出 JWT 與其來源：
//   - standalone：Authorization: Bearer → ?token=。cookie 不作為 API 授權來源（免 CSRF 前提）。
//   - portal：Authorization: Bearer → auth_token cookie，與 entryauth.ExtractToken 一致。
func (a *Auth) ExtractToken(c *gin.Context) (string, TokenSource) {
	if a.cfg.AuthMode == config.AuthModePortal {
		if t := bearerToken(c); t != "" {
			return t, SourceBearer
		}
		if t, err := c.Cookie(entryauth.CookieName); err == nil && t != "" {
			return t, SourceCookie
		}
		return "", SourceNone
	}
	if t := bearerToken(c); t != "" {
		return t, SourceBearer
	}
	if t := c.Query("token"); t != "" {
		return t, SourceQuery
	}
	return "", SourceNone
}

// bearerToken 取出 Authorization: Bearer 標頭中的 token；無則回空字串。
func bearerToken(c *gin.Context) string {
	const prefix = "Bearer "
	if h := c.GetHeader("Authorization"); strings.HasPrefix(h, prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// SubjectFromClaims 取出穩定身分鍵；舊 token 無 sub 時退而以 login_type:username 推導。
func SubjectFromClaims(claims *Claims) string {
	if claims.Subject != "" {
		return claims.Subject
	}
	return claims.LoginType + ":" + claims.Username
}

// Middleware 為 JWT 驗證中介層：驗證通過時把 username/login_type/subject 與
// token 來源（auth_source，供 RequireWriteHeader 的 CSRF 判斷）存入 context。
func (a *Auth) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenStr, source := a.ExtractToken(c)
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
		c.Set("auth_source", string(source))
		if a.cfg.AuthMode == config.AuthModePortal {
			c.Set("portal_auth", true)
			c.Set("app_permissions", append([]string(nil), claims.AppPermissions[authz.PortalAppID]...))
		}
		c.Next()
	}
}

// RequireWriteHeader 為 portal 模式的 CSRF 防線，需掛在 Middleware 之後（依賴 auth_source）：
// cookie 授權的寫入請求（POST/PUT/PATCH/DELETE）必須帶自訂標頭 X-Requested-With
// （跨站表單與跨站 fetch 無法設自訂標頭），或由瀏覽器標記為同源請求
// （Sec-Fetch-Site: same-origin，涵蓋無法自訂標頭的 sendBeacon）。
// Bearer 與 ?token= 授權的請求不依賴瀏覽器自動附帶的憑證，不受 CSRF 影響，一律放行。
func RequireWriteHeader() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			c.Next()
			return
		}
		if TokenSource(c.GetString("auth_source")) != SourceCookie {
			c.Next()
			return
		}
		if c.GetHeader("X-Requested-With") != "" || c.GetHeader("Sec-Fetch-Site") == "same-origin" {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "缺少 X-Requested-With 標頭（CSRF 防護）"})
	}
}

// AuthenticateWS 驗證 WebSocket 升級請求的身分：
//   - standalone：僅接受 ?token=（瀏覽器無法為 WS 設 Authorization 標頭；cookie 不授權）。
//   - portal：?token= → auth_token cookie（同 host 的升級請求會自動帶 cookie）。
func (a *Auth) AuthenticateWS(c *gin.Context) (*Claims, error) {
	tokenStr := c.Query("token")
	if tokenStr == "" && a.cfg.AuthMode == config.AuthModePortal {
		tokenStr, _ = c.Cookie(entryauth.CookieName)
	}
	if tokenStr == "" {
		return nil, errors.New("缺少 token")
	}
	return a.ParseJWT(tokenStr)
}

// PortalPageGate 為 portal 模式的頁面守門（GET / 等 HTML 頁）：
// 無有效認證 cookie 時 302 至 Portal 登入頁，登入後由 Portal 依 redirect 參數導回本站。
func (a *Auth) PortalPageGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		if tok, err := c.Cookie(entryauth.CookieName); err == nil {
			if _, err := a.ParseJWT(tok); err == nil {
				c.Next()
				return
			}
		}
		c.Redirect(http.StatusFound, a.portalLoginURL(c))
		c.Abort()
	}
}

// portalLoginURL 組出 Portal 登入頁網址；redirect 帶本站根的絕對網址
// （Portal 只接受站內相對路徑或 apps.yaml 已登記的絕對網址）。
// scheme 由請求本身推斷：反向代理終止 TLS 的部署以內網直連為前提，不處理 X-Forwarded-Proto。
func (a *Auth) portalLoginURL(c *gin.Context) string {
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	self := scheme + "://" + c.Request.Host + c.Request.URL.RequestURI()
	return a.cfg.PortalURL + "/login?redirect=" + url.QueryEscape(self)
}

// setAuthCookie 種下頁面守門用的認證 cookie（內容為 JWT）。
// SameSite=Lax：同站導覽與直接開網址/書籤的 top-level GET 會帶上（守門可用），跨站非 GET 不帶（免 CSRF）。
func (a *Auth) setAuthCookie(c *gin.Context, token string) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(entryauth.CookieName, token, int(a.cfg.JWTExpire.Seconds()), "/", "", a.cfg.CookieSecure, true)
}

// clearAuthCookie 清除認證 cookie（登出時呼叫；HttpOnly cookie 前端 JS 無法自行刪除）。
func (a *Auth) clearAuthCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(entryauth.CookieName, "", -1, "/", "", a.cfg.CookieSecure, true)
}

// LogoutHandler 處理 POST /api/logout：清除認證 cookie。前端另需自行清掉 localStorage 的 token。
func (a *Auth) LogoutHandler(c *gin.Context) {
	a.clearAuthCookie(c)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// RequireAdminPage 為「頁面層級」守門中介層（用於 GET /admin 等 HTML 頁）：
// 讀認證 cookie → 驗 JWT → 檢查是否為管理員；任一不符即導回首頁並中止，
// 使非管理員連頁面骨架都拿不到。真正的資料防線仍在各 API endpoint。
func (a *Auth) RequireAdminPage() gin.HandlerFunc {
	return func(c *gin.Context) {
		tok, err := c.Cookie(entryauth.CookieName)
		if err != nil {
			c.Redirect(http.StatusFound, "/")
			c.Abort()
			return
		}
		claims, err := a.ParseJWT(tok)
		if err != nil || !a.az.IsAdmin(SubjectFromClaims(claims)) {
			c.Redirect(http.StatusFound, "/")
			c.Abort()
			return
		}
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
		slog.Warn("登入被限流", "user", req.Username, "ip", ip)
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "登入嘗試次數過多，請稍後再試"})
		return
	}

	hash, ok := a.accounts.Lookup(req.Username)
	// 帳號不存在或密碼錯誤都回傳相同訊息，避免洩漏帳號是否存在
	if !ok {
		a.recordLoginFailure(ip)
		slog.Warn("登入失敗", "user", req.Username, "ip", ip)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "帳號或密碼錯誤"})
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		a.recordLoginFailure(ip)
		slog.Warn("登入失敗", "user", req.Username, "ip", ip)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "帳號或密碼錯誤"})
		return
	}

	token, err := a.SignJWT(req.Username, "local", "local:"+req.Username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "簽發 token 失敗"})
		return
	}

	a.resetLoginFailures(ip)
	slog.Info("登入成功", "user", req.Username, "type", "local", "ip", ip)
	a.setAuthCookie(c, token) // 供 /admin 等頁面層級守門
	c.JSON(http.StatusOK, gin.H{
		"token":      token,
		"username":   req.Username,
		"login_type": "local",
	})
}

// MeHandler 處理 GET /api/me：回傳登入者資訊與權限摘要。
func (a *Auth) MeHandler(c *gin.Context) {
	subject := authz.SubjectOf(c)
	hasAccess := a.az.HasAnyRead(subject)
	canWriteRoot := a.az.Can(subject, "", authz.AccessWrite)
	isAdmin := a.az.IsAdmin(subject)
	if c.GetBool("portal_auth") {
		permissions := authz.PortalPermissionsOf(c)
		hasAccess = authz.CanPortal(permissions, "", authz.AccessRead)
		if !hasAccess {
			for _, permission := range permissions {
				if strings.HasPrefix(permission, authz.PermissionPagesRead+":") || strings.HasPrefix(permission, authz.PermissionPagesWrite+":") {
					hasAccess = true
					break
				}
			}
		}
		canWriteRoot = authz.CanPortal(permissions, "", authz.AccessWrite)
		isAdmin = false // portal mode has no local account/group administration page
	}
	c.JSON(http.StatusOK, gin.H{
		"username":    c.GetString("username"),
		"login_type":  c.GetString("login_type"),
		"default_doc": a.cfg.DefaultDoc,
		"auth_mode":   a.cfg.AuthMode, // 前端據此切換登入/登出/改密碼等 UI（portal 模式帳號管理在 Portal）
		// 權限摘要：前端據此顯示歡迎頁、隱藏無權限的操作（伺服器端仍為真正防線）
		"has_access":     hasAccess,
		"can_write_root": canWriteRoot,
		"is_admin":       isAdmin,
	})
}

// ReloadConfigHandler 處理 POST /api/admin/reload：僅管理員可呼叫，
// 重新載入帳號設定與權限設定（免重啟）。任一檔解析失敗即回 400 並保留原有設定。
func (a *Auth) ReloadConfigHandler(c *gin.Context) {
	if !a.az.IsAdmin(authz.SubjectOf(c)) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "需要管理員權限"})
		return
	}
	if err := a.accounts.Reload(a.cfg.AccountsFile); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "帳號設定重新載入失敗：" + err.Error()})
		return
	}
	if err := a.az.Reload(a.cfg.PermissionsFile); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "權限設定重新載入失敗：" + err.Error()})
		return
	}
	slog.Info("設定已由管理員手動重新載入", "by", c.GetString("username"))
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
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
	if !a.accounts.IsDiscordAllowed(du.ID) {
		slog.Warn("Discord 登入被拒（未授權）", "id", du.ID, "username", du.Username, "ip", c.ClientIP())
		c.JSON(http.StatusForbidden, gin.H{"error": "此 Discord 帳號未被授權使用本系統"})
		return
	}

	// 5) 簽發 JWT：顯示名稱用 username，但授權身分鍵用穩定且唯一的 Discord ID
	jwtStr, err := a.SignJWT(du.Username, "discord", "discord:"+du.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "簽發 token 失敗"})
		return
	}
	// 顯示備註自動補洞（A+B 的 B）：若該 ID 尚無備註，記下其當下 Discord 用戶名供管理面板辨識。
	// 手動備註優先、僅在空時寫一次；失敗不影響登入。
	if err := a.accounts.NoteDiscordLogin(du.ID, du.Username); err != nil {
		slog.Warn("記錄 Discord 顯示名稱失敗", "id", du.ID, "err", err)
	}
	slog.Info("登入成功", "user", du.Username, "type", "discord", "id", du.ID, "ip", c.ClientIP())

	a.setAuthCookie(c, jwtStr) // 供 /admin 等頁面層級守門
	c.Redirect(http.StatusFound, "/index.html#token="+url.QueryEscape(jwtStr))
}
