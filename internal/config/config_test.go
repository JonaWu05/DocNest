package config

import (
	"testing"
)

func TestParseBoolEnv(t *testing.T) {
	const key = "TEST_BOOL_ENV"
	truthy := []string{"1", "true", "TRUE", "Yes", " on "}
	for _, v := range truthy {
		t.Setenv(key, v)
		if !parseBoolEnv(key) {
			t.Errorf("%q 應為 true", v)
		}
	}
	falsy := []string{"", "0", "false", "no", "random"}
	for _, v := range falsy {
		t.Setenv(key, v)
		if parseBoolEnv(key) {
			t.Errorf("%q 應為 false", v)
		}
	}
}

func TestParseUsers(t *testing.T) {
	// alice/carol 為 bcrypt（保留）；bob 為明文（忽略）；nocolon 無冒號（忽略）
	u := parseUsers("alice:$2a$10$abc,bob:plaintext,nocolon,  carol:$2b$10$xyz  ")
	if _, ok := u["alice"]; !ok {
		t.Error("alice（bcrypt）應保留")
	}
	if _, ok := u["carol"]; !ok {
		t.Error("carol（bcrypt，去空白）應保留")
	}
	if _, ok := u["bob"]; ok {
		t.Error("bob（明文）應被忽略")
	}
	if len(u) != 2 {
		t.Errorf("應有 2 位使用者，得到 %d：%v", len(u), u)
	}
}

func TestParseTrustedProxies(t *testing.T) {
	if got := parseTrustedProxies(""); got != nil {
		t.Errorf("空字串應回傳 nil，得到 %v", got)
	}
	got := parseTrustedProxies(" 10.0.0.1 , 192.168.0.0/16 ,")
	if len(got) != 2 || got[0] != "10.0.0.1" || got[1] != "192.168.0.0/16" {
		t.Errorf("解析結果不正確：%v", got)
	}
}

func TestOriginAllowed(t *testing.T) {
	open := &Config{} // 未設定 AllowedOrigins → 開發模式全放行
	if !open.OriginAllowed("https://evil.example") {
		t.Error("未設定來源時應全部放行")
	}
	c := &Config{AllowedOrigins: []string{"https://a.com"}}
	if !c.OriginAllowed("https://a.com") {
		t.Error("清單內來源應放行")
	}
	if c.OriginAllowed("https://b.com") {
		t.Error("清單外來源應拒絕")
	}
}
