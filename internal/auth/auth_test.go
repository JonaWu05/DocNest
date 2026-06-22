package auth

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"markdownEditor/internal/authz"
	"markdownEditor/internal/config"
)

func init() { gin.SetMode(gin.TestMode) }

func newTestAuth(t *testing.T) *Auth {
	t.Helper()
	cfg := &config.Config{JWTSecret: []byte("test-secret"), JWTExpire: time.Hour}
	az, err := authz.Load(filepath.Join(t.TempDir(), "none.json")) // 停用模式即可
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, az)
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
	other := New(&config.Config{JWTSecret: []byte("different-secret"), JWTExpire: time.Hour}, a.az)
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
