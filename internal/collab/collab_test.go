package collab

import (
	"encoding/json"
	"testing"
)

// 測試用：建立一個不接真 WS 的 client（send 緩衝夠大即不會走到關閉 conn 的分支）。
func newClient(canWrite bool) *client {
	return &client{send: make(chan []byte, 64), canWrite: canWrite, username: "u", subject: "s"}
}

func newHub() *Hub { return &Hub{rooms: map[string]*room{}} }

// 從 client 的 send 取一則 frame；無訊息則 fail。
func recvFrame(t *testing.T, c *client) []byte {
	t.Helper()
	select {
	case b := <-c.send:
		return b
	default:
		t.Fatal("預期有訊息，但 send 為空")
		return nil
	}
}

func parseControl(t *testing.T, b []byte) controlMsg {
	t.Helper()
	if len(b) == 0 || b[0] != tagControl {
		t.Fatalf("預期控制訊息（tag=c），got %v", b)
	}
	var m controlMsg
	if err := json.Unmarshal(b[1:], &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func parseInit(t *testing.T, initMsg []byte) controlMsg {
	t.Helper()
	var m controlMsg
	if err := json.Unmarshal(initMsg, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestJoinRolesAndSeed：第一個可寫者成為 seeder/saver；唯讀者不 seed、不可寫。
func TestJoinRolesAndSeed(t *testing.T) {
	h := newHub()
	w1 := newClient(true)
	init1, replay1 := h.addClient("doc.md", w1)
	m1 := parseInit(t, init1)
	if !m1.Seed || !m1.CanWrite || !m1.Saver {
		t.Errorf("第一個可寫者應 seed+canWrite+saver，got %+v", m1)
	}
	if len(replay1) != 0 {
		t.Errorf("新房間回放應為空，got %d", len(replay1))
	}

	r1 := newClient(false)
	init2, _ := h.addClient("doc.md", r1)
	m2 := parseInit(t, init2)
	if m2.Seed || m2.CanWrite || m2.Saver {
		t.Errorf("唯讀者不應 seed/canWrite/saver，got %+v", m2)
	}
}

// TestUpdateRelayAndReadonly：可寫者的 update 會存入 log 並轉發；唯讀者的 update 被忽略。
func TestUpdateRelayAndReadonly(t *testing.T) {
	h := newHub()
	w1 := newClient(true)
	h.addClient("doc.md", w1)
	r1 := newClient(false)
	h.addClient("doc.md", r1)

	h.handleFrame(w1, frame(tagUpdate, []byte("hello")))
	got := recvFrame(t, r1)
	if got[0] != tagUpdate || string(got[1:]) != "hello" {
		t.Errorf("唯讀者應收到轉發的 update，got %v", got)
	}
	if r := h.rooms["doc.md"]; len(r.log) != 1 || !r.seeded {
		t.Errorf("update 應存入 log 並標記 seeded，got len=%d seeded=%v", len(r.log), r.seeded)
	}

	// 唯讀者送 update → 應被忽略（不轉發給 w1、log 不變）
	h.handleFrame(r1, frame(tagUpdate, []byte("nope")))
	select {
	case b := <-w1.send:
		t.Errorf("唯讀者的 update 不應轉發，卻收到 %v", b)
	default:
	}
	if r := h.rooms["doc.md"]; len(r.log) != 1 {
		t.Errorf("唯讀者的 update 不應入 log，got len=%d", len(r.log))
	}
}

// TestLateJoinReplay：晚加入者應收到既有 update 的回放。
func TestLateJoinReplay(t *testing.T) {
	h := newHub()
	w1 := newClient(true)
	h.addClient("doc.md", w1)
	h.handleFrame(w1, frame(tagUpdate, []byte("aaa")))
	h.handleFrame(w1, frame(tagUpdate, []byte("bbb")))

	w2 := newClient(true)
	init2, replay := h.addClient("doc.md", w2)
	m2 := parseInit(t, init2)
	if m2.Seed || m2.Saver {
		t.Errorf("已 seed 的房間，晚加入者不應 seed/saver，got %+v", m2)
	}
	if len(replay) != 2 || string(replay[0]) != "aaa" || string(replay[1]) != "bbb" {
		t.Errorf("回放應含既有兩則 update，got %v", replay)
	}
}

// TestSaverHandoffAndEmptyRoom：saver 離開→移交給其他可寫者；房間清空→回收。
func TestSaverHandoffAndEmptyRoom(t *testing.T) {
	h := newHub()
	w1 := newClient(true)
	h.addClient("doc.md", w1)
	w2 := newClient(true)
	h.addClient("doc.md", w2)

	h.removeClient(w1) // saver(w1) 離開 → 應移交 w2
	ctrl := parseControl(t, recvFrame(t, w2))
	if ctrl.Type != "role" || !ctrl.Saver {
		t.Errorf("w2 應收到 role saver 通知，got %+v", ctrl)
	}
	if r := h.rooms["doc.md"]; r == nil || r.saver != w2 {
		t.Errorf("saver 應移交給 w2")
	}

	h.removeClient(w2) // 最後一人離開 → 房間回收
	if _, ok := h.rooms["doc.md"]; ok {
		t.Error("空房應被回收")
	}
}
