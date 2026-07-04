package auth

import (
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestValidateUsername(t *testing.T) {
	if err := ValidateUsername("alice"); err != nil {
		t.Errorf("alice 應合法：%v", err)
	}
	for _, bad := range []string{"", "   ", "local:alice"} {
		if err := ValidateUsername(bad); err == nil {
			t.Errorf("%q 應為非法", bad)
		}
	}
}

func TestCreateAndSetPassword(t *testing.T) {
	p := writeAccounts(t, `{"local":{},"discord_allowed":[]}`)
	a, _ := LoadAccounts(p)

	hash, err := HashPassword("secret1")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.CreateUser("alice", hash); err != nil {
		t.Fatal(err)
	}
	// 建立後可查得，且密碼比對成立
	got, ok := a.Lookup("alice")
	if !ok || bcrypt.CompareHashAndPassword([]byte(got), []byte("secret1")) != nil {
		t.Error("建立後 alice 應可用 secret1 通過比對")
	}
	// 重複建立 → 錯誤
	if err := a.CreateUser("alice", hash); err == nil {
		t.Error("重複建立同名帳號應回錯")
	}
	// 非法帳號名 → 錯誤
	if err := a.CreateUser("bad:name", hash); err == nil {
		t.Error("含冒號的帳號名應回錯")
	}

	// 重設密碼
	h2, _ := HashPassword("secret2")
	if err := a.SetPassword("alice", h2); err != nil {
		t.Fatal(err)
	}
	got, _ = a.Lookup("alice")
	if bcrypt.CompareHashAndPassword([]byte(got), []byte("secret2")) != nil {
		t.Error("重設後應以 secret2 通過比對")
	}
	// 重設不存在的帳號 → 錯誤
	if err := a.SetPassword("nobody", h2); err == nil {
		t.Error("重設不存在帳號應回錯")
	}

	// 持久化：重新載入仍在
	b, _ := LoadAccounts(p)
	if !b.HasUser("alice") {
		t.Error("寫回後重新載入應仍有 alice")
	}
}

func TestDeleteUser(t *testing.T) {
	hash, _ := HashPassword("x")
	a, _ := LoadAccounts(writeAccounts(t, `{"local":{"alice":"`+hash+`"}}`))
	if err := a.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	if a.HasUser("alice") {
		t.Error("刪除後 alice 應消失")
	}
	if err := a.DeleteUser("alice"); err == nil {
		t.Error("刪除不存在帳號應回錯")
	}
}

func TestDiscordAllowlistMutation(t *testing.T) {
	p := writeAccounts(t, `{"local":{},"discord_allowed":["111"]}`)
	a, _ := LoadAccounts(p)

	if err := a.AddDiscord("222"); err != nil {
		t.Fatal(err)
	}
	if err := a.AddDiscord("222"); err != nil { // 冪等
		t.Fatal(err)
	}
	if !a.IsDiscordAllowed("111") || !a.IsDiscordAllowed("222") {
		t.Error("111 與 222 都應在白名單")
	}
	if err := a.AddDiscord("  "); err == nil {
		t.Error("空 ID 應回錯")
	}

	if err := a.RemoveDiscord("111"); err != nil {
		t.Fatal(err)
	}
	if a.IsDiscordAllowed("111") {
		t.Error("移除後 111 不應在白名單")
	}

	// 持久化
	b, _ := LoadAccounts(p)
	if b.IsDiscordAllowed("111") || !b.IsDiscordAllowed("222") {
		t.Error("寫回後應只剩 222")
	}
}

func TestCreateUserOnMissingFile(t *testing.T) {
	// 檔案不存在時，建立首個帳號應自動產生檔案
	p := filepath.Join(t.TempDir(), "accounts.json")
	a, _ := LoadAccounts(p)
	hash, _ := HashPassword("x")
	if err := a.CreateUser("first", hash); err != nil {
		t.Fatal(err)
	}
	b, _ := LoadAccounts(p)
	if !b.HasUser("first") {
		t.Error("檔案原不存在時建立帳號後應可載入")
	}
}
