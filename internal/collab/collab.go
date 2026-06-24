// Package collab 提供即時共編的 WebSocket 房間層（M1）：以文件路徑分房、權限 join、
// 中繼 Yjs 二進位 update 與 awareness，並為晚加入者回放既有 update（dumb relay：Go 端不解碼 CRDT）。
//
// 訊息格式:每個 WebSocket 二進位 frame 第 1 個位元組為 tag,其後為負載:
//   'u' 文件 update(Yjs)  'a' awareness(游標等)  'c' 控制訊息(JSON)
// 控制訊息僅由伺服器送出(init / role)。落地 .md 由被指派為 saver 的客戶端走既有 POST /api/file。
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
	tagControl   = 'c' // 控制訊息（JSON）

	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 1 << 20 // 1 MB：Yjs update 可能較大
	sendBuffer     = 256
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
	path    string
	clients map[*client]bool
	log     [][]byte // 已廣播的文件 update（供晚加入者回放）；空房回收時清除
	seeded  bool     // 是否已由某客戶端用 .md 內容初始化
	saver   *client  // 負責落檔的客戶端（可寫者其一）
}

type client struct {
	hub      *Hub
	conn     *websocket.Conn
	send     chan []byte
	username string
	subject  string
	canWrite bool
	room     *room
}

// controlMsg 為伺服器→客戶端的控制訊息（以 tagControl 前綴的 JSON）。
type controlMsg struct {
	Type     string `json:"type"`               // "init" | "role"
	Seed     bool   `json:"seed,omitempty"`     // 是否由你以 .md 內容初始化文件
	CanWrite bool   `json:"canWrite,omitempty"` // 你是否可寫（init）
	Saver    bool   `json:"saver,omitempty"`    // 你是否為落檔者
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
		r = &room{path: path, clients: map[*client]bool{}}
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
	initMsg, _ = json.Marshal(controlMsg{Type: "init", Seed: seed, CanWrite: cl.canWrite, Saver: r.saver == cl})

	replay = make([][]byte, len(r.log))
	copy(replay, r.log)
	return initMsg, replay
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
	case tagAwareness:
		// awareness 不需保存（即時狀態），僅轉發
		h.broadcastLocked(r, cl, frame(tagAwareness, payload))
	}
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
	// saver 離開 → 從剩餘可寫者選新 saver(尚未 seed 時請它 seed)
	if r.saver == cl {
		r.saver = nil
		for c := range r.clients {
			if c.canWrite {
				r.saver = c
				msg, _ := json.Marshal(controlMsg{Type: "role", Saver: true, Seed: !r.seeded})
				c.trySend(frame(tagControl, msg))
				break
			}
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
