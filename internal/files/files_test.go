package files

import (
	"os"
	"path/filepath"
	"testing"

	"markdownEditor/internal/authz"
	"markdownEditor/internal/store"
)

// TestFilterTree 驗證檔案樹依讀取權過濾、並正確標記 writable。
func TestFilterTree(t *testing.T) {
	permPath := filepath.Join(t.TempDir(), "permissions.json")
	body := `{"default":"none","groups":{
	  "everyone":{"members":["*"],"rules":[{"path":"welcome.md","access":"read"}]},
	  "editors":{"members":["local:alice"],"rules":[{"path":"teamA","access":"write"}]}
	}}`
	if err := os.WriteFile(permPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	az, err := authz.Load(permPath)
	if err != nil {
		t.Fatal(err)
	}
	f := New(store.New(t.TempDir()), az, nil, false) // filterTree 不使用 hub，傳 nil 即可

	nodes := []*store.FileNode{
		{Name: "welcome.md", Path: "welcome.md"},
		{Name: "teamA", Path: "teamA", IsDir: true, Children: []*store.FileNode{
			{Name: "x.md", Path: "teamA/x.md"},
		}},
		{Name: "secret", Path: "secret", IsDir: true, Children: []*store.FileNode{
			{Name: "s.md", Path: "secret/s.md"},
		}},
	}

	out := f.filterTree(nodes, "local:alice")

	// alice：welcome 可讀（everyone，唯讀）、teamA 可寫、secret 完全看不到
	got := map[string]bool{} // path -> writable
	for _, n := range out {
		got[n.Path] = n.Writable
	}
	if _, ok := got["secret"]; ok {
		t.Error("secret 無讀取權，應被過濾掉")
	}
	if w, ok := got["welcome.md"]; !ok || w {
		t.Errorf("welcome.md 應保留且唯讀（writable=false），got ok=%v writable=%v", ok, w)
	}
	if w, ok := got["teamA"]; !ok || !w {
		t.Errorf("teamA 應保留且可寫，got ok=%v writable=%v", ok, w)
	}

	// 未分組者：只剩 welcome.md（透過 "*"）
	out2 := f.filterTree(cloneNodes(nodes), "local:nobody")
	if len(out2) != 1 || out2[0].Path != "welcome.md" {
		t.Errorf("未分組者應只看到 welcome.md，got %+v", out2)
	}
}

// TestFileVersion 驗證版本識別（size+mtime）對同一狀態穩定、內容大小改變時變動。
func TestFileVersion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "v.md")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	info1, _ := os.Stat(p)
	v1 := fileVersion(info1)

	// 同一狀態再次 stat：版本應一致
	info1b, _ := os.Stat(p)
	if fileVersion(info1b) != v1 {
		t.Error("同一檔案狀態的版本應穩定")
	}

	// 改變內容大小：版本應改變（size 不同即可，毋須依賴 mtime 粒度）
	if err := os.WriteFile(p, []byte("hello world!!"), 0o644); err != nil {
		t.Fatal(err)
	}
	info2, _ := os.Stat(p)
	if fileVersion(info2) == v1 {
		t.Error("內容大小改變後版本應不同")
	}
}

// cloneNodes 複製一份節點樹，避免 filterTree 就地修改 Children 影響第二次呼叫。
func cloneNodes(nodes []*store.FileNode) []*store.FileNode {
	out := make([]*store.FileNode, len(nodes))
	for i, n := range nodes {
		cp := *n
		cp.Children = cloneNodes(n.Children)
		out[i] = &cp
	}
	return out
}
