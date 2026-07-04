package auth

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"markdownEditor/internal/authz"
	"markdownEditor/internal/config"
)

// newAdminTestAuth 建立一個帶「admins 群組含 local:boss、另有 editors 群組」的 Auth，回傳它與 admin 的 subject。
func newAdminTestAuth(t *testing.T) *Auth {
	t.Helper()
	permPath := filepath.Join(t.TempDir(), "permissions.json")
	perms := `{"default":"none","groups":{
	  "admins":{"members":["local:boss"],"rules":[{"path":"","access":"write"}]},
	  "editors":{"members":[],"rules":[{"path":"teamA","access":"write"}]}
	}}`
	if err := os.WriteFile(permPath, []byte(perms), 0o644); err != nil {
		t.Fatal(err)
	}
	az, err := authz.Load(permPath)
	if err != nil {
		t.Fatal(err)
	}
	accPath := writeAccounts(t, `{"local":{},"discord_allowed":[]}`)
	accounts, err := LoadAccounts(accPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		JWTSecret: []byte("test"), JWTExpire: time.Hour,
		PermissionsFile: permPath, AccountsFile: accPath,
	}
	return New(cfg, accounts, az)
}

// adminCtx 造一個帶身分與 JSON body 的測試 context。
func adminCtx(w *httptest.ResponseRecorder, subject, username, loginType, body string) *gin.Context {
	c, _ := gin.CreateTestContext(w)
	c.Set("subject", subject)
	c.Set("username", username)
	c.Set("login_type", loginType)
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	return c
}

func TestAdminGuardRejectsNonAdmin(t *testing.T) {
	a := newAdminTestAuth(t)
	w := httptest.NewRecorder()
	c := adminCtx(w, "local:nobody", "nobody", "local", `{"username":"x","password":"y12345"}`)
	a.CreateUserHandler(c)
	if w.Code != 403 {
		t.Errorf("非管理員應回 403，得到 %d", w.Code)
	}
}

func TestCreateUserHandler(t *testing.T) {
	a := newAdminTestAuth(t)
	w := httptest.NewRecorder()
	c := adminCtx(w, "local:boss", "boss", "local", `{"username":"alice","password":"pw123456"}`)
	a.CreateUserHandler(c)
	if w.Code != 200 {
		t.Fatalf("建立帳號應回 200，得到 %d：%s", w.Code, w.Body.String())
	}
	if !a.accounts.HasUser("alice") {
		t.Error("建立後應有 alice")
	}
}

func TestDeleteSelfBlocked(t *testing.T) {
	a := newAdminTestAuth(t)
	w := httptest.NewRecorder()
	c := adminCtx(w, "local:boss", "boss", "local", `{"username":"boss"}`)
	a.DeleteUserHandler(c)
	if w.Code != 400 {
		t.Errorf("刪除自己應回 400，得到 %d", w.Code)
	}
}

func TestSetGroupMemberSelfDemoteBlocked(t *testing.T) {
	a := newAdminTestAuth(t)
	w := httptest.NewRecorder()
	c := adminCtx(w, "local:boss", "boss", "local", `{"group":"admins","subject":"local:boss","add":false}`)
	a.SetGroupMemberHandler(c)
	if w.Code != 400 {
		t.Errorf("把自己移出 admins 應回 400，得到 %d", w.Code)
	}
	if !a.az.IsAdmin("local:boss") {
		t.Error("被阻擋後 boss 仍應為管理員")
	}
}

func TestSetGroupMemberAddSucceeds(t *testing.T) {
	a := newAdminTestAuth(t)
	w := httptest.NewRecorder()
	c := adminCtx(w, "local:boss", "boss", "local", `{"group":"editors","subject":"local:alice","add":true}`)
	a.SetGroupMemberHandler(c)
	if w.Code != 200 {
		t.Fatalf("加入 editors 應回 200，得到 %d：%s", w.Code, w.Body.String())
	}
	groups := a.az.GroupsOf("local:alice")
	if len(groups) != 1 || groups[0] != "editors" {
		t.Errorf("alice 應屬 editors，得到 %v", groups)
	}
}

func TestChangeOwnPassword(t *testing.T) {
	a := newAdminTestAuth(t)
	hash, _ := HashPassword("oldpass")
	if err := a.accounts.CreateUser("alice", hash); err != nil {
		t.Fatal(err)
	}

	// 舊密碼錯誤 → 401
	w := httptest.NewRecorder()
	c := adminCtx(w, "local:alice", "alice", "local", `{"old_password":"wrong","new_password":"newpass1"}`)
	a.ChangeOwnPasswordHandler(c)
	if w.Code != 401 {
		t.Errorf("舊密碼錯誤應回 401，得到 %d", w.Code)
	}

	// 正確 → 200，且新密碼生效
	w = httptest.NewRecorder()
	c = adminCtx(w, "local:alice", "alice", "local", `{"old_password":"oldpass","new_password":"newpass1"}`)
	a.ChangeOwnPasswordHandler(c)
	if w.Code != 200 {
		t.Fatalf("正確舊密碼應回 200，得到 %d：%s", w.Code, w.Body.String())
	}
	got, _ := a.accounts.Lookup("alice")
	if bcrypt.CompareHashAndPassword([]byte(got), []byte("newpass1")) != nil {
		t.Error("改密碼後應以新密碼通過比對")
	}

	// Discord 登入者 → 400
	w = httptest.NewRecorder()
	c = adminCtx(w, "discord:123", "someone", "discord", `{"old_password":"x","new_password":"y1234567"}`)
	a.ChangeOwnPasswordHandler(c)
	if w.Code != 400 {
		t.Errorf("Discord 登入者改密碼應回 400，得到 %d", w.Code)
	}
}
