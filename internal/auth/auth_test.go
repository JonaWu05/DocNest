package auth

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	entryauth "github.com/JonaWu05/UniEntry/entryauth"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/JonaWu05/DocNest/internal/authz"
	"github.com/JonaWu05/DocNest/internal/config"
)

func init() { gin.SetMode(gin.TestMode) }

func newTestAuth(t *testing.T) *Auth {
	t.Helper()
	cfg := &config.Config{JWTSecret: []byte("test-secret"), JWTExpire: time.Hour}
	az, err := authz.Load(filepath.Join(t.TempDir(), "none.json")) // 停用模式即可
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := LoadAccounts(filepath.Join(t.TempDir(), "none-accounts.json")) // 空帳號即可
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, accounts, az)
}

func redirectTarget(t *testing.T, loginURL string) string {
	t.Helper()
	u, err := url.Parse(loginURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("redirect")
}

func newPortalTestAuth(t *testing.T) *Auth {
	t.Helper()
	cfg := &config.Config{JWTSecret: []byte("test-secret"), JWTExpire: time.Hour, AuthMode: config.AuthModePortal, PortalURL: "http://portal:8081"}
	return New(cfg, NewEmptyAccounts(), authz.NewPortalFallback())
}

func signPortalToken(t *testing.T, permissions []string) string {
	t.Helper()
	claims := entryauth.NewClaimsWithAuthz("Alice", "local", "local:alice", nil,
		map[string][]string{authz.PortalAppID: {"editor"}},
		map[string][]string{authz.PortalAppID: permissions}, time.Hour)
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestJWTRoundTrip(t *testing.T) {
	a := newTestAuth(t)
	tok, err := a.SignJWT("alice", "local", "local:alice")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := a.ParseJWT(tok)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Username != "alice" || claims.LoginType != "local" || claims.Subject != "local:alice" {
		t.Errorf("claims 不正確：%+v", claims)
	}
}

func TestParseJWTRejectsBadToken(t *testing.T) {
	a := newTestAuth(t)
	tok, _ := a.SignJWT("alice", "local", "local:alice")

	if _, err := a.ParseJWT(tok + "tampered"); err == nil {
		t.Error("被竄改的 token 應驗證失敗")
	}
	// 換一把密鑰驗證同一個 token → 應失敗
	other := New(&config.Config{JWTSecret: []byte("different-secret"), JWTExpire: time.Hour}, a.accounts, a.az)
	if _, err := other.ParseJWT(tok); err == nil {
		t.Error("錯誤密鑰應驗證失敗")
	}
}

func TestSubjectFromClaims(t *testing.T) {
	if got := SubjectFromClaims(&Claims{Subject: "discord:123"}); got != "discord:123" {
		t.Errorf("有 sub 時應直接用，得到 %q", got)
	}
	// 舊 token 無 sub → 由 login_type:username 推導
	if got := SubjectFromClaims(&Claims{Username: "bob", LoginType: "local"}); got != "local:bob" {
		t.Errorf("無 sub 的後備推導錯誤，得到 %q", got)
	}
}

func TestMiddleware(t *testing.T) {
	a := newTestAuth(t)

	// 缺 token → 401
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/x", nil)
	a.Middleware()(c)
	if w.Code != 401 {
		t.Errorf("缺 token 應回 401，得到 %d", w.Code)
	}

	// 有效 token → 設定 context 的 username/subject
	tok, _ := a.SignJWT("alice", "local", "local:alice")
	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	c2.Request = httptest.NewRequest("GET", "/api/x", nil)
	c2.Request.Header.Set("Authorization", "Bearer "+tok)
	a.Middleware()(c2)
	if c2.IsAborted() {
		t.Error("有效 token 不應被中止")
	}
	if c2.GetString("subject") != "local:alice" || c2.GetString("username") != "alice" {
		t.Errorf("context 未正確設定：subject=%q username=%q", c2.GetString("subject"), c2.GetString("username"))
	}
}

func TestPortalMiddlewareUsesEntryAuthClaims(t *testing.T) {
	a := newPortalTestAuth(t)
	token := signPortalToken(t, []string{"pages.read:teamA", "pages.write:shared"})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/me?token=must-not-be-used", nil)
	c.Request.AddCookie(&http.Cookie{Name: entryauth.CookieName, Value: token})
	a.Middleware()(c)
	if c.IsAborted() {
		t.Fatalf("valid portal cookie should pass: %s", w.Body.String())
	}
	if !c.GetBool("portal_auth") || c.GetString("auth_source") != string(SourceCookie) {
		t.Fatalf("portal context/source missing: %#v", c.Keys)
	}
	permissions := authz.PortalPermissionsOf(c)
	if len(permissions) != 2 || !authz.CanPortal(permissions, "shared/x.md", authz.AccessWrite) {
		t.Fatalf("portal app permissions missing: %#v", permissions)
	}
}

func TestPortalDoesNotAcceptQueryTokenForAPI(t *testing.T) {
	a := newPortalTestAuth(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/me?token="+signPortalToken(t, []string{"pages.read"}), nil)
	a.Middleware()(c)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("portal API query token should be rejected, got %d", w.Code)
	}
}

func TestPortalLoginURLTrustedForwardedOrigin(t *testing.T) {
	a := newPortalTestAuth(t)
	a.cfg.PortalURL = "https://gcentry.jonawu55.com"
	a.cfg.TrustedProxies = []string{"172.24.15.23"}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "http://backend:8080/", nil)
	c.Request.RemoteAddr = "172.24.15.23:43210"
	c.Request.Header.Set("X-Forwarded-Proto", "https")
	c.Request.Header.Set("X-Forwarded-Host", "docnest.jonawu55.com")

	if got, want := redirectTarget(t, a.portalLoginURL(c)), "https://docnest.jonawu55.com/"; got != want {
		t.Fatalf("redirect target = %q, want %q", got, want)
	}
}

func TestPortalLoginURLFallsBackToRequestOrigin(t *testing.T) {
	a := newPortalTestAuth(t)
	for _, tc := range []struct {
		name, requestURL, want string
		tls                    bool
	}{
		{"plain HTTP", "http://docnest.internal:8080/notes?a=1", "http://docnest.internal:8080/notes?a=1", false},
		{"direct TLS", "https://docnest.jonawu55.com/", "https://docnest.jonawu55.com/", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, tc.requestURL, nil)
			if tc.tls {
				c.Request.TLS = &tls.ConnectionState{}
			}
			if got := redirectTarget(t, a.portalLoginURL(c)); got != tc.want {
				t.Fatalf("redirect target = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPortalLoginURLIgnoresForwardedOriginFromUntrustedPeer(t *testing.T) {
	a := newPortalTestAuth(t)
	a.cfg.TrustedProxies = []string{"172.24.15.23"}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "http://docnest.internal:8080/", nil)
	c.Request.RemoteAddr = "203.0.113.50:54321"
	c.Request.Header.Set("X-Forwarded-Proto", "https")
	c.Request.Header.Set("X-Forwarded-Host", "attacker.example")

	if got, want := redirectTarget(t, a.portalLoginURL(c)), "http://docnest.internal:8080/"; got != want {
		t.Fatalf("redirect target = %q, want %q", got, want)
	}
}

func TestRequireWriteHeaderForPortalCookie(t *testing.T) {
	for _, tc := range []struct {
		name, header, value string
		want                int
	}{
		{"missing", "", "", http.StatusForbidden},
		{"custom header", "X-Requested-With", "DocNest", http.StatusOK},
		{"same origin beacon", "Sec-Fetch-Site", "same-origin", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/file", nil)
			c.Set("auth_source", string(SourceCookie))
			if tc.header != "" {
				c.Request.Header.Set(tc.header, tc.value)
			}
			RequireWriteHeader()(c)
			if w.Code != tc.want {
				t.Fatalf("got %d want %d", w.Code, tc.want)
			}
		})
	}
}

func TestLoginRateLimit(t *testing.T) {
	a := newTestAuth(t)
	ip := "1.2.3.4"
	if a.loginBlocked(ip) {
		t.Error("初始不應被封鎖")
	}
	for i := 0; i < loginMaxFailures; i++ {
		a.recordLoginFailure(ip)
	}
	if !a.loginBlocked(ip) {
		t.Error("達失敗上限後應被封鎖")
	}
	a.resetLoginFailures(ip)
	if a.loginBlocked(ip) {
		t.Error("重置後應解除封鎖")
	}
}
