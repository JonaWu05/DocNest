package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"markdownEditor/internal/authz"
	"markdownEditor/internal/config"
)

// loadAuthz 在臨時目錄寫一份 permissions.json 並載入，供權限過濾測試使用。
func loadAuthz(t *testing.T, body string) *authz.Authz {
	t.Helper()
	p := filepath.Join(t.TempDir(), "permissions.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := authz.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// decodePresence 讀取某連線收到的 presence_update 並回傳使用者清單。
func decodePresence(t *testing.T, data []byte) []PresenceUser {
	t.Helper()
	var m struct {
		Type    string          `json:"type"`
		Payload presencePayload `json:"payload"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.Type != "presence_update" {
		t.Fatalf("type=%q want presence_update", m.Type)
	}
	return m.Payload.Users
}

// TestBroadcastPresenceDedup 驗證 presence 以穩定身分鍵去重：
// 同一使用者多條連線只算一人，且狀態合併（任一在編輯即編輯中、採有開檔那條的檔名）。
func TestBroadcastPresenceDedup(t *testing.T) {
	h := &Hub{clients: map[*Client]bool{}}
	c1 := &Client{send: make(chan []byte, 4), username: "jonawu", subject: "discord:1", currentFile: "a.md"}
	c2 := &Client{send: make(chan []byte, 4), username: "jonawu", subject: "discord:1", currentFile: "b.md", isEditing: true}
	c3 := &Client{send: make(chan []byte, 4), username: "bob", subject: "local:bob"}
	h.clients[c1] = true
	h.clients[c2] = true
	h.clients[c3] = true

	h.broadcastPresence()

	// 任取一條連線收到的廣播來檢查（內容對所有人相同）
	var m struct {
		Type    string          `json:"type"`
		Payload presencePayload `json:"payload"`
	}
	if err := json.Unmarshal(<-c3.send, &m); err != nil {
		t.Fatal(err)
	}
	if m.Type != "presence_update" {
		t.Errorf("type=%q want presence_update", m.Type)
	}
	if len(m.Payload.Users) != 2 {
		t.Fatalf("去重後應為 2 人，得到 %d：%+v", len(m.Payload.Users), m.Payload.Users)
	}

	var found bool
	for _, u := range m.Payload.Users {
		if u.Username == "jonawu" {
			found = true
			if !u.IsEditing || u.CurrentFile != "b.md" {
				t.Errorf("多連線狀態合併錯誤：%+v（期望 editing=true, file=b.md）", u)
			}
		}
	}
	if !found {
		t.Error("找不到合併後的 jonawu")
	}
}

// TestBroadcastPresenceFiltersUnreadableFiles 驗證 presence 對每位收件者個別過濾：
// 對方無讀取權的 current_file 會被遮去（連帶 is_editing），但仍保留該人在線。
func TestBroadcastPresenceFiltersUnreadableFiles(t *testing.T) {
	az := loadAuthz(t, `{"default":"none","groups":{
	  "team":{"members":["local:alice"],"rules":[{"path":"teamA","access":"read"}]},
	  "sec":{"members":["local:bob"],"rules":[{"path":"secret","access":"write"}]}
	}}`)
	h := &Hub{az: az, clients: map[*Client]bool{}}
	alice := &Client{send: make(chan []byte, 4), username: "alice", subject: "local:alice"}
	bob := &Client{send: make(chan []byte, 4), username: "bob", subject: "local:bob", currentFile: "secret/plan.md", isEditing: true}
	h.clients[alice] = true
	h.clients[bob] = true

	h.broadcastPresence()

	// alice 對 secret 無讀取權：bob 的 current_file 應被遮去、is_editing 轉為 false。
	for _, u := range decodePresence(t, <-alice.send) {
		if u.Username == "bob" {
			if u.CurrentFile != "" || u.IsEditing {
				t.Errorf("alice 不應看到 bob 的 secret 路徑：%+v", u)
			}
		}
	}

	// bob 對 secret 有寫入權（隱含讀取）：應看到自己完整的 current_file。
	var seen bool
	for _, u := range decodePresence(t, <-bob.send) {
		if u.Username == "bob" {
			seen = true
			if u.CurrentFile != "secret/plan.md" || !u.IsEditing {
				t.Errorf("bob 應看到自己的完整狀態：%+v", u)
			}
		}
	}
	if !seen {
		t.Error("bob 的 presence 中找不到自己")
	}
}

// TestDeliverFiltersByReadPermission 驗證帶 path 的訊息只送給有讀取權的連線。
func TestDeliverFiltersByReadPermission(t *testing.T) {
	az := loadAuthz(t, `{"default":"none","groups":{
	  "sec":{"members":["local:bob"],"rules":[{"path":"secret","access":"read"}]}
	}}`)
	h := &Hub{az: az, clients: map[*Client]bool{}}
	alice := &Client{send: make(chan []byte, 4), subject: "local:alice"}
	bob := &Client{send: make(chan []byte, 4), subject: "local:bob"}
	h.clients[alice] = true
	h.clients[bob] = true

	h.deliver(outbound{data: []byte("x"), path: "secret/plan.md"})

	if len(alice.send) != 0 {
		t.Error("alice 無 secret 讀取權，不應收到通知")
	}
	if len(bob.send) != 1 {
		t.Error("bob 有 secret 讀取權，應收到通知")
	}
}

// TestCloseShutsDownRun 驗證 Close 會結束 Run 迴圈並關閉所有連線的 send channel。
func TestCloseShutsDownRun(t *testing.T) {
	h := New(nil, nil, &config.Config{})
	done := make(chan struct{})
	go func() { h.Run(); close(done) }()

	c := &Client{send: make(chan []byte, 4), subject: "local:x"}
	h.register <- c // 經由 Run 註冊，確保在 hub goroutine 內加入 clients

	h.Close()

	// Run 應結束
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close 後 Run 未結束")
	}

	// 連線的 send 應被關閉（writePump 據此送出 Close frame 後退出）
	select {
	case _, ok := <-c.send:
		if ok {
			// 可能先有 presence 訊息；耗盡後最終應關閉
			for range c.send {
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send channel 未關閉")
	}

	// Close 可重複呼叫而不 panic
	h.Close()
}
