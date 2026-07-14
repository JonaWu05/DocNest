package files

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestConcurrentCreateHasSingleWinner(t *testing.T) {
	tests := []struct {
		name, path, itemType string
	}{
		{name: "file", path: "race.md", itemType: "file"},
		{name: "directory", path: "race-dir", itemType: "dir"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			f := newUnrestrictedTestFiles(t, root)
			const workers = 64
			start := make(chan struct{})
			var successes atomic.Int32
			var conflicts atomic.Int32
			var unexpected atomic.Int32
			var wg sync.WaitGroup
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					w := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(w)
					c.Request = httptest.NewRequest(http.MethodPost, "/api/create?path="+tc.path+"&type="+tc.itemType, nil)
					c.Set("subject", "local:test")
					f.Create(c)
					switch w.Code {
					case http.StatusOK:
						successes.Add(1)
					case http.StatusConflict:
						conflicts.Add(1)
					default:
						unexpected.Add(1)
					}
				}()
			}
			close(start)
			wg.Wait()

			if got := successes.Load(); got != 1 {
				t.Fatalf("並發建立成功數=%d，預期只有 1", got)
			}
			if got := conflicts.Load(); got != workers-1 {
				t.Errorf("衝突回應數=%d，預期 %d", got, workers-1)
			}
			if got := unexpected.Load(); got != 0 {
				t.Errorf("非預期回應數=%d", got)
			}
		})
	}
}

func TestTrashMetadataFailureRollsBack(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "notes", "a.md")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newUnrestrictedTestFiles(t, root)
	f.writeTrashMeta = func(string, []byte) error { return errors.New("injected metadata failure") }

	if err := f.moveToTrash(source, "notes/a.md", "local:test", false); err == nil {
		t.Fatal("預期 metadata 寫入失敗")
	}
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("失敗後原始內容應已 rollback：%v", err)
	}
	if string(content) != "keep" {
		t.Fatalf("rollback 後內容=%q，預期 keep", content)
	}
	entries, err := os.ReadDir(filepath.Join(root, trashDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("rollback 後不應殘留回收項目：%v", entries)
	}
}

func TestTrashMetadataFailureRollsBackDirectory(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "notes")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "a.md"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newUnrestrictedTestFiles(t, root)
	f.writeTrashMeta = func(string, []byte) error { return errors.New("injected metadata failure") }

	if err := f.moveToTrash(source, "notes", "local:test", true); err == nil {
		t.Fatal("預期 metadata 寫入失敗")
	}
	content, err := os.ReadFile(filepath.Join(source, "a.md"))
	if err != nil || string(content) != "keep" {
		t.Fatalf("資料夾 rollback 失敗：content=%q err=%v", content, err)
	}
}

func TestTrashPayloadNamedMetaJSONDoesNotCollide(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "meta.json")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "a.md"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newUnrestrictedTestFiles(t, root)

	if err := f.moveToTrash(source, "meta.json", "local:test", true); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, trashDirName))
	if err != nil || len(entries) != 1 {
		t.Fatalf("回收項目數量不正確：entries=%v err=%v", entries, err)
	}
	m, err := f.readTrashMeta(entries[0].Name())
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(f.trashPayloadPath(entries[0].Name(), m), "a.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "keep" {
		t.Fatalf("回收內容=%q，預期 keep", content)
	}
}

func TestRestoreCurrentTrashLayout(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "notes", "a.md")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newUnrestrictedTestFiles(t, root)
	if err := f.moveToTrash(source, "notes/a.md", "local:test", false); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, trashDirName))
	if err != nil || len(entries) != 1 {
		t.Fatalf("回收項目數量不正確：entries=%v err=%v", entries, err)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/trash/restore?id="+entries[0].Name(), nil)
	c.Set("subject", "local:test")
	f.RestoreTrash(c)

	if w.Code != http.StatusOK {
		t.Fatalf("新版回收項目還原狀態=%d，預期 200", w.Code)
	}
	content, err := os.ReadFile(source)
	if err != nil || string(content) != "current" {
		t.Fatalf("新版內容還原失敗：content=%q err=%v", content, err)
	}
}

func TestRestoreLegacyTrashLayout(t *testing.T) {
	root := t.TempDir()
	f := newUnrestrictedTestFiles(t, root)
	entryDir := filepath.Join(root, trashDirName, "123")
	if err := os.MkdirAll(entryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entryDir, "a.md"), []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := `{"original":"notes/a.md","name":"a.md","isDir":false,"deletedAt":"2026-01-01T00:00:00Z","deletedBy":"local:test"}`
	if err := os.WriteFile(filepath.Join(entryDir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/trash/restore?id=123", nil)
	c.Set("subject", "local:test")
	f.RestoreTrash(c)

	if w.Code != http.StatusOK {
		t.Fatalf("舊版回收項目還原狀態=%d，預期 200", w.Code)
	}
	content, err := os.ReadFile(filepath.Join(root, "notes", "a.md"))
	if err != nil || string(content) != "legacy" {
		t.Fatalf("舊版內容還原失敗：content=%q err=%v", content, err)
	}
}
