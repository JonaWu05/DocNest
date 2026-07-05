package store

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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
	mustMkdir(t, filepath.Join(root, "中文dir"))
	mustMkdir(t, filepath.Join(root, "assets", "sub")) // assets 應整個被略過
	mustMkdir(t, filepath.Join(root, ".hidden"))       // 隱藏目錄略過
	mustWriteContent(t, filepath.Join(root, "welcome.md"), "intro text without heading")
	mustWriteContent(t, filepath.Join(root, "notes", "index.md"), "# 筆記資料夾\n")
	mustWriteContent(t, filepath.Join(root, "notes", "a.md"), "\n# A 文件\nbody")
	mustWrite(t, filepath.Join(root, "報告.md"))         // 不合法真實檔名略過
	mustWrite(t, filepath.Join(root, "中文dir", "a.md")) // 不合法真實資料夾名整個略過
	mustWrite(t, filepath.Join(root, "ignore.exe"))    // 非允許副檔名略過
	mustWrite(t, filepath.Join(root, ".secret.md"))    // 隱藏檔略過

	tree, err := buildTree(root, "")
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
	if notes.Title != "筆記資料夾" {
		t.Errorf("notes Title=%q want 筆記資料夾", notes.Title)
	}
	if len(notes.Children) != 2 || notes.Children[0].Name != "a.md" || notes.Children[0].Path != "notes/a.md" {
		t.Errorf("notes 子節點不正確：%+v", notes.Children)
	}
	if notes.Children[0].Title != "A 文件" {
		t.Errorf("a.md Title=%q want A 文件", notes.Children[0].Title)
	}
	if !notes.Children[1].IsIndex {
		t.Error("index.md 應標記 IsIndex")
	}
	if notes.Children[1].Title != "" {
		t.Errorf("index.md 自身不應帶顯示標題，得到 %q", notes.Children[1].Title)
	}
	if tree.Children[1].Title != "intro text without h" {
		t.Errorf("welcome.md fallback Title=%q", tree.Children[1].Title)
	}
}

// TestAtomicWrite 驗證原子寫入能正確覆寫內容、套用權限，且不殘留暫存檔。
func TestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "note.md")

	if err := AtomicWrite(p, []byte("hello"), 0o644, false); err != nil {
		t.Fatalf("首次寫入失敗：%v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "hello" {
		t.Errorf("內容=%q want hello", b)
	}

	// 覆寫（開啟 fsync 路徑也應正常）
	if err := AtomicWrite(p, []byte("world"), 0o644, true); err != nil {
		t.Fatalf("覆寫失敗：%v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "world" {
		t.Errorf("覆寫後內容=%q want world", b)
	}

	// 目錄內不應殘留 .tmp-* 暫存檔
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("殘留暫存檔：%s", e.Name())
		}
	}
}

// TestLockSerializes 驗證同一路徑的 Lock 會互斥，序列化臨界區。
func TestLockSerializes(t *testing.T) {
	s := New(t.TempDir())
	abs := filepath.Join(s.Root, "a.md")

	var mu sync.Mutex
	inside := 0
	maxInside := 0
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lock := s.Lock(abs)
			defer lock.Unlock()
			mu.Lock()
			inside++
			if inside > maxInside {
				maxInside = inside
			}
			mu.Unlock()
			mu.Lock()
			inside--
			mu.Unlock()
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Errorf("同一路徑臨界區最多應只有 1 個 goroutine，實測 %d", maxInside)
	}

	// 不同路徑取得不同鎖
	if s.Lock(abs) == s.Lock(filepath.Join(s.Root, "b.md")) {
		t.Error("不同路徑不應共用同一把鎖")
	}
}

// TestValidateRelPath 驗證跨平台檔名規則：合法者放行、非法者拒絕。
func TestValidateRelPath(t *testing.T) {
	valid := []string{
		"notes/a.md", "teamA/sub/b.txt", "a-b_c.1.md",
		"assets", "x/../y.md", // 穿越交給 SafeResolve，這裡不擋
	}
	for _, p := range valid {
		if err := ValidateRelPath(p); err != nil {
			t.Errorf("ValidateRelPath(%q) 應通過，得到 %v", p, err)
		}
	}

	invalid := []string{
		"a<b.md",          // 非法字元
		"a:b.md",          // 非法字元（Windows 磁碟分隔）
		"a|b.md",          // 非法字元
		"foo/CON.md",      // 保留裝置名
		"nul",             // 保留裝置名（無副檔名）
		"LPT1.txt",        // 保留裝置名（大小寫不敏感）
		"報告.md",           // 真實路徑僅允許英文、數字與 - _ .
		"trailing.md ",    // 段結尾空白
		"dir/ leading.md", // 段首空白
		"end.",            // 結尾為點
		"a\x00b.md",       // 控制字元
	}
	for _, p := range invalid {
		if err := ValidateRelPath(p); err == nil {
			t.Errorf("ValidateRelPath(%q) 應被拒絕", p)
		}
	}
}

// TestSanitizeName 驗證檔名淨化：不合法字元壓成 _、頭尾清理、空名與保留名 fallback，
// 且產出一律通過 validateName（與驗證共用同一字元集定義）。
func TestSanitizeName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"photo.png", "photo.png"},              // 已合法：原樣保留
		{"螢幕截圖.png", "file.png"},                // 全不合法：fallback
		{"螢幕擷取 2026-07.png", "2026-07.png"},     // 前段不合法：不留前導 _
		{"my photo (1).png", "my_photo_1.png"},  // 連續不合法字元壓成一個 _
		{"...test.png", "test.png"},             // 頭尾多餘的 . 清除
		{"___.png", "file.png"},                 // 清理後為空：fallback
		{"a b.tar.gz", "a_b.tar.gz"},            // 多段副檔名：只動主檔名
		{"報告 v2 final.pdf", "v2_final.pdf"},     // 中英混合
		{"con.png", "file.png"},                 // Windows 保留裝置名：fallback
		{"AUX.report.pdf", "file.pdf"},          // 保留名比對第一個點之前的主檔名
	}
	for _, tc := range cases {
		got := SanitizeName(tc.in, "file")
		if got != tc.want {
			t.Errorf("SanitizeName(%q)=%q want %q", tc.in, got, tc.want)
		}
		if err := validateName(got); err != nil {
			t.Errorf("SanitizeName(%q)=%q 未通過 validateName：%v", tc.in, got, err)
		}
	}
}

// TestCachedTreeInvalidate 驗證快取命中、失效後重建，以及回傳的是共用實例。
func TestCachedTreeInvalidate(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.md"))
	s := New(root)

	t1, err := s.CachedTree()
	if err != nil {
		t.Fatal(err)
	}
	// 連續取用應命中同一快取實例（未逾 TTL、未失效）
	t2, _ := s.CachedTree()
	if t1 != t2 {
		t.Error("TTL 內應回傳同一快取實例")
	}

	// 失效後應重建出新的實例
	s.InvalidateTree()
	t3, _ := s.CachedTree()
	if t3 == t1 {
		t.Error("失效後應重建，不該是舊實例")
	}

	// 新增檔案後（失效）應反映於樹中
	mustWrite(t, filepath.Join(root, "b.md"))
	s.InvalidateTree()
	t4, _ := s.CachedTree()
	var names []string
	for _, n := range t4.Children {
		names = append(names, n.Name)
	}
	if len(names) != 2 {
		t.Errorf("重建後應有 2 個檔案，得到 %v", names)
	}
}

// TestScanAssetsAndCache 驗證 assets 掃描內容、快取命中與失效後重建。
func TestScanAssetsAndCache(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "assets", "sub"))
	mustMkdir(t, filepath.Join(root, "assets", "中文夾")) // 不合法真實資料夾名整個略過
	mustWrite(t, filepath.Join(root, "assets", "a.png"))
	mustWrite(t, filepath.Join(root, "assets", "sub", "b.pdf"))
	mustWrite(t, filepath.Join(root, "assets", "報告.png"))       // 不合法真實檔名略過
	mustWrite(t, filepath.Join(root, "assets", ".hidden.png"))  // 隱藏檔略過
	mustWrite(t, filepath.Join(root, "assets", "中文夾", "c.png")) // 不合法資料夾底下不列入
	s := New(root)

	entries, err := s.ScanAssets()
	if err != nil {
		t.Fatal(err)
	}
	var files, dirs int
	var img bool
	for _, e := range entries {
		if e.IsDir {
			dirs++
		} else {
			files++
			if e.Path == "assets/a.png" && e.IsImage {
				img = true
			}
		}
	}
	if files != 2 || dirs != 1 {
		t.Errorf("應有 2 檔 1 資料夾，得到 files=%d dirs=%d", files, dirs)
	}
	if !img {
		t.Error("a.png 應標記為圖片")
	}

	// 快取命中：同一實例
	e2, _ := s.ScanAssets()
	if &entries[0] != &e2[0] {
		t.Error("TTL 內應回傳同一快取切片")
	}

	// 失效後新增檔案應反映
	mustWrite(t, filepath.Join(root, "assets", "c.zip"))
	s.InvalidateAssets()
	e3, _ := s.ScanAssets()
	var n int
	for _, e := range e3 {
		if !e.IsDir {
			n++
		}
	}
	if n != 3 {
		t.Errorf("失效重建後應有 3 個檔案，得到 %d", n)
	}
}

// TestScanAssetsNoDir 驗證 assets 目錄不存在時回傳空清單而非錯誤。
func TestScanAssetsNoDir(t *testing.T) {
	s := New(t.TempDir())
	entries, err := s.ScanAssets()
	if err != nil {
		t.Fatalf("無 assets 目錄不應出錯：%v", err)
	}
	if len(entries) != 0 {
		t.Errorf("應為空清單，得到 %d", len(entries))
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
	mustWriteContent(t, p, "x")
}

func mustWriteContent(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
