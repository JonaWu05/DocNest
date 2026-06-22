package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

// 權限等級（由低到高）：none < read < write；write 隱含 read，
// 而 create / rename / delete 等修改動作一律歸類為 write。
const (
	accessNone = iota
	accessRead
	accessWrite
)

// permRule 為單一條規則：對某路徑前綴授予某等級的存取權。
type permRule struct {
	Path   string `json:"path"`   // 相對 DOC_ROOT 的路徑前綴；"" 代表根（涵蓋全部）
	Access string `json:"access"` // "read" | "write" | "none"
}

// permGroup 為一個權限群組：成員 + 規則。
type permGroup struct {
	Members []string   `json:"members"` // 身分鍵：local:<帳號> 或 discord:<ID>
	Rules   []permRule `json:"rules"`
}

// permissionConfig 對應 permissions.json 的整體結構。
type permissionConfig struct {
	Default string               `json:"default"` // 未命中任何規則時的預設等級（建議 "none"）
	Groups  map[string]permGroup `json:"groups"`
}

// normRule 為正規化後的規則（路徑統一格式、等級轉成整數），供查詢時直接比對。
type normRule struct {
	path   string // 正規化路徑（/ 分隔、去頭尾 /），"" 代表根
	access int
}

var (
	permsEnabled bool // 是否成功載入 permissions.json；未載入時為相容的「全開」模式
	permDefault  int  // 預設權限等級
	// subject -> 該使用者所屬所有群組規則的攤平索引（啟動時建好，查詢時零配置）
	permRulesBySubject map[string][]normRule
	// 萬用成員 "*" 的規則：套用到「所有已登入者」，與個別 subject 規則合併取最寬鬆。
	// 用途如：把首頁 welcome.md 開放給任何能登入的人讀取。
	permRulesEveryone []normRule
)

// accessLevel 將設定字串轉成權限等級整數。
func accessLevel(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "write":
		return accessWrite
	case "read":
		return accessRead
	default:
		return accessNone
	}
}

// normPath 正規化相對路徑：統一斜線、去除頭尾的 /。
func normPath(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	return strings.Trim(p, "/")
}

// loadPermissions 於啟動時載入權限設定檔（PERMISSIONS_FILE，預設 ./permissions.json）。
// 檔案不存在時不啟用權限分組（相容舊部署：所有登入者皆可全權存取，並印出警告）。
// 檔案存在但解析失敗則直接中止啟動，避免在權限設定有誤時帶著錯誤上線。
func loadPermissions() {
	path := strings.TrimSpace(os.Getenv("PERMISSIONS_FILE"))
	if path == "" {
		path = "./permissions.json"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("[警告] 找不到權限設定檔 %s：未啟用權限分組，所有登入者皆可存取全部檔案（僅適用於開發或單一信任群組）", path)
			permsEnabled = false
			return
		}
		panic("讀取權限設定檔失敗：" + err.Error())
	}

	var cfg permissionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		panic("解析權限設定檔失敗：" + err.Error())
	}

	permsEnabled = true
	permDefault = accessLevel(cfg.Default) // 空字串 / 無法辨識 → none

	// 建立 subject -> 規則 的攤平索引：一個使用者可能同屬多個群組，規則全部合併。
	// 成員 "*" 為萬用：規則歸入 permRulesEveryone，套用到所有已登入者。
	permRulesBySubject = map[string][]normRule{}
	permRulesEveryone = nil
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
				permRulesEveryone = append(permRulesEveryone, rules...)
			} else {
				permRulesBySubject[m] = append(permRulesBySubject[m], rules...)
			}
		}
	}

	log.Printf("[權限] 已載入 %s：%d 個群組、預設權限 = %s", path, len(cfg.Groups), strings.ToLower(strings.TrimSpace(cfg.Default)))
}

// matchPath 判斷規則路徑 rule 是否涵蓋目標 target（兩者皆已正規化）。
// 採路徑分段比對，避免 "team" 誤命中 "teamA"。
func matchPath(rule, target string) bool {
	if rule == "" {
		return true // 根：涵蓋全部
	}
	return target == rule || strings.HasPrefix(target, rule+"/")
}

// effectiveAccess 回傳 subject 對 relPath 的有效權限等級：
// 以預設等級為基準，取所有命中規則中的最大值（多群組／多規則皆取最寬鬆）。
func effectiveAccess(subject, relPath string) int {
	if !permsEnabled {
		return accessWrite // 未啟用權限分組：全開（相容舊行為）
	}
	target := normPath(relPath)
	eff := permDefault
	// 先套用萬用（所有人）規則，再套用個別 subject 規則；皆取最寬鬆
	for _, r := range permRulesEveryone {
		if r.access > eff && matchPath(r.path, target) {
			eff = r.access
		}
	}
	for _, r := range permRulesBySubject[subject] {
		if r.access > eff && matchPath(r.path, target) {
			eff = r.access
		}
	}
	return eff
}

// canAccess 判斷 subject 對 relPath 是否具備至少 need 等級的權限。
func canAccess(subject, relPath string, need int) bool {
	return effectiveAccess(subject, relPath) >= need
}

// hasAnyRead 判斷 subject 是否在任何地方擁有讀取權；
// 全無讀取權者登入後僅顯示歡迎頁（檔案樹為空）。
func hasAnyRead(subject string) bool {
	if !permsEnabled || permDefault >= accessRead {
		return true
	}
	for _, r := range permRulesEveryone {
		if r.access >= accessRead {
			return true
		}
	}
	for _, r := range permRulesBySubject[subject] {
		if r.access >= accessRead {
			return true
		}
	}
	return false
}

// subjectOf 取出 JWT 中的穩定身分鍵（由 AuthMiddleware 存入 context）。
func subjectOf(c *gin.Context) string {
	return c.GetString("subject")
}

// relOf 由絕對路徑換算成相對 DOC_ROOT 的正規化路徑，供權限比對使用。
func relOf(absPath string) string {
	rel, err := filepath.Rel(docRoot, absPath)
	if err != nil {
		return ""
	}
	return normPath(rel)
}

// requireAccess 在 handler 開頭檢查權限，不足時回 403 並中止請求；回傳是否放行。
func requireAccess(c *gin.Context, relPath string, need int) bool {
	if canAccess(subjectOf(c), relPath, need) {
		return true
	}
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "權限不足"})
	return false
}

// filterTree 依 subject 的讀取權過濾檔案樹：
//   - 檔案：可讀才保留
//   - 資料夾：自身可讀、或含有可讀後代時保留（讓使用者能逐層導覽到可讀檔案）
//
// 同時標記每個保留節點的 Writable，供前端隱藏無權限的編輯／刪除操作。
func filterTree(nodes []*FileNode, subject string) []*FileNode {
	out := []*FileNode{}
	for _, n := range nodes {
		if n.IsDir {
			n.Children = filterTree(n.Children, subject)
			if len(n.Children) > 0 || canAccess(subject, n.Path, accessRead) {
				n.Writable = canAccess(subject, n.Path, accessWrite)
				out = append(out, n)
			}
		} else if canAccess(subject, n.Path, accessRead) {
			n.Writable = canAccess(subject, n.Path, accessWrite)
			out = append(out, n)
		}
	}
	return out
}
