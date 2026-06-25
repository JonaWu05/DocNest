// Package hub 管理所有 WebSocket 連線：presence（誰在看/編輯）與 file_updated 即時通知。
package hub

import (
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"markdownEditor/internal/auth"
	"markdownEditor/internal/authz"
	"markdownEditor/internal/config"
)

// ===== WebSocket 連線參數 =====
const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 1 << 16
	sendBuffer     = 256
)

// ===== 統一訊息格式：{ "type": ..., "payload": ... } =====

type wsMessage struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

// PresenceUser 為 presence_update 中的單一使用者狀態
type PresenceUser struct {
	Username    string `json:"username"`
	CurrentFile string `json:"current_file"`
	IsEditing   bool   `json:"is_editing"`
}

type presencePayload struct {
	Users []PresenceUser `json:"users"`
}

// fileUpdatedPayload 僅送「通知」（哪個檔被誰存了），不夾帶內容。
type fileUpdatedPayload struct {
	Path    string `json:"path"`
	SavedBy string `json:"saved_by"`
}

// outbound 為一則待送出的訊息；path 非空時僅投遞給對該路徑具讀取權的連線，
// 避免廣播洩漏無權限使用者看不到的檔案路徑（與 authz 的 path-prefix ACL 一致）。
type outbound struct {
	data []byte
	path string
}

// Client 代表單一 WebSocket 連線。
type Client struct {
	hub         *Hub
	conn        *websocket.Conn
	send        chan []byte
	username    string // 顯示用
	subject     string // 穩定身分鍵，presence 去重用
	currentFile string
	isEditing   bool
}

// presenceUpdate 為「更新某連線狀態」的請求，交由 hub goroutine 套用以確保 thread-safe。
type presenceUpdate struct {
	client      *Client
	currentFile string
	isEditing   bool
}

// Hub 管理所有連線。clients map 與所有 Client 欄位的讀寫只在 Run() 的單一 goroutine 內進行，
// 因此不需 mutex；其他 goroutine 一律透過 channel 與 hub 溝通。
type Hub struct {
	auth     *auth.Auth
	az       *authz.Authz
	upgrader websocket.Upgrader

	clients      map[*Client]bool
	broadcast    chan outbound
	register     chan *Client
	unregister   chan *Client
	presence     chan presenceUpdate
	openPathsReq chan chan []string // 查詢目前開檔清單（在 Run goroutine 內讀 clients，維持單 goroutine 紀律）
	count        atomic.Int64

	// quit 由 Close() 關閉，通知 Run 結束並讓各 goroutine 的 channel 送出可解除阻塞。
	quit      chan struct{}
	closeOnce sync.Once
}

// New 建立 Hub；需要 auth（serveWs 驗證 token）、az（廣播時依讀取權過濾路徑）與 cfg（WebSocket Origin 檢查）。
func New(a *auth.Auth, az *authz.Authz, cfg *config.Config) *Hub {
	return &Hub{
		auth: a,
		az:   az,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			// 與 CORS 共用 ALLOWED_ORIGINS，防止跨站 WebSocket 連線（CSWSH）。
			CheckOrigin: func(r *http.Request) bool { return cfg.OriginAllowed(r.Header.Get("Origin")) },
		},
		clients:      make(map[*Client]bool),
		broadcast:    make(chan outbound, sendBuffer),
		register:     make(chan *Client),
		unregister:   make(chan *Client),
		presence:     make(chan presenceUpdate),
		openPathsReq: make(chan chan []string),
		quit:         make(chan struct{}),
	}
}

// Close 啟動 Hub 的優雅關閉：通知 Run 結束並關閉所有 WebSocket 連線。
// 可安全重複呼叫。供伺服器收到結束訊號時呼叫（補 http.Server.Shutdown 不收尾被劫持的 WS 連線之缺口）。
func (h *Hub) Close() {
	h.closeOnce.Do(func() { close(h.quit) })
}

// canRead 判斷某連線是否對指定路徑具讀取權。az 未注入（如測試）或路徑為空時一律放行。
func (h *Hub) canRead(subject, path string) bool {
	if h.az == nil || path == "" {
		return true
	}
	return h.az.Can(subject, path, authz.AccessRead)
}

// Run 為 Hub 的單一事件迴圈，所有對 clients / Client 欄位的操作都集中在此。
func (h *Hub) Run() {
	for {
		select {
		case c := <-h.register:
			h.clients[c] = true
			h.count.Store(int64(len(h.clients)))
			h.broadcastPresence()

		case c := <-h.unregister:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				close(c.send)
				h.count.Store(int64(len(h.clients)))
				h.broadcastPresence()
			}

		case p := <-h.presence:
			if _, ok := h.clients[p.client]; ok {
				p.client.currentFile = p.currentFile
				p.client.isEditing = p.isEditing
				h.broadcastPresence()
			}

		case o := <-h.broadcast:
			h.deliver(o)

		case ch := <-h.openPathsReq:
			ch <- h.currentOpenPaths()

		case <-h.quit:
			// 關閉所有連線：close(send) 會讓 writePump 送出 Close frame 後結束，
			// 連帶讓 readPump 讀取出錯而退出。之後 Run 結束，不再處理任何訊息。
			for c := range h.clients {
				delete(h.clients, c)
				close(c.send)
			}
			h.count.Store(0)
			return
		}
	}
}

// deliver 把一則「已序列化」訊息送給所有連線；緩衝滿者視為異常連線並移除。
// o.path 非空時，僅送給對該路徑具讀取權的連線。
func (h *Hub) deliver(o outbound) {
	for c := range h.clients {
		if !h.canRead(c.subject, o.path) {
			continue
		}
		h.sendTo(c, o.data)
	}
	h.count.Store(int64(len(h.clients)))
}

// sendTo 將訊息送給單一連線；緩衝滿者視為異常連線並移除。須在 hub goroutine 內呼叫。
func (h *Hub) sendTo(c *Client, data []byte) {
	select {
	case c.send <- data:
	default:
		delete(h.clients, c)
		close(c.send)
	}
}

// broadcastPresence 蒐集目前所有連線的狀態，組成 presence_update 廣播給所有人。
//
// 以穩定身分鍵（subject）去重：同一使用者開多個分頁／多條連線時只算一人，
// 避免 presence 清單出現重複的自己（同一人多連線是正常情況，例如 OAuth 整頁跳轉的瞬間重疊）。
func (h *Hub) broadcastPresence() {
	bySubject := make(map[string]*PresenceUser, len(h.clients))
	order := make([]string, 0, len(h.clients))

	for c := range h.clients {
		key := c.subject
		if key == "" {
			key = c.username
		}
		if u, ok := bySubject[key]; ok {
			// 合併同一人多條連線的狀態：任一條在編輯即視為編輯中，並採用有開檔那條的檔案路徑
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

	// 每位收件者收到的清單可能不同：對方無讀取權的 current_file 會被遮去（連帶 is_editing），
	// 避免洩漏「某人正在編輯你看不到的檔案」這類路徑資訊。仍保留該使用者「在線」的事實。
	for c := range h.clients {
		view := make([]PresenceUser, len(users))
		for i, u := range users {
			if u.CurrentFile != "" && !h.canRead(c.subject, u.CurrentFile) {
				u.CurrentFile = ""
				u.IsEditing = false
			}
			view[i] = u
		}
		msg, err := json.Marshal(wsMessage{Type: "presence_update", Payload: presencePayload{Users: view}})
		if err != nil {
			return
		}
		h.sendTo(c, msg)
	}
	h.count.Store(int64(len(h.clients)))
}

// currentOpenPaths 蒐集目前所有連線開著的不同檔案路徑（current_file）。僅在 Run goroutine 內呼叫。
func (h *Hub) currentOpenPaths() []string {
	set := make(map[string]bool, len(h.clients))
	for c := range h.clients {
		if c.currentFile != "" {
			set[c.currentFile] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	return out
}

// OpenPaths 回傳目前有人開著的檔案路徑（去重）。供 filewatch 決定要輪詢哪些檔。
// 經 openPathsReq channel 交由 Run goroutine 讀取 clients，維持「clients 只在單一 goroutine 存取」紀律。
func (h *Hub) OpenPaths() []string {
	ch := make(chan []string, 1)
	select {
	case h.openPathsReq <- ch:
		select {
		case v := <-ch:
			return v
		case <-h.quit:
			return nil
		}
	case <-h.quit:
		return nil
	}
}

// BroadcastFileUpdated 由檔案儲存流程呼叫，廣播「某檔被某人更新」的通知給所有人（不含內容）。
func (h *Hub) BroadcastFileUpdated(path, savedBy string) {
	msg, err := json.Marshal(wsMessage{
		Type:    "file_updated",
		Payload: fileUpdatedPayload{Path: path, SavedBy: savedBy},
	})
	if err != nil {
		return
	}
	// 帶上 path：deliver 只會送給對該檔具讀取權的連線。關閉中則放棄。
	select {
	case h.broadcast <- outbound{data: msg, path: path}:
	case <-h.quit:
	}
}

// ServeWs 處理 GET /ws：先用 query 參數的 token 做 JWT 驗證，再升級為 WebSocket。
func (h *Hub) ServeWs(c *gin.Context) {
	tokenStr := c.Query("token")
	if tokenStr == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "缺少 token"})
		return
	}
	claims, err := h.auth.ParseJWT(tokenStr)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "token 無效或已過期"})
		return
	}

	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}

	// username / subject 一律取自 JWT，不採信前端。
	client := &Client{
		hub:      h,
		conn:     conn,
		send:     make(chan []byte, sendBuffer),
		username: claims.Username,
		subject:  auth.SubjectFromClaims(claims),
	}
	// Hub 已開始關閉時不再接收新連線（避免 register 永久阻塞拖住此 handler）。
	select {
	case h.register <- client:
	case <-h.quit:
		conn.Close()
		return
	}

	go client.writePump()
	go client.readPump()
}

// OnlineCountHandler 處理 GET /api/online-count：回傳目前連線數（原子讀取，不碰 map）。
func (h *Hub) OnlineCountHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"count": h.count.Load()})
}

// readPump 持續從連線讀取訊息。目前只處理 client_presence（更新自己的狀態）。
func (c *Client) readPump() {
	defer func() {
		// Hub 關閉後 Run 已結束，unregister 不再有人接收：以 quit 解除阻塞，避免 goroutine 卡死。
		select {
		case c.hub.unregister <- c:
		case <-c.hub.quit:
		}
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
			break
		}

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
				select {
				case c.hub.presence <- presenceUpdate{client: c, currentFile: p.CurrentFile, isEditing: p.IsEditing}:
				case <-c.hub.quit:
					return
				}
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
