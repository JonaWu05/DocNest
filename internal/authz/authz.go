// Package authz 實作以「群組 + 路徑前綴」為基礎的授權判斷。
// 純粹處理 (身分鍵, 相對路徑, 等級) 的決策，不涉及檔案系統或路徑換算。
package authz

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// 權限等級（由低到高）：none < read < write；write 隱含 read，
// 而 create / rename / delete 等修改動作一律歸類為 write。
const (
	AccessNone = iota
	AccessRead
	AccessWrite
)

// adminGroup 為「管理員」的群組名稱：此群組的具名成員可執行管理操作（如熱重載設定）。
// 沿用既有設定檔中的群組，不另立新概念；"*" 萬用成員不視為管理員（須為具名身分）。
const adminGroup = "admins"

// rule 為設定檔中的單一條規則。
type rule struct {
	Path   string `json:"path"`   // 相對 DOC_ROOT 的路徑前綴；"" 代表根（涵蓋全部）
	Access string `json:"access"` // "read" | "write" | "none"
}

// group 為一個權限群組：成員 + 規則。
type group struct {
	Members []string `json:"members"` // 身分鍵 local:/discord:；特殊值 "*" 代表所有已登入者
	Rules   []rule   `json:"rules"`
}

// fileConfig 對應 permissions.json 的整體結構。
type fileConfig struct {
	Default string           `json:"default"`
	Groups  map[string]group `json:"groups"`
}

// normRule 為正規化後的規則（路徑統一格式、等級轉成整數）。
type normRule struct {
	path   string
	access int
}

// snapshot 為一份「載入完成後即不可變」的權限設定。
// 熱重載時整份替換（swap 指標），使進行中的讀取仍看到一致的舊快照、不需在計算期間持鎖。
type snapshot struct {
	enabled        bool                  // 是否成功載入設定檔；否則為相容的「全開」模式
	defaultLevel   int                   // 預設權限等級
	rulesBySubject map[string][]normRule // subject -> 規則
	rulesEveryone  []normRule            // 萬用成員 "*" 的規則（套用到所有已登入者）
	admins         map[string]bool       // adminGroup 的具名成員（身分鍵 -> true）
}

// Authz 保存一份可熱重載的權限設定，提供查詢方法。取代原本的 package 級全域。
// 內部以 RWMutex 保護 snap 指標：讀取取當下快照後即釋放鎖，Reload 只在替換指標的瞬間持寫鎖。
type Authz struct {
	mu   sync.RWMutex
	snap *snapshot
}

// Load 從設定檔建立 Authz。
// 檔案不存在時不啟用權限分組（相容舊部署：全開，並印出警告）；
// 檔案存在但解析失敗則回傳錯誤，由呼叫端決定是否中止啟動。
func Load(path string) (*Authz, error) {
	s, err := parse(path)
	if err != nil {
		return nil, err
	}
	return &Authz{snap: s}, nil
}

// Reload 重新讀取設定檔並就地替換快照；供管理員手動觸發、免重啟。
// 解析失敗時回傳錯誤且不動既有設定（呼叫端應保留原設定並回報錯誤，避免半形檔打斷服務）。
func (a *Authz) Reload(path string) error {
	s, err := parse(path)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.snap = s
	a.mu.Unlock()
	return nil
}

// parse 讀取並解析設定檔為一份不可變快照。Load 與 Reload 共用。
func parse(path string) (*snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Warn("找不到權限設定檔，未啟用權限分組（所有登入者可存取全部檔案，僅適用於開發或單一信任群組）", "path", path)
			return &snapshot{enabled: false}, nil
		}
		return nil, err
	}

	var cfg fileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	s := &snapshot{
		enabled:        true,
		defaultLevel:   accessLevel(cfg.Default),
		rulesBySubject: map[string][]normRule{},
		admins:         map[string]bool{},
	}
	for name, g := range cfg.Groups {
		rules := make([]normRule, 0, len(g.Rules))
		for _, r := range g.Rules {
			rules = append(rules, normRule{path: normPath(r.Path), access: accessLevel(r.Access)})
		}
		for _, m := range g.Members {
			if m = strings.TrimSpace(m); m == "" {
				continue
			}
			if m == "*" {
				s.rulesEveryone = append(s.rulesEveryone, rules...)
				continue // 萬用成員不計入具名的管理員名單
			}
			s.rulesBySubject[m] = append(s.rulesBySubject[m], rules...)
			if name == adminGroup {
				s.admins[m] = true
			}
		}
	}
	slog.Info("已載入權限設定", "path", path, "groups", len(cfg.Groups), "default", strings.ToLower(strings.TrimSpace(cfg.Default)), "admins", len(s.admins))
	return s, nil
}

// current 取出當下的權限快照（取後即可脫離鎖使用，因為快照載入後不再變動）。
func (a *Authz) current() *snapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.snap
}

// effectiveAccess 回傳 subject 對 relPath 的有效權限等級：
// 以預設等級為基準，取所有命中規則（含萬用規則）中的最大值。
func (a *Authz) effectiveAccess(subject, relPath string) int {
	s := a.current()
	if !s.enabled {
		return AccessWrite // 未啟用權限分組：全開（相容舊行為）
	}
	target := normPath(relPath)
	eff := s.defaultLevel
	for _, r := range s.rulesEveryone {
		if r.access > eff && matchPath(r.path, target) {
			eff = r.access
		}
	}
	for _, r := range s.rulesBySubject[subject] {
		if r.access > eff && matchPath(r.path, target) {
			eff = r.access
		}
	}
	return eff
}

// Can 判斷 subject 對 relPath 是否具備至少 need 等級的權限。
func (a *Authz) Can(subject, relPath string, need int) bool {
	return a.effectiveAccess(subject, relPath) >= need
}

// HasAnyRead 判斷 subject 是否在任何地方擁有讀取權；全無者登入後僅顯示歡迎頁。
func (a *Authz) HasAnyRead(subject string) bool {
	s := a.current()
	if !s.enabled || s.defaultLevel >= AccessRead {
		return true
	}
	for _, r := range s.rulesEveryone {
		if r.access >= AccessRead {
			return true
		}
	}
	for _, r := range s.rulesBySubject[subject] {
		if r.access >= AccessRead {
			return true
		}
	}
	return false
}

// IsAdmin 判斷 subject 是否為管理員（adminGroup 的具名成員）。
// 未啟用權限分組（相容全開模式）時，視所有登入者為管理員，與「全開＝皆可寫根」一致。
func (a *Authz) IsAdmin(subject string) bool {
	s := a.current()
	if !s.enabled {
		return true
	}
	return s.admins[subject]
}

// RequireAccess 在 handler 開頭檢查權限，不足時回 403 並中止請求；回傳是否放行。
func (a *Authz) RequireAccess(c *gin.Context, relPath string, need int) bool {
	if a.Can(SubjectOf(c), relPath, need) {
		return true
	}
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "權限不足"})
	return false
}

// SubjectOf 取出 JWT 中的穩定身分鍵（由 auth.Middleware 存入 context）。
func SubjectOf(c *gin.Context) string {
	return c.GetString("subject")
}

// matchPath 判斷規則路徑 rule 是否涵蓋目標 target（兩者皆已正規化）。
// 採路徑分段比對，避免 "team" 誤命中 "teamA"。
func matchPath(rule, target string) bool {
	if rule == "" {
		return true
	}
	return target == rule || strings.HasPrefix(target, rule+"/")
}

// accessLevel 將設定字串轉成權限等級整數。
func accessLevel(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "write":
		return AccessWrite
	case "read":
		return AccessRead
	default:
		return AccessNone
	}
}

// normPath 正規化相對路徑：統一斜線、去除頭尾的 /。
func normPath(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	return strings.Trim(p, "/")
}
