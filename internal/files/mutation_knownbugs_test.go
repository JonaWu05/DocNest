//go:build knownbugs

package files

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

// 此規格在階段 4 修復：並發 Create 必須以 O_EXCL 保證只有一個請求成功。
func TestKnownBugConcurrentCreateHasSingleWinner(t *testing.T) {
	root := t.TempDir()
	f := newUnrestrictedTestFiles(t, root)
	const workers = 64
	start := make(chan struct{})
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/create?path=race.md&type=file", nil)
			c.Set("subject", "local:test")
			f.Create(c)
			if w.Code == http.StatusOK {
				successes.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := successes.Load(); got != 1 {
		t.Fatalf("並發建立成功數=%d，預期只有 1", got)
	}
}

// 此規格在階段 4 修復：metadata 寫入失敗時，軟刪除必須 rollback 原始內容。
// 以名為 meta.json 的目錄穩定製造 metadata 路徑與 payload 目錄衝突。
func TestKnownBugTrashMetadataFailureRollsBack(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "meta.json")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "a.md"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newUnrestrictedTestFiles(t, root)
	if err := f.moveToTrash(source, "meta.json", "local:test", true); err == nil {
		t.Fatal("預期 metadata 路徑衝突造成錯誤")
	}
	if _, err := os.Stat(filepath.Join(source, "a.md")); err != nil {
		t.Fatalf("失敗後原始內容應已 rollback：%v", err)
	}
}
