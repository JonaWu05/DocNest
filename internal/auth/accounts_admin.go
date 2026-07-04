package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/JonaWu05/DocNest/internal/store"
)

// maxUsernameLen 為本地帳號名稱長度上限（避免異常長的輸入）。
const maxUsernameLen = 64

// ValidateUsername 檢查本地帳號名稱是否合法：非空、長度合理、且不含冒號
// （冒號用於身分鍵命名空間 local:/discord:，帳號名含冒號會破壞比對）。
func ValidateUsername(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("帳號名稱不可為空")
	}
	if len(name) > maxUsernameLen {
		return fmt.Errorf("帳號名稱過長（上限 %d 字元）", maxUsernameLen)
	}
	if strings.ContainsAny(name, ":") {
		return fmt.Errorf("帳號名稱不可含冒號")
	}
	return nil
}

// HashPassword 把明文密碼轉成 bcrypt hash（CPU 密集，刻意在鎖外呼叫）。
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", fmt.Errorf("密碼不可為空")
	}
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// mutate 以 read-modify-write 更新 accounts.json：讀檔 → fn 修改 → 原子寫回 → 就地重載快照。
// 整個過程持寫鎖以序列化並發變動；accounts.json 含密碼 hash，故以 0o600 權限寫入。
func (a *Accounts) mutate(fn func(*accountsFile) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	var f accountsFile
	data, err := os.ReadFile(a.path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// 檔案不存在：從空白開始（讓首個帳號建立能自動產生檔案）
	} else if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	if f.Local == nil {
		f.Local = map[string]string{}
	}

	if err := fn(&f); err != nil {
		return err
	}

	out, err := json.MarshalIndent(&f, "", "  ")
	if err != nil {
		return err
	}
	if err := store.AtomicWrite(a.path, out, 0o600, false); err != nil {
		return err
	}
	s, err := parseAccountsData(out)
	if err != nil {
		return err
	}
	a.snap = s
	return nil
}

// CreateUser 新增一個本地帳號（帳號已存在則回錯）。hash 須為呼叫端先算好的 bcrypt hash。
func (a *Accounts) CreateUser(username, hash string) error {
	if err := ValidateUsername(username); err != nil {
		return err
	}
	return a.mutate(func(f *accountsFile) error {
		if _, ok := f.Local[username]; ok {
			return fmt.Errorf("帳號已存在：%s", username)
		}
		f.Local[username] = hash
		return nil
	})
}

// SetPassword 重設既有本地帳號的密碼（帳號不存在則回錯）。hash 須為先算好的 bcrypt hash。
func (a *Accounts) SetPassword(username, hash string) error {
	return a.mutate(func(f *accountsFile) error {
		if _, ok := f.Local[username]; !ok {
			return fmt.Errorf("帳號不存在：%s", username)
		}
		f.Local[username] = hash
		return nil
	})
}

// DeleteUser 刪除一個本地帳號（帳號不存在則回錯）。
func (a *Accounts) DeleteUser(username string) error {
	return a.mutate(func(f *accountsFile) error {
		if _, ok := f.Local[username]; !ok {
			return fmt.Errorf("帳號不存在：%s", username)
		}
		delete(f.Local, username)
		return nil
	})
}

// AddDiscord 把一個 Discord User ID 加入登入白名單（冪等 upsert）。
// 已存在時：僅在有提供非空 label 時更新其備註，否則不動（不覆蓋既有備註）。
func (a *Accounts) AddDiscord(id, label string) error {
	id = strings.TrimSpace(id)
	label = strings.TrimSpace(label)
	if id == "" {
		return fmt.Errorf("Discord User ID 不可為空")
	}
	return a.mutate(func(f *accountsFile) error {
		for i := range f.DiscordAllowed {
			if strings.TrimSpace(f.DiscordAllowed[i].ID) == id {
				if label != "" {
					f.DiscordAllowed[i].Label = label
				}
				return nil // 已存在，冪等
			}
		}
		f.DiscordAllowed = append(f.DiscordAllowed, discordEntry{ID: id, Label: label})
		return nil
	})
}

// SetDiscordLabel 更新既有 Discord 白名單項目的顯示備註（ID 不存在則回錯）。
func (a *Accounts) SetDiscordLabel(id, label string) error {
	id = strings.TrimSpace(id)
	label = strings.TrimSpace(label)
	return a.mutate(func(f *accountsFile) error {
		for i := range f.DiscordAllowed {
			if strings.TrimSpace(f.DiscordAllowed[i].ID) == id {
				f.DiscordAllowed[i].Label = label
				return nil
			}
		}
		return fmt.Errorf("Discord ID 不存在：%s", id)
	})
}

// NoteDiscordLogin 於 Discord 登入通過白名單後呼叫：若該 ID 的備註仍為空，
// 自動補上其當下的 Discord 用戶名（手動備註優先，不覆蓋；已有備註則不寫檔）。
func (a *Accounts) NoteDiscordLogin(id, username string) error {
	id = strings.TrimSpace(id)
	username = strings.TrimSpace(username)
	if id == "" || username == "" {
		return nil
	}
	// 先讀當下快照：不在白名單或已有備註就不動（避免每次登入都寫檔）。
	a.mu.RLock()
	label, ok := a.snap.discordAllowed[id]
	a.mu.RUnlock()
	if !ok || label != "" {
		return nil
	}
	return a.mutate(func(f *accountsFile) error {
		for i := range f.DiscordAllowed {
			if strings.TrimSpace(f.DiscordAllowed[i].ID) == id && strings.TrimSpace(f.DiscordAllowed[i].Label) == "" {
				f.DiscordAllowed[i].Label = username
			}
		}
		return nil
	})
}

// RemoveDiscord 把一個 Discord User ID 移出登入白名單。
func (a *Accounts) RemoveDiscord(id string) error {
	id = strings.TrimSpace(id)
	return a.mutate(func(f *accountsFile) error {
		filtered := f.DiscordAllowed[:0:0]
		for _, existing := range f.DiscordAllowed {
			if strings.TrimSpace(existing.ID) != id {
				filtered = append(filtered, existing)
			}
		}
		f.DiscordAllowed = filtered
		return nil
	})
}

// LocalUsernames 回傳所有本地帳號名稱（供管理面板列出；不含密碼 hash）。
func (a *Accounts) LocalUsernames() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, 0, len(a.snap.users))
	for name := range a.snap.users {
		out = append(out, name)
	}
	return out
}

// HasUser 判斷本地帳號是否存在。
func (a *Accounts) HasUser(username string) bool {
	_, ok := a.Lookup(username)
	return ok
}
