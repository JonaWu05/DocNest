package files

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/JonaWu05/DocNest/internal/authz"
	"github.com/JonaWu05/DocNest/internal/store"
	"github.com/gin-gonic/gin"
)

// writeGraphFixture 在 root 下建立測試用的文件結構：
//
//	a.md         連到 b.md、folder/c.md、外部網址、secret/s.md（無權限）、not-exist.md
//	b.md         無連結
//	folder/c.md  連回 ../a.md（相對於自己資料夾）
//	folder/index.md  資料夾首頁，不應出現在圖裡
//	secret/s.md  alice 無權限讀取
func writeGraphFixture(t *testing.T, root string) {
	t.Helper()
	mustWrite := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("a.md", "# A\n\n連到 [B](b.md)、[C](folder/c.md)、[外部](https://example.com)、"+
		"[秘密](secret/s.md)、[不存在](not-exist.md)。")
	mustWrite("b.md", "# B\n\n沒有連結。")
	mustWrite("folder/c.md", "# C\n\n連回 [A](../a.md)。")
	mustWrite("folder/index.md", "# 資料夾首頁\n\n不該出現在圖裡。")
	mustWrite("secret/s.md", "# 秘密\n\nalice 不可讀。")
}

func TestGraphBasicAndPermissionFiltering(t *testing.T) {
	root := t.TempDir()
	writeGraphFixture(t, root)

	permPath := filepath.Join(t.TempDir(), "permissions.json")
	body := `{"default":"none","groups":{
	  "everyone":{"members":["*"],"rules":[{"path":"","access":"write"}]}
	}}`
	// alice 有整個根目錄的權限，但另外測一個完全沒有 secret 權限的情境需要獨立設定；
	// 這裡先驗證「有權限時」的節點與邊是否正確（含 index.md 排除、外部連結/不存在檔案被忽略）。
	if err := os.WriteFile(permPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	az, err := authz.Load(permPath)
	if err != nil {
		t.Fatal(err)
	}
	f := New(store.New(root), az, nil, nil, false)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("subject", "local:alice")
	f.Graph(c)

	if w.Code != 200 {
		t.Fatalf("預期 200，got %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Nodes []GraphNode
		Edges []GraphEdge
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	nodePaths := map[string]bool{}
	for _, n := range resp.Nodes {
		nodePaths[n.Path] = true
	}
	for _, want := range []string{"a.md", "b.md", "folder/c.md", "secret/s.md"} {
		if !nodePaths[want] {
			t.Errorf("節點應包含 %s，got %+v", want, resp.Nodes)
		}
	}
	if nodePaths["folder/index.md"] {
		t.Error("index.md 不應出現在圖裡")
	}

	edgeSet := map[[2]string]bool{}
	for _, e := range resp.Edges {
		edgeSet[[2]string{e.Source, e.Target}] = true
	}
	if !edgeSet[[2]string{"a.md", "b.md"}] {
		t.Error("應有 a.md -> b.md 的邊")
	}
	if !edgeSet[[2]string{"a.md", "folder/c.md"}] {
		t.Error("應有 a.md -> folder/c.md 的邊")
	}
	if !edgeSet[[2]string{"folder/c.md", "a.md"}] {
		t.Error("應有 folder/c.md -> a.md 的邊（相對 ../ 解析）")
	}
	if !edgeSet[[2]string{"a.md", "secret/s.md"}] {
		t.Error("alice 有權限時應有 a.md -> secret/s.md 的邊")
	}
	if edgeSet[[2]string{"a.md", "not-exist.md"}] {
		t.Error("不存在的檔案不應成為邊")
	}
	for _, e := range resp.Edges {
		if e.Target == "https://example.com" {
			t.Error("外部連結不應成為邊")
		}
	}

	for _, e := range resp.Edges {
		if e.Type != "contain" && e.Type != "link" {
			t.Errorf("每條邊都應標明 Type 為 contain 或 link，got %+v", e)
		}
	}
}

// TestGraphFolderNodesAndContainEdges 驗證資料夾（含虛擬根目錄）會成為節點，且每個節點對其
// 直接上層都有一條 contain 邊——這是使用者 2026-09-07 要求的「資料夾內的東西連到資料夾點」。
func TestGraphFolderNodesAndContainEdges(t *testing.T) {
	root := t.TempDir()
	writeGraphFixture(t, root)

	permPath := filepath.Join(t.TempDir(), "permissions.json")
	body := `{"default":"none","groups":{
	  "everyone":{"members":["*"],"rules":[{"path":"","access":"write"}]}
	}}`
	if err := os.WriteFile(permPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	az, err := authz.Load(permPath)
	if err != nil {
		t.Fatal(err)
	}
	f := New(store.New(root), az, nil, nil, false)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("subject", "local:alice")
	f.Graph(c)

	var resp struct {
		Nodes []GraphNode
		Edges []GraphEdge
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	nodeByPath := map[string]GraphNode{}
	for _, n := range resp.Nodes {
		nodeByPath[n.Path] = n
	}
	root2, ok := nodeByPath[""]
	if !ok || !root2.IsDir {
		t.Errorf("應有 path 為空字串、IsDir=true 的虛擬根目錄節點，got %+v", nodeByPath[""])
	}
	for _, want := range []string{"folder", "secret"} {
		n, ok := nodeByPath[want]
		if !ok || !n.IsDir {
			t.Errorf("節點應包含資料夾 %s 且 IsDir=true，got %+v ok=%v", want, n, ok)
		}
	}
	if n, ok := nodeByPath["a.md"]; !ok || n.IsDir {
		t.Errorf("a.md 應為節點且 IsDir=false，got %+v ok=%v", n, ok)
	}

	containEdges := map[[2]string]bool{}
	for _, e := range resp.Edges {
		if e.Type == "contain" {
			containEdges[[2]string{e.Source, e.Target}] = true
		}
	}
	for _, want := range [][2]string{
		{"", "a.md"}, {"", "b.md"}, {"", "folder"}, {"", "secret"},
		{"folder", "folder/c.md"}, {"secret", "secret/s.md"},
	} {
		if !containEdges[want] {
			t.Errorf("應有 contain 邊 %s -> %s，got %+v", want[0], want[1], containEdges)
		}
	}
	// folder/index.md 本身不成為節點，理所當然也不該有指向它的 contain 邊。
	if containEdges[[2]string{"folder", "folder/index.md"}] {
		t.Error("不應有指向 index.md 的 contain 邊")
	}
}

// TestGraphExcludesUnreadableTarget 驗證讀者看不到自己無權限的節點、也不會出現指向該節點的邊。
func TestGraphExcludesUnreadableTarget(t *testing.T) {
	root := t.TempDir()
	writeGraphFixture(t, root)

	permPath := filepath.Join(t.TempDir(), "permissions.json")
	body := `{"default":"none","groups":{
	  "reader":{"members":["local:bob"],"rules":[{"path":"","access":"read"},{"path":"secret","access":"none"}]}
	}}`
	if err := os.WriteFile(permPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	az, err := authz.Load(permPath)
	if err != nil {
		t.Fatal(err)
	}
	f := New(store.New(root), az, nil, nil, false)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("subject", "local:bob")
	f.Graph(c)

	var resp struct {
		Nodes []GraphNode
		Edges []GraphEdge
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, n := range resp.Nodes {
		if n.Path == "secret/s.md" {
			t.Error("bob 無權限讀 secret/s.md，不應出現在節點裡")
		}
	}
	for _, e := range resp.Edges {
		if e.Target == "secret/s.md" {
			t.Error("bob 無權限讀 secret/s.md，不應出現指向它的邊")
		}
	}
}
