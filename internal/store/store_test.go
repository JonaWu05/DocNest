package store

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSafeResolve(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	// 合法路徑：解析出的絕對路徑須在 root 底下
	abs, err := s.SafeResolve("notes/a.md")
	if err != nil {
		t.Fatalf("合法路徑不該失敗：%v", err)
	}
	if !strings.HasPrefix(abs, root) {
		t.Errorf("解析結果 %q 不在 root %q 底下", abs, root)
	}

	// 路徑穿越 / 空路徑：一律拒絕
	for _, bad := range []string{"../secret", "../../etc/passwd", "", "  "} {
		if _, err := s.SafeResolve(bad); err == nil {
			t.Errorf("SafeResolve(%q) 應失敗", bad)
		}
	}

	// 內部的 .. 但最終仍在 root 內 → 允許
	if _, err := s.SafeResolve("a/../b.md"); err != nil {
		t.Errorf("內部 .. 但仍在 root 內應允許：%v", err)
	}
}

func TestRelOf(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	abs, _ := s.SafeResolve("notes/a.md")
	if got := s.RelOf(abs); got != "notes/a.md" {
		t.Errorf("RelOf=%q want notes/a.md", got)
	}
}

func TestExtHelpers(t *testing.T) {
	if !IsAllowedFile("x.md") || !IsAllowedFile("X.TXT") {
		t.Error("md/txt 應允許（含大寫）")
	}
	if IsAllowedFile("x.exe") {
		t.Error("exe 不應允許")
	}
	if !IsImageExt(".PNG") {
		t.Error("png 應為圖片")
	}
	if IsImageExt(".svg") {
		t.Error("svg 不應視為允許圖片（XSS 風險）")
	}
	if !IsAllowedUpload(".pdf") || !IsAllowedUpload(".png") {
		t.Error("pdf/png 應允許上傳")
	}
	if IsAllowedUpload(".exe") {
		t.Error("exe 不應允許上傳")
	}
}

func TestBuildTree(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "notes"))
	mustMkdir(t, filepath.Join(root, "assets", "sub")) // assets 應整個被略過
	mustMkdir(t, filepath.Join(root, ".hidden"))       // 隱藏目錄略過
	mustWrite(t, filepath.Join(root, "welcome.md"))
	mustWrite(t, filepath.Join(root, "notes", "a.md"))
	mustWrite(t, filepath.Join(root, "ignore.exe")) // 非允許副檔名略過
	mustWrite(t, filepath.Join(root, ".secret.md")) // 隱藏檔略過

	tree, err := New(root).BuildTree()
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, n := range tree.Children {
		names = append(names, n.Name)
	}
	// 資料夾在前、檔案在後；assets/.hidden/ignore.exe/.secret.md 皆排除
	if want := []string{"notes", "welcome.md"}; !reflect.DeepEqual(names, want) {
		t.Errorf("頂層節點=%v want %v", names, want)
	}
	notes := tree.Children[0]
	if len(notes.Children) != 1 || notes.Children[0].Name != "a.md" || notes.Children[0].Path != "notes/a.md" {
		t.Errorf("notes 子節點不正確：%+v", notes.Children)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}
