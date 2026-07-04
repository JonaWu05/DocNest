package authz

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// writePerms 在臨時目錄寫一份 permissions.json，回傳路徑。
func writePerms(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "permissions.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCanEffectiveAccess(t *testing.T) {
	a, err := Load(writePerms(t, `{
	  "default": "none",
	  "groups": {
	    "everyone": { "members": ["*"],          "rules": [{"path":"welcome.md","access":"read"}] },
	    "admins":   { "members": ["local:admin"], "rules": [{"path":"","access":"write"}] },
	    "editors":  { "members": ["local:alice"], "rules": [{"path":"teamA","access":"write"},{"path":"shared","access":"read"}] }
	  }
	}`))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		subj, path string
		need       int
		want       bool
	}{
		{"local:admin", "any/where.md", AccessWrite, true},     // 根規則涵蓋全部
		{"local:alice", "teamA/x.md", AccessWrite, true},       // 前綴 write
		{"local:alice", "teamA", AccessWrite, true},            // 等於前綴本身
		{"local:alice", "teamAnother/x.md", AccessRead, false}, // 不可被 "teamA" 誤命中
		{"local:alice", "shared/y.md", AccessWrite, false},     // shared 只給 read
		{"local:alice", "shared/y.md", AccessRead, true},
		{"local:alice", "other.md", AccessRead, false},   // 落到 default none
		{"local:alice", "welcome.md", AccessRead, true},  // 透過 "*" everyone
		{"local:nobody", "welcome.md", AccessRead, true}, // "*" 套用到所有人
		{"local:nobody", "other.md", AccessRead, false},
	}
	for _, c := range cases {
		if got := a.Can(c.subj, c.path, c.need); got != c.want {
			t.Errorf("Can(%q,%q,%d)=%v want %v", c.subj, c.path, c.need, got, c.want)
		}
	}
}

func TestHasAnyRead(t *testing.T) {
	// 無 "*" 群組：未分組者應無任何讀取權
	a, _ := Load(writePerms(t, `{"default":"none","groups":{
	  "editors":{"members":["local:alice"],"rules":[{"path":"teamA","access":"write"}]}
	}}`))
	if !a.HasAnyRead("local:alice") {
		t.Error("alice 應有讀取權")
	}
	if a.HasAnyRead("local:nobody") {
		t.Error("未分組者應無讀取權")
	}

	// 有 "*" 群組讀 welcome：所有人都算有讀取權
	b, _ := Load(writePerms(t, `{"default":"none","groups":{
	  "everyone":{"members":["*"],"rules":[{"path":"welcome.md","access":"read"}]}
	}}`))
	if !b.HasAnyRead("local:nobody") {
		t.Error("\"*\" 讀 welcome 應讓所有人 HasAnyRead=true")
	}
}

func TestDisabledModeAllowsAll(t *testing.T) {
	// 設定檔不存在 → 相容的全開模式
	a, err := Load(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !a.Can("anyone", "anything/deep.md", AccessWrite) {
		t.Error("停用模式應全開")
	}
	if !a.HasAnyRead("anyone") {
		t.Error("停用模式 HasAnyRead 應為 true")
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	if _, err := Load(writePerms(t, "{ not json")); err == nil {
		t.Error("無效 JSON 應回傳錯誤")
	}
}

func TestIsAdmin(t *testing.T) {
	a, _ := Load(writePerms(t, `{"default":"none","groups":{
	  "admins":{"members":["local:boss","*"],"rules":[{"path":"","access":"write"}]},
	  "editors":{"members":["local:alice"],"rules":[{"path":"teamA","access":"write"}]}
	}}`))
	if !a.IsAdmin("local:boss") {
		t.Error("admins 具名成員應為管理員")
	}
	if a.IsAdmin("local:alice") {
		t.Error("非 admins 成員不應為管理員")
	}
	if a.IsAdmin("*") || a.IsAdmin("local:nobody") {
		t.Error("\"*\" 萬用成員不應被視為管理員")
	}

	// 停用模式（無設定檔）：所有登入者視為管理員
	dis, _ := Load(filepath.Join(t.TempDir(), "nonexistent.json"))
	if !dis.IsAdmin("anyone") {
		t.Error("停用模式應視所有人為管理員")
	}
}

func TestReload(t *testing.T) {
	p := writePerms(t, `{"default":"none","groups":{
	  "editors":{"members":["local:alice"],"rules":[{"path":"teamA","access":"write"}]}
	}}`)
	a, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Can("local:alice", "teamA/x.md", AccessWrite) {
		t.Fatal("重載前 alice 應可寫 teamA")
	}

	// 覆寫設定檔：改由 bob 管 teamB，並使 bob 成為管理員
	if err := os.WriteFile(p, []byte(`{"default":"none","groups":{
	  "admins":{"members":["local:bob"],"rules":[{"path":"","access":"write"}]},
	  "editors":{"members":["local:bob"],"rules":[{"path":"teamB","access":"write"}]}
	}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(p); err != nil {
		t.Fatal(err)
	}

	if a.Can("local:alice", "teamA/x.md", AccessWrite) {
		t.Error("重載後 alice 的舊規則應消失")
	}
	if !a.Can("local:bob", "teamB/y.md", AccessWrite) {
		t.Error("重載後 bob 的新規則應生效")
	}
	if !a.IsAdmin("local:bob") {
		t.Error("重載後 bob 應為管理員")
	}
}

func TestReloadInvalidKeepsOld(t *testing.T) {
	p := writePerms(t, `{"default":"none","groups":{
	  "editors":{"members":["local:alice"],"rules":[{"path":"teamA","access":"write"}]}
	}}`)
	a, _ := Load(p)

	// 寫入半形 JSON：Reload 應回錯且不動既有設定
	if err := os.WriteFile(p, []byte("{ broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(p); err == nil {
		t.Error("解析失敗應回傳錯誤")
	}
	if !a.Can("local:alice", "teamA/x.md", AccessWrite) {
		t.Error("重載失敗後應保留舊設定")
	}
}

func TestRequireAccess(t *testing.T) {
	a, _ := Load(writePerms(t, `{"default":"none","groups":{
	  "editors":{"members":["local:alice"],"rules":[{"path":"teamA","access":"write"}]}
	}}`))

	// 允許：放行、不寫狀態碼
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("subject", "local:alice")
	if !a.RequireAccess(c, "teamA/x.md", AccessWrite) {
		t.Error("teamA 應放行")
	}

	// 拒絕：回 403 並中止
	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	c2.Set("subject", "local:alice")
	if a.RequireAccess(c2, "other.md", AccessWrite) {
		t.Error("other.md 應拒絕")
	}
	if w2.Code != 403 {
		t.Errorf("拒絕應回 403，得到 %d", w2.Code)
	}
}
