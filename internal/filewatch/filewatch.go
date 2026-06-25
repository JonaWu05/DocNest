// Package filewatch 偵測「非經本程式」的外部改檔（git pull、腳本、伺服器直接編輯、同步工具），
// 並回呼通知上層（廣播給開檔者 / 通知共編房間）。
//
// 設計：只輪詢「目前有人開著」的檔案，而非掃描整棵文件樹——工作量上界為同時開啟的不同檔數，
// 契合 store 既有「閒置不掃盤」的哲學。經本程式的寫入會以 NoteWrite 登記版本，下次輪詢比對相同
// 即視為自寫而略過，只有版本不同（外部改寫）才觸發 onExternal。
//
// 本套件刻意零向內耦合：開檔清單、版本計算、外部變更回呼皆由建構時注入的函式提供，
// 由 main 以 hub / collab / store 接線（見 main.go）。
package filewatch

import (
	"sync"
	"time"
)

// Watcher 週期性比對開檔的磁碟版本，偵測外部改檔。
type Watcher struct {
	interval   time.Duration
	openPaths  func() []string             // 目前有人開著的檔案（DOC_ROOT 相對路徑）
	versionOf  func(rel string) (string, bool) // 取得檔案目前版本（size+mtime）；檔案不存在/不可讀時 ok=false
	onExternal func(rel string)            // 偵測到外部改檔時呼叫（同一 rel 每次變更僅觸發一次）

	mu    sync.Mutex
	known map[string]string // rel → 最近一次已知版本（含自寫登記與已回報的外部版本）

	quit chan struct{}
	once sync.Once
}

// New 建立 Watcher。interval 為輪詢間隔；其餘為注入的相依函式。
func New(interval time.Duration, openPaths func() []string, versionOf func(rel string) (string, bool), onExternal func(rel string)) *Watcher {
	return &Watcher{
		interval:   interval,
		openPaths:  openPaths,
		versionOf:  versionOf,
		onExternal: onExternal,
		known:      map[string]string{},
		quit:       make(chan struct{}),
	}
}

// NoteWrite 由本程式寫檔後呼叫，登記寫入後的版本，使下次輪詢比對相同而不誤判為外部改檔。
func (w *Watcher) NoteWrite(rel, version string) {
	w.mu.Lock()
	w.known[rel] = version
	w.mu.Unlock()
}

// Run 啟動輪詢迴圈，直到 Stop。通常以 goroutine 執行。
func (w *Watcher) Run() {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w.tick()
		case <-w.quit:
			return
		}
	}
}

// Stop 結束輪詢迴圈。可安全重複呼叫。
func (w *Watcher) Stop() {
	w.once.Do(func() { close(w.quit) })
}

// tick 為單次輪詢：比對每個開檔的目前版本與已知版本，變更則回報；並清掉已不再開著的條目。
func (w *Watcher) tick() {
	paths := w.openPaths()
	seen := make(map[string]bool, len(paths))

	w.mu.Lock()
	defer w.mu.Unlock()
	for _, rel := range paths {
		seen[rel] = true
		ver, ok := w.versionOf(rel)
		if !ok {
			continue // 檔案不存在 / 不可讀：略過（刪除等情境另循一般流程）
		}
		prev, had := w.known[rel]
		if !had {
			w.known[rel] = ver // 首次納入監看：建立基準，不觸發
			continue
		}
		if prev != ver {
			w.known[rel] = ver
			w.onExternal(rel)
		}
	}

	// 清掉已無人開著的條目，避免 known 無界成長；重新開啟時會重新建立基準（不誤觸發）。
	for rel := range w.known {
		if !seen[rel] {
			delete(w.known, rel)
		}
	}
}
