package files

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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
	f := New(store.New(t.TempDir()), az, nil, nil, false) // filterTree 不使用 hub / filewatch，傳 nil 即可

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

// TestResolveAssetRel 驗證「相對於來源文件」的連結換算成 DOC_ROOT 相對路徑，與前端規則一致。
func TestResolveAssetRel(t *testing.T) {
	cases := []struct{ docRel, src, want string }{
		{"notes/a.md", "assets/x.png", "notes/assets/x.png"},   // 同層相對
		{"notes/a.md", "../assets/x.png", "assets/x.png"},      // 上一層
		{"notes/sub/a.md", "../../assets/x.png", "assets/x.png"}, // 多層上溯
		{"a.md", "assets/x.png", "assets/x.png"},              // 根目錄文件
		{"notes/a.md", "./img/x.png", "notes/img/x.png"},      // ./ 當前目錄
		{"notes/a.md", "../../../x.png", "x.png"},             // 上溯超出根：多餘的 .. 被吃掉
	}
	for _, c := range cases {
		if got := resolveAssetRel(c.docRel, c.src); got != c.want {
			t.Errorf("resolveAssetRel(%q, %q) = %q, want %q", c.docRel, c.src, got, c.want)
		}
	}
}

// TestExtractAssetRefs 驗證從文件內容解析出引用的本機 asset：
// 涵蓋 Markdown 圖片/連結、原始 HTML、標題與角括號、URL 編碼，並略過外部連結。
func TestExtractAssetRefs(t *testing.T) {
	content := "" +
		"![pic](assets/a.png)\n" +
		"[file](../shared/b.pdf)\n" +
		"![t](assets/c.png \"標題\")\n" + // 帶標題
		"[d](<assets/d%20space.png>)\n" + // 角括號 + URL 編碼空白
		"<img src=\"assets/e.png\">\n" + // 原始 HTML img
		"[ext](https://example.com/x.png)\n" + // 外部，略過
		"[abs](/etc/passwd)\n" + // 絕對路徑，略過
		"[anchor](#section)\n" // 錨點，略過

	refs := extractAssetRefs(content, "notes/doc.md")

	want := []string{
		"notes/assets/a.png",
		"shared/b.pdf",
		"notes/assets/c.png",
		"notes/assets/d space.png",
		"notes/assets/e.png",
	}
	for _, w := range want {
		if !refs[w] {
			t.Errorf("應解析出引用 %q，refs=%v", w, refs)
		}
	}
	if len(refs) != len(want) {
		t.Errorf("外部/絕對/錨點連結應被略過，預期 %d 筆，got %d：%v", len(want), len(refs), refs)
	}
}

// TestPurgeExpiredTrash 驗證背景清除：刪除時間超過保留期的回收項目會被永久刪除，未過期的保留。
func TestPurgeExpiredTrash(t *testing.T) {
	root := t.TempDir()
	f := New(store.New(root), nil, nil, nil, false) // 清除邏輯不使用 az / hub / filewatch

	mkEntry := func(id, deletedAt string) {
		dir := filepath.Join(root, ".trash", id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		meta := `{"original":"notes/a.md","name":"a.md","isDir":false,"deletedAt":"` + deletedAt + `","deletedBy":"x"}`
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkEntry("1000000000000000001", time.Now().Add(-20*24*time.Hour).Format(time.RFC3339)) // 過期
	mkEntry("1000000000000000002", time.Now().Add(-1*24*time.Hour).Format(time.RFC3339))  // 未過期

	if removed := f.purgeExpiredTrash(15 * 24 * time.Hour); removed != 1 {
		t.Fatalf("應清除 1 筆過期項目，got %d", removed)
	}
	if _, err := os.Stat(filepath.Join(root, ".trash", "1000000000000000001")); !os.IsNotExist(err) {
		t.Error("過期項目應被永久刪除")
	}
	if _, err := os.Stat(filepath.Join(root, ".trash", "1000000000000000002")); err != nil {
		t.Error("未過期項目應保留")
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
