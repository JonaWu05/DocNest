package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// ===== 登入速率限制（per-IP，防暴力破解）=====
// 在 loginWindow 內累積 loginMaxFailures 次「失敗」即封鎖該 IP，登入成功則清零。
// 只計失敗、不計成功，避免正常使用者被自己的成功登入卡到。
const (
	loginMaxFailures = 10
	loginWindow      = 15 * time.Minute
)

type loginAttempt struct {
	failures int
	resetAt  time.Time // 視窗到期時間，逾時即整筆作廢重來
}

var (
	loginMu       sync.Mutex
	loginAttempts = map[string]*loginAttempt{}
)

// loginBlocked 回報該 IP 是否已達失敗上限（視窗未過期）
func loginBlocked(ip string) bool {
	loginMu.Lock()
	defer loginMu.Unlock()
	a := loginAttempts[ip]
	if a == nil || time.Now().After(a.resetAt) {
		return false
	}
	return a.failures >= loginMaxFailures
}

// recordLoginFailure 記一次失敗（必要時開新視窗），並順手清掉過期紀錄避免 map 無限成長
func recordLoginFailure(ip string) {
	loginMu.Lock()
	defer loginMu.Unlock()
	now := time.Now()
	a := loginAttempts[ip]
	if a == nil || now.After(a.resetAt) {
		a = &loginAttempt{resetAt: now.Add(loginWindow)}
		loginAttempts[ip] = a
	}
	a.failures++
	// 機會式清理：紀錄數偏多時掃掉所有已過期項目
	if len(loginAttempts) > 256 {
		for k, v := range loginAttempts {
			if now.After(v.resetAt) {
				delete(loginAttempts, k)
			}
		}
	}
}

// resetLoginFailures 清除某 IP 的失敗紀錄（登入成功時呼叫）
func resetLoginFailures(ip string) {
	loginMu.Lock()
	defer loginMu.Unlock()
	delete(loginAttempts, ip)
}

// loginHandler 處理 POST /api/login：比對 Local Account（bcrypt），成功則簽發 JWT。
func loginHandler(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "請求格式錯誤"})
		return
	}

	// 速率限制：同一 IP 連續失敗過多即暫時拒絕，拖慢暴力破解
	ip := c.ClientIP()
	if loginBlocked(ip) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "登入嘗試次數過多，請稍後再試"})
		return
	}

	hash, ok := auth.users[req.Username]
	// 帳號不存在或密碼錯誤都回傳相同訊息，避免洩漏帳號是否存在
	if !ok {
		recordLoginFailure(ip)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "帳號或密碼錯誤"})
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		recordLoginFailure(ip)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "帳號或密碼錯誤"})
		return
	}

	token, err := signJWT(req.Username, "local")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "簽發 token 失敗"})
		return
	}

	// 登入成功：清除該 IP 的失敗紀錄
	resetLoginFailures(ip)
	c.JSON(http.StatusOK, gin.H{
		"token":      token,
		"username":   req.Username,
		"login_type": "local",
	})
}

// meHandler 處理 GET /api/me：回傳目前登入者的 username 與 login_type
// （資料由 AuthMiddleware 驗證 token 後存入 gin.Context）。
func meHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"username":    c.GetString("username"),
		"login_type":  c.GetString("login_type"),
		"default_doc": auth.defaultDoc, // 前端據此決定登入後自動開啟哪份文件作為首頁
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

// discordAuthHandler 處理 GET /auth/discord：產生帶 state 的授權 URL，
// 把 state 存進 cookie，並導向 Discord 授權頁。
func discordAuthHandler(c *gin.Context) {
	if auth.discord == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "尚未設定 Discord OAuth"})
		return
	}

	state, err := randomState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "產生 state 失敗"})
		return
	}

	// 將 state 存入 cookie（HttpOnly、限定 callback 路徑、10 分鐘有效），供 callback 比對
	// 注意：本機開發走 http，secure 設為 false；正式環境（https）應改為 true
	c.SetCookie("oauth_state", state, 600, "/auth/discord/callback", "", false, true)

	// AuthCodeURL 會把 state 一併帶入授權 URL
	c.Redirect(http.StatusFound, auth.discord.AuthCodeURL(state))
}

// discordCallbackHandler 處理 GET /auth/discord/callback：
// 驗證 state → 用 code 換 access token → 取使用者資料 → 白名單檢查 → 簽發 JWT 並導回前端。
func discordCallbackHandler(c *gin.Context) {
	if auth.discord == nil {
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
	// state 用過即作廢
	c.SetCookie("oauth_state", "", -1, "/auth/discord/callback", "", false, true)

	code := c.Query("code")
	if code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 code 參數"})
		return
	}

	// 2) 用 code 向 Discord 換取 access token（限時 10 秒，避免卡住）
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	oauthToken, err := auth.discord.Exchange(ctx, code)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "向 Discord 換取 token 失敗：" + err.Error()})
		return
	}

	// 3) 用 access token 呼叫 Discord API 取得使用者資料
	client := auth.discord.Client(ctx, oauthToken)
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
	if !auth.discordAllowed[du.ID] {
		c.JSON(http.StatusForbidden, gin.H{"error": "此 Discord 帳號未被授權使用本系統"})
		return
	}

	// 5) 以 Discord username 簽發 JWT，導回前端並在 URL fragment 帶上 token
	jwtStr, err := signJWT(du.Username, "discord")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "簽發 token 失敗"})
		return
	}
	c.Redirect(http.StatusFound, "/index.html#token="+url.QueryEscape(jwtStr))
}
