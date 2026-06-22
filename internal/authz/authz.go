// Package authz 實作以「群組 + 路徑前綴」為基礎的授權判斷。
// 純粹處理 (身分鍵, 相對路徑, 等級) 的決策，不涉及檔案系統或路徑換算。
package authz

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// 權限等級（由低到高）：none < read < write；write 隱含 read，
// 而 create / rename / delete 等修改動作一律歸類為 write。
const (
	AccessNone = iota
	AccessRead
	AccessWrite
)

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

// Authz 保存一份已載入的權限設定，提供查詢方法。取代原本的 package 級全域。
type Authz struct {
	enabled        bool                  // 是否成功載入設定檔；否則為相容的「全開」模式
	defaultLevel   int                   // 預設權限等級
	rulesBySubject map[string][]normRule // subject -> 規則
	rulesEveryone  []normRule            // 萬用成員 "*" 的規則（套用到所有已登入者）
}

// Load 從設定檔建立 Authz。
// 檔案不存在時不啟用權限分組（相容舊部署：全開，並印出警告）；
// 檔案存在但解析失敗則回傳錯誤，由呼叫端決定是否中止啟動。
func Load(path string) (*Authz, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("[警告] 找不到權限設定檔 %s：未啟用權限分組，所有登入者皆可存取全部檔案（僅適用於開發或單一信任群組）", path)
			return &Authz{enabled: false}, nil
		}
		return nil, err
	}

	var cfg fileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	a := &Authz{
		enabled:        true,
		defaultLevel:   accessLevel(cfg.Default),
		rulesBySubject: map[string][]normRule{},
	}
	for _, g := range cfg.Groups {
		rules := make([]normRule, 0, len(g.Rules))
		for _, r := range g.Rules {
			rules = append(rules, normRule{path: normPath(r.Path), access: accessLevel(r.Access)})
		}
		for _, m := range g.Members {
			if m = strings.TrimSpace(m); m == "" {
				continue
			}
			if m == "*" {
				a.rulesEveryone = append(a.rulesEveryone, rules...)
			} else {
				a.rulesBySubject[m] = append(a.rulesBySubject[m], rules...)
			}
		}
	}
	log.Printf("[權限] 已載入 %s：%d 個群組、預設權限 = %s", path, len(cfg.Groups), strings.ToLower(strings.TrimSpace(cfg.Default)))
	return a, nil
}

// effectiveAccess 回傳 subject 對 relPath 的有效權限等級：
// 以預設等級為基準，取所有命中規則（含萬用規則）中的最大值。
func (a *Authz) effectiveAccess(subject, relPath string) int {
	if !a.enabled {
		return AccessWrite // 未啟用權限分組：全開（相容舊行為）
	}
	target := normPath(relPath)
	eff := a.defaultLevel
	for _, r := range a.rulesEveryone {
		if r.access > eff && matchPath(r.path, target) {
			eff = r.access
		}
	}
	for _, r := range a.rulesBySubject[subject] {
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
	if !a.enabled || a.defaultLevel >= AccessRead {
		return true
	}
	for _, r := range a.rulesEveryone {
		if r.access >= AccessRead {
			return true
		}
	}
	for _, r := range a.rulesBySubject[subject] {
		if r.access >= AccessRead {
			return true
		}
	}
	return false
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
