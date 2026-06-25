package filewatch

import (
	"testing"
	"time"
)

// newTestWatcher 建立一個不啟動輪詢迴圈的 Watcher，直接以 tick() 驅動，測試可控且不依賴時間。
func newTestWatcher(open func() []string, ver func(string) (string, bool), ext func(string)) *Watcher {
	return New(time.Hour, open, ver, ext)
}

// TestFirstSeenNoEvent：首次納入監看只建立基準，不應觸發外部改檔事件。
func TestFirstSeenNoEvent(t *testing.T) {
	var fired []string
	w := newTestWatcher(
		func() []string { return []string{"a.md"} },
		func(string) (string, bool) { return "v1", true },
		func(rel string) { fired = append(fired, rel) },
	)
	w.tick()
	if len(fired) != 0 {
		t.Fatalf("首次監看不應觸發事件，got %v", fired)
	}
}

// TestExternalChangeFires：版本改變（外部改寫）應觸發一次事件，且不重複觸發同一版本。
func TestExternalChangeFires(t *testing.T) {
	var fired []string
	ver := "v1"
	w := newTestWatcher(
		func() []string { return []string{"a.md"} },
		func(string) (string, bool) { return ver, true },
		func(rel string) { fired = append(fired, rel) },
	)
	w.tick()       // 建立基準 v1
	ver = "v2"     // 外部改寫
	w.tick()       // 應觸發
	w.tick()       // 版本未再變，不應重複觸發
	if len(fired) != 1 || fired[0] != "a.md" {
		t.Fatalf("外部改檔應恰好觸發一次，got %v", fired)
	}
}

// TestNoteWriteSuppresses：本程式寫檔後以 NoteWrite 登記新版本，下次輪詢比對相同，不應視為外部改檔。
func TestNoteWriteSuppresses(t *testing.T) {
	var fired []string
	ver := "v1"
	w := newTestWatcher(
		func() []string { return []string{"a.md"} },
		func(string) (string, bool) { return ver, true },
		func(rel string) { fired = append(fired, rel) },
	)
	w.tick()              // 基準 v1
	ver = "v2"            // 本程式寫入造成版本變動
	w.NoteWrite("a.md", "v2") // 登記自寫
	w.tick()              // 版本與登記相同 → 不觸發
	if len(fired) != 0 {
		t.Fatalf("自寫不應觸發事件，got %v", fired)
	}
}

// TestPruneClosedPaths：已不再開著的檔案應從 known 移除；重新開啟時重建基準而不誤觸發。
func TestPruneClosedPaths(t *testing.T) {
	var fired []string
	open := []string{"a.md"}
	ver := "v1"
	w := newTestWatcher(
		func() []string { return open },
		func(string) (string, bool) { return ver, true },
		func(rel string) { fired = append(fired, rel) },
	)
	w.tick() // 基準 v1
	open = []string{}
	w.tick() // a.md 不再開著 → 從 known 移除
	if _, ok := w.known["a.md"]; ok {
		t.Fatal("關閉後 a.md 應自 known 移除")
	}
	open = []string{"a.md"}
	ver = "v2" // 期間磁碟已改，但重新開啟視為新基準
	w.tick()   // 重建基準，不觸發
	if len(fired) != 0 {
		t.Fatalf("重新開啟應重建基準而不觸發，got %v", fired)
	}
}

// TestUnreadableSkipped：versionOf 回報不可讀（ok=false）時略過，不觸發也不影響既有基準。
func TestUnreadableSkipped(t *testing.T) {
	var fired []string
	ok := true
	w := newTestWatcher(
		func() []string { return []string{"a.md"} },
		func(string) (string, bool) { return "v1", ok },
		func(rel string) { fired = append(fired, rel) },
	)
	w.tick()      // 基準 v1
	ok = false    // 暫時不可讀
	w.tick()      // 略過
	if len(fired) != 0 {
		t.Fatalf("不可讀應略過，got %v", fired)
	}
	if w.known["a.md"] != "v1" {
		t.Fatal("不可讀不應改動既有基準")
	}
}
