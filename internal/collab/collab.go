// Package collab 提供即時共編的 WebSocket 房間層（M1/M2）：以文件路徑分房、權限 join、
// 中繼 Yjs 二進位 update 與 awareness，並為晚加入者回放既有 update（dumb relay：Go 端不解碼 CRDT）。
//
// 訊息格式:每個 WebSocket 二進位 frame 第 1 個位元組為 tag,其後為負載:
//   'u' 文件 update(Yjs)  'a' awareness(游標等)  's' 完整狀態快照(saver→伺服器)  'c' 控制訊息(JSON)
// 控制訊息僅由伺服器送出(init / role / stream / compact)。落地 .md 由被指派為 saver 的客戶端走既有 POST /api/file。
//
// 兩個負載最佳化(M2):
//   A. log 壓縮：log 累積過長時請 saver 送一份完整狀態('s')取代之,使記憶體上限 ≈ 文件大小而非編輯歷史。
//   B. 單人延後串流：房內僅一人時不需上傳 update(本機累積即可);第二人加入時才通知開始串流並補送完整狀態。
package collab

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"markdownEditor/internal/auth"
	"markdownEditor/internal/authz"
	"markdownEditor/internal/config"
)

const (
	tagUpdate    = 'u' // Yjs 文件 update
	tagAwareness = 'a' // awareness（游標 / 選取）
	tagState     = 's' // saver 送出的完整狀態快照（供 log 壓縮，不廣播）
	tagControl   = 'c' // 控制訊息（JSON）

	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 1 << 20 // 1 MB：Yjs update 可能較大
	sendBuffer     = 256

	compactThreshold = 256 // log 累積到此筆數即請 saver 送完整狀態壓縮（最佳化 A）
)

// Hub 管理所有共編房間。rooms 與房間/連線欄位的存取一律以 mu 保護。
type Hub struct {
	auth     *auth.Auth
	az       *authz.Authz
	upgrader websocket.Upgrader

	mu    sync.Mutex
	rooms map[string]*room
}

// room 為單一文件的共編房間。
type room struct {
	path        string
	clients     map[*client]bool
	log         [][]byte // 已廣播的文件 update（供晚加入者回放）；空房回收時清除
	seeded      bool     // 是否已由某客戶端用 .md 內容初始化
	saver       *client  // 負責落檔的客戶端（可寫者其一）
	compactMark int      // 壓縮請求當下的 log 長度（-1 表無待處理）；快照到達時保留其後新進的 update
	extPending  bool     // 偵測到外部改檔且 saver 尚未決定如何處理：saver 移交 / 加入 / 重連時告知新 saver
}

type client struct {
	hub       *Hub
	conn      *websocket.Conn
	send      chan []byte
	username  string
	subject   string
	canWrite  bool
	streaming bool   // 是否應上傳本地 update（最佳化 B：單人時為 false，本機累積不上傳）
	yjsID     int64  // 客戶端的 Yjs awareness clientID（經 hello 控制訊息登記）；離線時用於通知他人移除其游標
	hasYjsID  bool   // 是否已收到 hello（yjsID 有效）
	room      *room
}

// controlMsg 為共編控制訊息（以 tagControl 前綴的 JSON）。
// 伺服器→客戶端："init" | "role" | "stream" | "compact" | "peerLeft" | "external"；
// 客戶端→伺服器："hello"（登記 Yjs awareness clientID）| "extResolved"（saver 已處理外部改檔）。
type controlMsg struct {
	Type      string `json:"type"`
	Seed      bool   `json:"seed,omitempty"`      // 是否由你以 .md 內容初始化文件
	CanWrite  bool   `json:"canWrite,omitempty"`  // 你是否可寫（init）
	Saver     bool   `json:"saver,omitempty"`     // 你是否為落檔者
	Stream    bool   `json:"stream,omitempty"`    // 是否應上傳本地 update（init / stream）
	SendState bool   `json:"sendState,omitempty"` // 是否需立即送出完整狀態（單人→多人時補餵新加入者）
	ClientID  int64  `json:"clientId,omitempty"`  // Yjs awareness clientID（hello 登記 / peerLeft 通知移除）
	External  bool   `json:"external,omitempty"`  // 有未處理的外部改檔（init / role 告知接手的 saver 出橫幅）
}

// New 建立 collab Hub。
func New(a *auth.Auth, az *authz.Authz, cfg *config.Config) *Hub {
	return &Hub{
		auth: a,
		az:   az,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     func(r *http.Request) bool { return cfg.OriginAllowed(r.Header.Get("Origin")) },
		},
		rooms: map[string]*room{},
	}
}

func frame(tag byte, payload []byte) []byte {
	b := make([]byte, 1+len(payload))
	b[0] = tag
	copy(b[1:], payload)
	return b
}

// ctrlFrame 把控制訊息序列化為 tagControl 前綴的 frame。
func ctrlFrame(m controlMsg) []byte {
	b, _ := json.Marshal(m)
	return frame(tagControl, b)
}

// ServeWs 處理 GET /ws/collab?path=xxx&token=yyy：驗證 token 與讀取權後升級並加入房間。
func (h *Hub) ServeWs(c *gin.Context) {
	tokenStr := c.Query("token")
	path := c.Query("path")
	if tokenStr == "" || path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 token 或 path"})
		return
	}
	claims, err := h.auth.ParseJWT(tokenStr)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "token 無效或已過期"})
		return
	}
	subject := auth.SubjectFromClaims(claims)
	if h.az != nil && !h.az.Can(subject, path, authz.AccessRead) {
		c.JSON(http.StatusForbidden, gin.H{"error": "權限不足"})
		return
	}
	canWrite := h.az == nil || h.az.Can(subject, path, authz.AccessWrite)

	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	cl := &client{
		hub: h, conn: conn, send: make(chan []byte, sendBuffer),
		username: claims.Username, subject: subject, canWrite: canWrite,
	}

	// 加入房間並取得初始控制訊息與待回放的 update。回放在 writePump 啟動前同步送出，
	// 避免大量回放塞爆 send 緩衝（update 數可能超過 sendBuffer）。
	initMsg, replay := h.addClient(path, cl)
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	_ = conn.WriteMessage(websocket.BinaryMessage, frame(tagControl, initMsg))
	for _, u := range replay {
		_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
		if err := conn.WriteMessage(websocket.BinaryMessage, frame(tagUpdate, u)); err != nil {
			h.removeClient(cl)
			conn.Close()
			return
		}
	}

	go cl.writePump()
	go cl.readPump()
}

// addClient 把連線加入房間、指派 seed/saver 角色,回傳 init 控制訊息與待回放的 update 快照。
func (h *Hub) addClient(path string, cl *client) (initMsg []byte, replay [][]byte) {
	h.mu.Lock()
	defer h.mu.Unlock()

	r := h.rooms[path]
	if r == nil {
		r = &room{path: path, clients: map[*client]bool{}, compactMark: -1}
		h.rooms[path] = r
	}
	cl.room = r
	r.clients[cl] = true

	// 第一個可寫者成為 saver；房間尚未 seed 時，請它用 .md 內容初始化文件
	seed := false
	if cl.canWrite && r.saver == nil {
		r.saver = cl
		seed = !r.seeded
	}

	// 串流旗標（最佳化 B）：房內僅一人時不需上傳 update（本機累積即可）。
	// 由單人變為兩人時，通知原本獨自在房者開始串流，並請 saver 補送一次完整狀態給新加入者。
	stream := len(r.clients) >= 2
	cl.streaming = stream
	if len(r.clients) == 2 {
		for c := range r.clients {
			if c != cl {
				c.streaming = true
				c.trySend(ctrlFrame(controlMsg{Type: "stream", Stream: true, SendState: c == r.saver}))
			}
		}
	}

	// 接手為 saver 且房間有未處理的外部改檔（前一個 saver 離開時尚未決定）→ 一併告知，讓新 saver 出橫幅。
	initMsg, _ = json.Marshal(controlMsg{Type: "init", Seed: seed, CanWrite: cl.canWrite, Saver: r.saver == cl, Stream: stream, External: r.saver == cl && r.extPending})
	replay = make([][]byte, len(r.log))
	copy(replay, r.log)
	return initMsg, replay
}

// RoomPaths 回傳目前有活躍共編房間的文件路徑。供 filewatch 決定要輪詢哪些檔。
func (h *Hub) RoomPaths() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.rooms))
	for p := range h.rooms {
		out = append(out, p)
	}
	return out
}

// NotifyExternalChange 通知指定文件的共編房間：磁碟上的 .md 被外部修改。
// 房內客戶端收到後，由落檔者暫停自動落檔並出橫幅，讓使用者選擇保留共編版本或改用磁碟版本，
// 避免 saver 下次落檔靜默覆蓋外部變更。無對應房間時為 no-op。
func (h *Hub) NotifyExternalChange(path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[path]
	if r == nil {
		return
	}
	// 記住「有未處理的外部改檔」：若 saver 在決定前離開，移交時告知新 saver（見 removeClient / addClient）。
	r.extPending = true
	f := ctrlFrame(controlMsg{Type: "external"})
	for c := range r.clients {
		c.trySend(f)
	}
}

// handleFrame 處理客戶端送來的一個 frame。
func (h *Hub) handleFrame(cl *client, data []byte) {
	if len(data) < 1 {
		return
	}
	tag, payload := data[0], data[1:]

	h.mu.Lock()
	defer h.mu.Unlock()
	r := cl.room
	if r == nil {
		return
	}
	switch tag {
	case tagUpdate:
		if !cl.canWrite {
			return // 唯讀者不得修改文件
		}
		r.log = append(r.log, append([]byte(nil), payload...))
		r.seeded = true
		h.broadcastLocked(r, cl, frame(tagUpdate, payload))
		h.maybeCompactLocked(r) // log 過長時請 saver 送完整狀態壓縮（最佳化 A）
	case tagState:
		// saver 回應壓縮請求送來的完整狀態：以快照取代壓縮點之前的 log，保留其後到達的 tail
		// （避免遺失請求期間新進的 update）；快照僅供 log / 晚加入回放，不需廣播（他人已同步）。
		if cl != r.saver {
			return
		}
		mark := r.compactMark
		if mark < 0 || mark > len(r.log) {
			mark = len(r.log)
		}
		tail := append([][]byte{}, r.log[mark:]...)
		r.log = append([][]byte{append([]byte(nil), payload...)}, tail...)
		r.seeded = true
		r.compactMark = -1
	case tagAwareness:
		// awareness 不需保存（即時狀態），僅轉發
		h.broadcastLocked(r, cl, frame(tagAwareness, payload))
	case tagControl:
		var m controlMsg
		if json.Unmarshal(payload, &m) != nil {
			return
		}
		switch m.Type {
		case "hello":
			// 登記自己的 Yjs clientID，供離線時通知他人移除其殘留游標。
			cl.yjsID = m.ClientID
			cl.hasYjsID = true
		case "extResolved":
			// saver 已決定如何處理外部改檔（保留共編 / 改用磁碟）→ 清除待處理旗標，
			// 之後的 saver 移交不再重複出橫幅。僅採信目前的 saver。
			if cl == r.saver {
				r.extPending = false
			}
		}
	}
}

// maybeCompactLocked 在 log 累積過長時，請 saver 送出完整狀態以取代 log（最佳化 A：記憶體有界）。
// 須持有 h.mu。記錄請求當下的 log 長度（compactMark），供快照到達時保留其後新進的 update。
func (h *Hub) maybeCompactLocked(r *room) {
	if r.saver == nil || r.compactMark >= 0 || len(r.log) < compactThreshold {
		return
	}
	r.compactMark = len(r.log)
	r.saver.trySend(ctrlFrame(controlMsg{Type: "compact"}))
}

// broadcastLocked 把 frame 送給房間內除 sender 外的所有連線。呼叫前須持有 h.mu。
func (h *Hub) broadcastLocked(r *room, sender *client, f []byte) {
	for c := range r.clients {
		if c != sender {
			c.trySend(f)
		}
	}
}

// removeClient 連線離開:移出房間,必要時移交 saver 或回收空房。
func (h *Hub) removeClient(cl *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := cl.room
	if r == nil {
		return
	}
	delete(r.clients, cl)
	close(cl.send)
	cl.room = nil

	if len(r.clients) == 0 {
		delete(h.rooms, r.path) // 空房回收:下次開檔重新從 .md 種子
		return
	}

	// 通知房內其他人移除這位離線者的 awareness（游標 / 選取）。
	// 伺服器為 dumb relay，不解 CRDT，但連線關閉是確定性事件：用登記的 Yjs clientID
	// 廣播 peerLeft，存活端據此即時清掉殘留游標，避免快速 F5 在 awareness 30s 逾時前堆積 ghost。
	if cl.hasYjsID {
		h.broadcastLocked(r, cl, ctrlFrame(controlMsg{Type: "peerLeft", ClientID: cl.yjsID}))
	}
	// saver 離開 → 從剩餘可寫者選新 saver(尚未 seed 時請它 seed)；
	// 舊 saver 的壓縮請求已無人回應，重置 compactMark 讓未來仍能再次壓縮。
	if r.saver == cl {
		r.saver = nil
		r.compactMark = -1
		for c := range r.clients {
			if c.canWrite {
				r.saver = c
				// 帶上 extPending：前一個 saver 在決定如何處理外部改檔前離開時，由新 saver 接手出橫幅，
				// 避免新 saver 不知情而以共編內容靜默覆蓋外部變更。
				c.trySend(ctrlFrame(controlMsg{Type: "role", Saver: true, Seed: !r.seeded, External: r.extPending}))
				break
			}
		}
	}
	// 回到單人（最佳化 B）：通知剩餘者停止串流（改回本機累積，不再上傳）。
	if len(r.clients) == 1 {
		for c := range r.clients {
			c.streaming = false
			c.trySend(ctrlFrame(controlMsg{Type: "stream", Stream: false}))
		}
	}
}

// trySend 非阻塞送出;緩衝滿視為異常連線,關閉其 conn(由 pump 收尾觸發 removeClient)。須持有 h.mu。
func (c *client) trySend(data []byte) {
	select {
	case c.send <- data:
	default:
		c.conn.Close()
	}
}

func (c *client) readPump() {
	defer func() {
		c.hub.removeClient(c)
		c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	for {
		mt, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		if mt == websocket.BinaryMessage {
			c.hub.handleFrame(c, data)
		}
	}
}

func (c *client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.BinaryMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
