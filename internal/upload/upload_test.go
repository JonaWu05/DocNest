package upload

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/JonaWu05/DocNest/internal/authz"
	"github.com/JonaWu05/DocNest/internal/store"
)

// newTestUpload 建立測試用 Upload：root 為暫存 DOC_ROOT，permBody 為 permissions.json 內容。
func newTestUpload(t *testing.T, root, permBody string) *Upload {
	t.Helper()
	permPath := filepath.Join(t.TempDir(), "permissions.json")
	if err := os.WriteFile(permPath, []byte(permBody), 0o644); err != nil {
		t.Fatal(err)
	}
	az, err := authz.Load(permPath)
	if err != nil {
		t.Fatal(err)
	}
	return New(store.New(root), az)
}

// allWrite 為全域可寫的 permissions.json（簡化與權限無關的測試）。
const allWrite = `{"default":"none","groups":{
  "everyone":{"members":["*"],"rules":[{"path":"","access":"write"}]}
}}`

// newTestCtx 建立帶 subject 的 gin 測試 context 與回應記錄器。
func newTestCtx(method, target, subject string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, target, nil)
	c.Set("subject", subject)
	return c, w
}

func mustWrite(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestUploadSanitizesFilename 驗證上傳含中文/空白檔名時自動淨化存檔，而非拒絕。
func TestUploadSanitizesFilename(t *testing.T) {
	root := t.TempDir()
	u := newTestUpload(t, root, allWrite)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "測試 photo.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("fake-png")); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("dir", "assets"); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	c, w := newTestCtx("POST", "/api/upload", "local:alice")
	c.Request = httptest.NewRequest("POST", "/api/upload", &buf)
	c.Request.Header.Set("Content-Type", mw.FormDataContentType())
	c.Set("subject", "local:alice")

	u.UploadFile(c)
	if w.Code != 200 {
		t.Fatalf("上傳應成功，得到 %d：%s", w.Code, w.Body.String())
	}
	var resp struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Name != "photo.png" {
		t.Errorf("淨化後檔名=%q want photo.png", resp.Name)
	}
	if !regexp.MustCompile(`^assets/\d+_photo\.png$`).MatchString(resp.Path) {
		t.Errorf("存放路徑=%q 不符「assets/時間戳_photo.png」", resp.Path)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(resp.Path))); err != nil {
		t.Errorf("實體檔案應存在：%v", err)
	}
}

// TestListAssetsDirFilter 驗證 dir 參數只回傳指定資料夾的直層附件，並擋 assets 以外路徑。
func TestListAssetsDirFilter(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "assets", "a.png"))
	mustWrite(t, filepath.Join(root, "assets", "sub", "b.pdf"))
	mustWrite(t, filepath.Join(root, "assets", "sub", "deep", "c.png"))
	u := newTestUpload(t, root, allWrite)

	listPaths := func(target string) ([]string, int) {
		c, w := newTestCtx("GET", target, "local:alice")
		u.ListAssets(c)
		var resp struct {
			Assets []AssetItem `json:"assets"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		var paths []string
		for _, a := range resp.Assets {
			paths = append(paths, a.Path)
		}
		return paths, w.Code
	}

	// 預設（無 dir）：只有 assets 根的直層檔案
	if paths, code := listPaths("/api/assets"); code != 200 || len(paths) != 1 || paths[0] != "assets/a.png" {
		t.Errorf("根目錄直層應只有 a.png，got code=%d paths=%v", code, paths)
	}
	// 指定子資料夾：只有該層檔案，不含更深層
	if paths, code := listPaths("/api/assets?dir=assets/sub"); code != 200 || len(paths) != 1 || paths[0] != "assets/sub/b.pdf" {
		t.Errorf("sub 直層應只有 b.pdf，got code=%d paths=%v", code, paths)
	}
	// assets 以外的路徑應拒絕
	if _, code := listPaths("/api/assets?dir=notes"); code != 403 {
		t.Errorf("assets 以外的 dir 應回 403，got %d", code)
	}
	// path traversal 應拒絕
	if _, code := listPaths("/api/assets?dir=assets/../secret"); code != 403 {
		t.Errorf("路徑跳脫應回 403，got %d", code)
	}
}

// TestListAssetFoldersVisibility 驗證資料夾樹的可見規則：可寫資料夾保留、
// 祖先資料夾穿透保留（標記不可寫）、無權限資料夾不出現。
func TestListAssetFoldersVisibility(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "assets", "team", "t.png"))
	mustWrite(t, filepath.Join(root, "assets", "other", "o.png"))
	perm := `{"default":"none","groups":{
	  "editors":{"members":["local:alice"],"rules":[{"path":"assets/team","access":"write"}]}
	}}`
	u := newTestUpload(t, root, perm)

	c, w := newTestCtx("GET", "/api/asset-folders", "local:alice")
	u.ListAssetFolders(c)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Folders []AssetFolder `json:"folders"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{} // path -> writable
	for _, f := range resp.Folders {
		got[f.Path] = f.Writable
	}
	if w, ok := got["assets"]; !ok || w {
		t.Errorf("assets 根應以祖先穿透保留且不可寫，got ok=%v writable=%v", ok, w)
	}
	if w, ok := got["assets/team"]; !ok || !w {
		t.Errorf("assets/team 應保留且可寫，got ok=%v writable=%v", ok, w)
	}
	if _, ok := got["assets/other"]; ok {
		t.Error("assets/other 無權限，不應出現")
	}
}

// TestRenameAsset 驗證附件改名：保留時間戳前綴、鎖副檔名、擋路徑分隔與 assets 以外路徑、同名衝突。
func TestRenameAsset(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "assets", "1719999999999_old-name.png"))
	mustWrite(t, filepath.Join(root, "assets", "1719999999998_taken.png"))
	mustWrite(t, filepath.Join(root, "assets", "1719999999998_dup.png")) // 與 taken 同時間戳前綴，供衝突測試
	mustWrite(t, filepath.Join(root, "assets", "manual.png")) // 手動放入、無時間戳前綴
	mustWrite(t, filepath.Join(root, "notes.md"))
	u := newTestUpload(t, root, allWrite)

	rename := func(p, newName string) (int, string) {
		c, w := newTestCtx("POST",
			"/api/asset/rename?path="+p+"&newName="+newName, "local:alice")
		u.RenameAsset(c)
		return w.Code, w.Body.String()
	}

	// 正常改名：時間戳前綴保留
	if code, body := rename("assets/1719999999999_old-name.png", "new-name.png"); code != 200 {
		t.Fatalf("改名應成功，got %d：%s", code, body)
	}
	if _, err := os.Stat(filepath.Join(root, "assets", "1719999999999_new-name.png")); err != nil {
		t.Error("改名後檔案應存在且保留時間戳前綴")
	}
	if _, err := os.Stat(filepath.Join(root, "assets", "1719999999999_old-name.png")); err == nil {
		t.Error("舊檔名不應殘留")
	}
	// 快取應已失效：清單反映新名稱
	entries, _ := u.store.ScanAssets()
	var seen bool
	for _, e := range entries {
		if e.Path == "assets/1719999999999_new-name.png" {
			seen = true
		}
	}
	if !seen {
		t.Error("改名後 ScanAssets 應看到新路徑")
	}

	// 無前綴的檔案：不強加前綴
	if code, body := rename("assets/manual.png", "renamed.png"); code != 200 {
		t.Fatalf("無前綴檔案改名應成功，got %d：%s", code, body)
	}
	if _, err := os.Stat(filepath.Join(root, "assets", "renamed.png")); err != nil {
		t.Error("無前綴檔案改名後應為純新檔名")
	}

	// 只改大小寫：大小寫不敏感檔案系統上不應誤判為同名衝突
	if code, body := rename("assets/renamed.png", "Renamed.png"); code != 200 {
		t.Fatalf("只改大小寫應成功，got %d：%s", code, body)
	}
	if _, err := os.Stat(filepath.Join(root, "assets", "Renamed.png")); err != nil {
		t.Error("大小寫改名後檔案應存在")
	}

	// 變更副檔名應拒絕
	if code, _ := rename("assets/1719999999998_taken.png", "evil.pdf"); code != 400 {
		t.Errorf("變更副檔名應回 400，got %d", code)
	}
	// 路徑分隔應拒絕（不可藉改名移動）
	if code, _ := rename("assets/1719999999998_taken.png", "sub%2Fx.png"); code != 400 {
		t.Errorf("含路徑分隔應回 400，got %d", code)
	}
	// assets 以外的檔案應拒絕
	if code, _ := rename("notes.md", "other.md"); code != 403 {
		t.Errorf("assets 以外應回 403，got %d", code)
	}
	// 同名衝突應回 409（保留來源時間戳前綴後與既有檔案同名）
	if code, _ := rename("assets/1719999999998_taken.png", "dup.png"); code != 409 {
		t.Errorf("目標已存在應回 409，got %d", code)
	}
}
