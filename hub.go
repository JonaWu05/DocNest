package main

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// ===== WebSocket 連線參數 =====
const (
	writeWait      = 10 * time.Second    // 單次寫入逾時
	pongWait       = 60 * time.Second    // 等待對方 pong 的最長時間
	pingPeriod     = (pongWait * 9) / 10 // 主動 ping 的間隔（需小於 pongWait）
	maxMessageSize = 1 << 16             // 接收訊息大小上限（client_presence 很小，64KB 綽綽有餘）
	sendBuffer     = 256                 // 每個連線的送出緩衝
)

// ===== 統一訊息格式：{ "type": ..., "payload": ... } =====

// WSMessage 為對外送出的訊息外層（payload 可為任意可序列化結構）
type WSMessage struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

// PresenceUser 為 presence_update 中的單一使用者狀態
type PresenceUser struct {
	Username    string `json:"username"`
	CurrentFile string `json:"current_file"`
	IsEditing   bool   `json:"is_editing"`
}

// presencePayload 為 presence_update 的 payload
type presencePayload struct {
	Users []PresenceUser `json:"users"`
}

// fileUpdatedPayload 為 file_updated 的 payload。
// 僅送「通知」（哪個檔被誰存了），不夾帶內容：避免把整份檔案廣播給沒開該檔的所有連線。
// 需要最新內容的 client（正開著該檔者）再自行向 /api/file 取回。
type fileUpdatedPayload struct {
	Path    string `json:"path"`
	SavedBy string `json:"saved_by"`
}

// ===== Client：代表單一 WebSocket 連線 =====
type Client struct {
	conn        *websocket.Conn
	send        chan []byte // 緩衝 channel，writePump 由此取出訊息寫到連線
	username    string      // 由 JWT 取出，不信任前端傳來的身份（顯示用）
	subject     string      // 穩定身分鍵（local:/discord:），presence 去重用
	currentFile string      // 目前查看的檔案路徑（可為空）
	isEditing   bool        // 是否處於編輯模式
}

// presenceUpdate 為「更新某連線狀態」的請求，交由 hub goroutine 套用以確保 thread-safe
type presenceUpdate struct {
	client      *Client
	currentFile string
	isEditing   bool
}

// ===== Hub：管理所有連線 =====
// 設計重點：clients map 與所有 Client 欄位的讀寫「只在 run() 的單一 goroutine 內進行」，
// 因此完全不需要 mutex；其他 goroutine 一律透過 channel 與 hub 溝通。
type Hub struct {
	clients    map[*Client]bool
	broadcast  chan []byte          // 對所有人廣播的「已序列化」訊息
	register   chan *Client         // 新連線註冊
	unregister chan *Client         // 連線移除
	presence   chan presenceUpdate  // 更新某連線的 presence 狀態（在 hub goroutine 內改 Client 欄位）
	count      atomic.Int64         // 線上人數（供 /api/online-count 原子讀取，不碰 map）
}

// hub 為全域單例
var hub *Hub

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte, sendBuffer),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		presence:   make(chan presenceUpdate),
	}
}

// run 為 Hub 的單一事件迴圈，所有對 clients / Client 欄位的操作都集中在此。
func (h *Hub) run() {
	for {
		select {
		case c := <-h.register:
			h.clients[c] = true
			h.count.Store(int64(len(h.clients)))
			// 新連線加入後，立即廣播一次最新的 presence
			h.broadcastPresence()

		case c := <-h.unregister:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				close(c.send)
				h.count.Store(int64(len(h.clients)))
				h.broadcastPresence()
			}

		case p := <-h.presence:
			// 只有仍在線的連線才更新（避免處理已斷線者）
			if _, ok := h.clients[p.client]; ok {
				p.client.currentFile = p.currentFile
				p.client.isEditing = p.isEditing
				h.broadcastPresence()
			}

		case msg := <-h.broadcast:
			h.deliver(msg)
		}
	}
}

// deliver 把一則「已序列化」訊息送給所有連線；
// 若某連線的 send 緩衝已滿，視為異常連線，主動關閉並移除（符合規格要求）。
func (h *Hub) deliver(msg []byte) {
	for c := range h.clients {
		select {
		case c.send <- msg:
		default:
			delete(h.clients, c)
			close(c.send) // 關閉後其 writePump 會結束、readPump 隨後送 unregister（會因已移除而無動作）
		}
	}
	h.count.Store(int64(len(h.clients)))
}

// broadcastPresence 蒐集目前所有連線的狀態，組成 presence_update 廣播給所有人。
// 注意：本函式只在 hub goroutine 內被呼叫，故可安全讀取 clients 與 Client 欄位。
//
// 以穩定身分鍵（subject）去重：同一使用者開多個分頁／多條連線時只算一人，
// 避免 presence 清單出現重複的自己（同一人多連線是正常情況，例如 OAuth 整頁跳轉的瞬間重疊）。
func (h *Hub) broadcastPresence() {
	bySubject := make(map[string]*PresenceUser, len(h.clients))
	order := make([]string, 0, len(h.clients)) // 保留首見順序，輸出較穩定

	for c := range h.clients {
		key := c.subject
		if key == "" {
			key = c.username // 後備：舊連線未帶 subject 時以 username 當鍵
		}
		if u, ok := bySubject[key]; ok {
			// 合併同一人多條連線的狀態：任一條在編輯即視為編輯中，並採用有開檔的那條的檔案路徑
			if c.isEditing {
				u.IsEditing = true
				u.CurrentFile = c.currentFile
			} else if u.CurrentFile == "" {
				u.CurrentFile = c.currentFile
			}
			continue
		}
		bySubject[key] = &PresenceUser{
			Username:    c.username,
			CurrentFile: c.currentFile,
			IsEditing:   c.isEditing,
		}
		order = append(order, key)
	}

	users := make([]PresenceUser, 0, len(order))
	for _, k := range order {
		users = append(users, *bySubject[k])
	}

	msg, err := json.Marshal(WSMessage{Type: "presence_update", Payload: presencePayload{Users: users}})
	if err != nil {
		return
	}
	h.deliver(msg)
}

// broadcastFileUpdated 由檔案儲存流程呼叫，廣播「某檔被某人更新」的通知給所有人（不含內容）。
func broadcastFileUpdated(path, savedBy string) {
	if hub == nil {
		return
	}
	msg, err := json.Marshal(WSMessage{
		Type:    "file_updated",
		Payload: fileUpdatedPayload{Path: path, SavedBy: savedBy},
	})
	if err != nil {
		return
	}
	// broadcast 為緩衝 channel，這裡直接送入；交由 hub goroutine 派發
	hub.broadcast <- msg
}

// ===== WebSocket 連線升級 =====
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// 與 CORS 共用 ALLOWED_ORIGINS：有設定就比對 Origin，未設定則開發模式全部放行。
	// 防止惡意網站從使用者瀏覽器發起跨站 WebSocket 連線（CSWSH）。
	CheckOrigin: func(r *http.Request) bool { return isOriginAllowed(r.Header.Get("Origin")) },
}

// serveWs 處理 GET /ws：先用 query 參數的 token 做 JWT 驗證，再升級為 WebSocket。
// 為何用 query token：瀏覽器的 WebSocket API 無法自訂請求標頭，無法帶 Authorization，
// 因此改以 ?token=xxx 傳遞 JWT —— 這是 WebSocket 驗證的標準做法。
func serveWs(c *gin.Context) {
	tokenStr := c.Query("token")
	if tokenStr == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "缺少 token"})
		return
	}
	claims, err := parseJWT(tokenStr)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "token 無效或已過期"})
		return
	}

	// 升級連線（失敗時 gorilla 會自行寫入錯誤回應）
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}

	// username / subject 一律取自 JWT，不採信前端。subject 為 presence 去重的穩定身分鍵；
	// 舊 token 無 sub 時退而以 login_type:username 推導（與 AuthMiddleware 一致）。
	subject := claims.Subject
	if subject == "" {
		subject = claims.LoginType + ":" + claims.Username
	}
	client := &Client{
		conn:     conn,
		send:     make(chan []byte, sendBuffer),
		username: claims.Username,
		subject:  subject,
	}
	hub.register <- client

	// 每個連線各開一個讀、一個寫 goroutine
	go client.writePump()
	go client.readPump()
}

// readPump 持續從連線讀取訊息。目前只處理 client_presence（更新自己的狀態）。
func (c *Client) readPump() {
	defer func() {
		hub.unregister <- c
		c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			break // 連線關閉或錯誤，結束讀取
		}

		// 解析外層訊息格式
		var env struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(data, &env) != nil {
			continue
		}

		if env.Type == "client_presence" {
			var p struct {
				CurrentFile string `json:"current_file"`
				IsEditing   bool   `json:"is_editing"`
			}
			if json.Unmarshal(env.Payload, &p) == nil {
				// 交由 hub goroutine 套用狀態變更並廣播
				hub.presence <- presenceUpdate{client: c, currentFile: p.CurrentFile, isEditing: p.IsEditing}
			}
		}
	}
}

// writePump 從 send channel 取出訊息寫到連線，並定期送 ping 維持連線。
func (c *Client) writePump() {
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
				// send 已被 hub 關閉，通知對方關閉連線
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
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

// onlineCountHandler 處理 GET /api/online-count：回傳目前連線數（原子讀取，不碰 map）。
func onlineCountHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"count": hub.count.Load()})
}
