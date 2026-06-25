package collab

import (
	"encoding/json"
	"testing"
)

// 測試用：建立一個不接真 WS 的 client（send 緩衝夠大即不會走到關閉 conn 的分支）。
func newClient(canWrite bool) *client { return newClientN(canWrite, 64) }

// newClientN 同 newClient，但可指定 send 緩衝大小（壓縮測試需容納大量廣播）。
func newClientN(canWrite bool, buf int) *client {
	return &client{send: make(chan []byte, buf), canWrite: canWrite, username: "u", subject: "s"}
}

// drainControl 取出 c.send 中第一則指定 type 的控制訊息（其餘略過）；找不到回傳 false。
func drainControl(c *client, typ string) bool {
	for {
		select {
		case b := <-c.send:
			if len(b) > 0 && b[0] == tagControl {
				var m controlMsg
				if json.Unmarshal(b[1:], &m) == nil && m.Type == typ {
					return true
				}
			}
		default:
			return false
		}
	}
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
	drainControl(w1, "stream") // r1 加入(第二人)觸發給 w1 的 stream 通知,先清掉

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

// TestSoloDeferredStreaming(最佳化 B):單人 init.Stream=false；第二人加入後 init.Stream=true，
// 且原本獨自在房的 saver 收到 stream:true + sendState 通知（補餵新加入者）。
func TestSoloDeferredStreaming(t *testing.T) {
	h := newHub()
	w1 := newClient(true)
	init1, _ := h.addClient("doc.md", w1)
	if parseInit(t, init1).Stream {
		t.Error("單人加入時 Stream 應為 false（本機累積不上傳）")
	}

	w2 := newClient(true)
	init2, _ := h.addClient("doc.md", w2)
	if !parseInit(t, init2).Stream {
		t.Error("第二人加入後 Stream 應為 true")
	}
	ctrl := parseControl(t, recvFrame(t, w1))
	if ctrl.Type != "stream" || !ctrl.Stream || !ctrl.SendState {
		t.Errorf("w1 應收到 stream(Stream+SendState) 通知，got %+v", ctrl)
	}
}

// TestStopStreamingWhenSolo(最佳化 B):多人變回單人時，剩餘者收到 stream:false（停止上傳）。
func TestStopStreamingWhenSolo(t *testing.T) {
	h := newHub()
	w1 := newClient(true)
	h.addClient("doc.md", w1)
	w2 := newClient(true)
	h.addClient("doc.md", w2)
	drainControl(w1, "stream") // 清掉 w2 加入時給 w1 的 stream:true

	h.removeClient(w2) // 回到單人
	ctrl := parseControl(t, recvFrame(t, w1))
	if ctrl.Type != "stream" || ctrl.Stream {
		t.Errorf("回到單人時 w1 應收到 stream:false，got %+v", ctrl)
	}
}

// TestLogCompaction(最佳化 A):log 達門檻後 saver 收到 compact 請求；
// saver 回傳完整狀態後，log 被壓縮為「快照 + 其後 tail」。
func TestLogCompaction(t *testing.T) {
	h := newHub()
	w1 := newClientN(true, compactThreshold+16) // saver；緩衝需容納大量廣播
	h.addClient("doc.md", w1)
	w2 := newClientN(true, compactThreshold+16)
	h.addClient("doc.md", w2)

	for i := 0; i < compactThreshold; i++ {
		h.handleFrame(w2, frame(tagUpdate, []byte("x"))) // w2 送 update，w1 為 saver
	}
	if !drainControl(w1, "compact") {
		t.Fatal("log 達門檻後 saver 應收到 compact 請求")
	}

	before := len(h.rooms["doc.md"].log)
	h.handleFrame(w1, frame(tagState, []byte("SNAPSHOT"))) // saver 回傳完整狀態
	r := h.rooms["doc.md"]
	if len(r.log) >= before {
		t.Errorf("壓縮後 log 應變短，before=%d after=%d", before, len(r.log))
	}
	if len(r.log) == 0 || string(r.log[0]) != "SNAPSHOT" {
		t.Errorf("壓縮後 log[0] 應為快照，got %v", r.log)
	}
}
