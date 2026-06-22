package hub

import (
	"encoding/json"
	"testing"
)

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
