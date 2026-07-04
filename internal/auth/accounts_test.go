package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// writeAccounts 在臨時目錄寫一份 accounts.json，回傳路徑。
func writeAccounts(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "accounts.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseAccounts(t *testing.T) {
	// alice 為 bcrypt（保留）；bob 為明文（忽略）；空鍵忽略。
	a, err := LoadAccounts(writeAccounts(t, `{
	  "local": { "alice": "$2a$10$abc", "bob": "plaintext", "": "$2a$10$xyz" },
	  "discord_allowed": ["111", " 222 ", ""]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Lookup("alice"); !ok {
		t.Error("alice（bcrypt）應保留")
	}
	if _, ok := a.Lookup("bob"); ok {
		t.Error("bob（明文）應被忽略")
	}
	if !a.IsDiscordAllowed("111") || !a.IsDiscordAllowed("222") {
		t.Error("Discord 白名單應含 111 與 222（去空白）")
	}
	if a.IsDiscordAllowed("999") {
		t.Error("名單外的 Discord ID 不應通過")
	}
}

func TestAccountsMissingFile(t *testing.T) {
	// 檔案不存在 → 空帳號啟動（不報錯），無人可登入
	a, err := LoadAccounts(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Lookup("alice"); ok {
		t.Error("空帳號時不應查得任何使用者")
	}
	if a.IsDiscordAllowed("111") {
		t.Error("空帳號時 Discord 一律拒絕")
	}
}

func TestAccountsInvalidJSON(t *testing.T) {
	if _, err := LoadAccounts(writeAccounts(t, "{ not json")); err == nil {
		t.Error("無效 JSON 應回傳錯誤")
	}
}

func TestAccountsReload(t *testing.T) {
	p := writeAccounts(t, `{"local":{"alice":"$2a$10$abc"},"discord_allowed":["111"]}`)
	a, err := LoadAccounts(p)
	if err != nil {
		t.Fatal(err)
	}

	// 覆寫：移除 alice、新增 bob；Discord 改為 222
	if err := os.WriteFile(p, []byte(`{"local":{"bob":"$2a$10$def"},"discord_allowed":["222"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(p); err != nil {
		t.Fatal(err)
	}

	if _, ok := a.Lookup("alice"); ok {
		t.Error("重載後 alice 應消失")
	}
	if _, ok := a.Lookup("bob"); !ok {
		t.Error("重載後 bob 應生效")
	}
	if a.IsDiscordAllowed("111") || !a.IsDiscordAllowed("222") {
		t.Error("重載後 Discord 白名單應更新為 222")
	}
}

func TestAccountsReloadInvalidKeepsOld(t *testing.T) {
	p := writeAccounts(t, `{"local":{"alice":"$2a$10$abc"}}`)
	a, _ := LoadAccounts(p)

	if err := os.WriteFile(p, []byte("{ broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(p); err == nil {
		t.Error("解析失敗應回傳錯誤")
	}
	if _, ok := a.Lookup("alice"); !ok {
		t.Error("重載失敗後應保留舊帳號")
	}
}
