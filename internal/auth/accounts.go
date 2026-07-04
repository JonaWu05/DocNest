package auth

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// accountsFile 對應 accounts.json 的整體結構：本地帳號與 Discord 登入白名單。
// 密碼一律以 bcrypt hash 儲存（不存明文）；Discord 白名單為允許登入的 User ID。
type accountsFile struct {
	Local          map[string]string `json:"local"`           // username -> bcrypt hash
	DiscordAllowed []string          `json:"discord_allowed"` // 允許登入的 Discord User ID
}

// accountsSnapshot 為一份「載入完成後即不可變」的帳號設定，供熱重載整份替換（swap 指標）。
type accountsSnapshot struct {
	users          map[string]string // username -> bcrypt hash
	discordAllowed map[string]bool   // Discord User ID -> true
}

// Accounts 保存一份可熱重載的帳號設定（本地帳號 + Discord 白名單）。
// 以 RWMutex 保護 snap 指標：查詢取當下快照，Reload / 變動只在替換指標的瞬間持寫鎖。
type Accounts struct {
	mu   sync.RWMutex
	snap *accountsSnapshot
	path string // 設定檔來源路徑（供變動時 read-modify-write）
}

// LoadAccounts 從設定檔建立 Accounts。
// 檔案不存在時以「空帳號」啟動並警告（無人可用本地登入、Discord 一律拒絕）；
// 待管理員放入 accounts.json 後由 Reload 即時生效。存在但解析失敗則回傳錯誤。
func LoadAccounts(path string) (*Accounts, error) {
	s, err := parseAccounts(path)
	if err != nil {
		return nil, err
	}
	return &Accounts{snap: s, path: path}, nil
}

// Reload 重新讀取設定檔並就地替換快照；供管理員手動觸發、免重啟。
// 解析失敗時回傳錯誤且不動既有設定（避免半形檔打斷登入）。
func (a *Accounts) Reload(path string) error {
	s, err := parseAccounts(path)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.snap = s
	a.path = path
	a.mu.Unlock()
	return nil
}

// parseAccounts 讀取設定檔並解析為一份不可變快照；檔案不存在時回傳空帳號快照並警告。
func parseAccounts(path string) (*accountsSnapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Warn("找不到帳號設定檔，以空帳號啟動（無法本地登入、Discord 一律拒絕）；放入設定檔後可經管理員重新載入生效", "path", path)
			return &accountsSnapshot{users: map[string]string{}, discordAllowed: map[string]bool{}}, nil
		}
		return nil, err
	}
	s, err := parseAccountsData(data)
	if err != nil {
		return nil, err
	}
	slog.Info("已載入帳號設定", "path", path, "local", len(s.users), "discord_allowed", len(s.discordAllowed))
	return s, nil
}

// parseAccountsData 將 accounts.json 內容（JSON bytes）解析為一份不可變快照。
// 與 parseAccounts 分離，讓變動寫回後可直接以新 bytes 重建快照，不必再讀一次磁碟。
func parseAccountsData(data []byte) (*accountsSnapshot, error) {
	var f accountsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}

	s := &accountsSnapshot{
		users:          make(map[string]string, len(f.Local)),
		discordAllowed: make(map[string]bool, len(f.DiscordAllowed)),
	}
	for name, hash := range f.Local {
		name = strings.TrimSpace(name)
		hash = strings.TrimSpace(hash)
		if name == "" {
			continue
		}
		// 安全性：一律要求 bcrypt hash（以 $2 開頭）；填入明文者忽略並警告。
		if !strings.HasPrefix(hash, "$2") {
			slog.Warn("使用者密碼不是 bcrypt hash，已忽略（請用 go run ./cmd/hashpw '密碼' 產生後填入 accounts.json）", "user", name)
			continue
		}
		s.users[name] = hash
	}
	for _, id := range f.DiscordAllowed {
		if id = strings.TrimSpace(id); id != "" {
			s.discordAllowed[id] = true
		}
	}
	return s, nil
}

// Lookup 依使用者名稱取出 bcrypt hash；ok 為 false 代表帳號不存在。
func (a *Accounts) Lookup(username string) (hash string, ok bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	hash, ok = a.snap.users[username]
	return
}

// IsDiscordAllowed 判斷某 Discord User ID 是否在登入白名單內。
func (a *Accounts) IsDiscordAllowed(id string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.snap.discordAllowed[id]
}
