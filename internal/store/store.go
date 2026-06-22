// Package store 負責文件根目錄底下的路徑安全檢查、副檔名白名單與檔案樹建立。
package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// FileNode 代表檔案樹中的一個節點（資料夾或檔案）
type FileNode struct {
	Name     string      `json:"name"`               // 節點名稱（檔名或資料夾名）
	Path     string      `json:"path"`               // 相對於 DOC_ROOT 的路徑（使用 / 分隔）
	IsDir    bool        `json:"isDir"`              // 是否為資料夾
	Writable bool        `json:"writable"`           // 目前使用者是否可寫（由呼叫端依權限標記）
	Children []*FileNode `json:"children,omitempty"` // 子節點（僅資料夾有）
}

// Store 綁定文件根目錄，所有路徑操作都以此為基準。
type Store struct {
	Root string // DOC_ROOT 的絕對路徑

	// pathLocks 為每個檔案路徑一把鎖，序列化同一檔的「讀版本→比對→寫入」流程，
	// 避免並發寫入交錯（樂觀鎖的 TOCTOU）。鎖只增不減，數量上限為檔案數，可接受。
	muIndex   sync.Mutex
	pathLocks map[string]*sync.Mutex
}

// New 建立繫結指定根目錄的 Store。
func New(root string) *Store {
	return &Store{Root: root, pathLocks: map[string]*sync.Mutex{}}
}

// Lock 取得指定絕對路徑的專屬鎖；呼叫端取得後須在用畢時 Unlock。
// 用於把「讀取現有版本 → 比對 → 寫入」包成同一檔的臨界區。
func (s *Store) Lock(absPath string) *sync.Mutex {
	s.muIndex.Lock()
	mu, ok := s.pathLocks[absPath]
	if !ok {
		mu = &sync.Mutex{}
		s.pathLocks[absPath] = mu
	}
	s.muIndex.Unlock()
	mu.Lock()
	return mu
}

// AtomicWrite 以「寫入暫存檔再 rename」的方式原子地覆寫檔案，
// 避免並發讀者讀到半寫入的內容（os.WriteFile 非原子）。
func AtomicWrite(absPath string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(absPath)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// 任一步失敗都清掉暫存檔，避免殘留。
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	// 同目錄 rename 在 Windows / Linux 皆為原子操作。
	return os.Rename(tmpName, absPath)
}

// SafeResolve 將使用者傳入的相對路徑解析為絕對路徑，並驗證其確實位於 Root 底下，
// 以防止路徑穿越攻擊（path traversal，例如 ../../etc/passwd）。
func (s *Store) SafeResolve(userPath string) (string, error) {
	cleaned := strings.TrimSpace(userPath)
	cleaned = strings.ReplaceAll(cleaned, "\\", "/")
	if cleaned == "" {
		return "", os.ErrInvalid
	}

	joined := filepath.Join(s.Root, cleaned)
	absPath, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}

	// 核心安全檢查：確認最終路徑仍在 Root 範圍內（補分隔符避免 /docs 與 /docs-evil 誤判）
	rootWithSep := s.Root + string(os.PathSeparator)
	if absPath != s.Root && !strings.HasPrefix(absPath, rootWithSep) {
		return "", os.ErrPermission
	}
	return absPath, nil
}

// RelOf 由絕對路徑換算成相對 Root 的正規化路徑（/ 分隔、去頭尾 /），供權限比對使用。
func (s *Store) RelOf(absPath string) string {
	rel, err := filepath.Rel(s.Root, absPath)
	if err != nil {
		return ""
	}
	return strings.Trim(filepath.ToSlash(rel), "/")
}

// BuildTree 建立 Root 底下的檔案樹，回傳根節點（其 Children 即頂層項目）。
func (s *Store) BuildTree() (*FileNode, error) {
	return buildTree(s.Root, "")
}

// reservedNames 為 Windows 保留的裝置名稱（不分大小寫，含或不含副檔名皆視為非法）。
// 即使部署在 Linux，也一併禁止以維持跨平台一致並避免日後遷移踩雷。
var reservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// ValidateRelPath 檢查使用者提供之相對路徑的每一段檔名是否合法（跨平台安全）。
// 僅驗名稱字元；路徑穿越仍由 SafeResolve 負責。
func ValidateRelPath(p string) error {
	p = strings.ReplaceAll(p, "\\", "/")
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		if err := validateName(seg); err != nil {
			return err
		}
	}
	return nil
}

// validateName 驗證單一路徑分段（檔名或資料夾名）。
func validateName(name string) error {
	if len(name) > 255 {
		return errors.New("檔名過長（單段上限 255 位元組）")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return errors.New("檔名不可包含控制字元")
		}
		switch r {
		case '<', '>', ':', '"', '|', '?', '*':
			return fmt.Errorf("檔名不可包含字元 %q", r)
		}
	}
	// 首尾空白、結尾的點：在 Windows 會被靜默修剪，易造成混淆或繞過比對。
	if strings.TrimSpace(name) != name {
		return errors.New("檔名首尾不可有空白")
	}
	if strings.HasSuffix(name, ".") {
		return errors.New("檔名結尾不可為點")
	}
	// Windows 保留裝置名：比對去除副檔名後的主檔名。
	base := name
	if i := strings.IndexByte(name, '.'); i >= 0 {
		base = name[:i]
	}
	if reservedNames[strings.ToLower(base)] {
		return fmt.Errorf("檔名為系統保留名稱：%s", name)
	}
	return nil
}

// isAllowedFile 判斷檔案副檔名是否為允許的文件類型（.md 或 .txt）
func IsAllowedFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".md" || ext == ".txt"
}

// imageExts 為允許上傳的圖片副檔名（不含 .svg，避免內嵌指令碼造成 XSS 風險）
var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true,
}

// attachExts 為允許上傳的附件副檔名
var attachExts = map[string]bool{
	".pdf": true, ".zip": true, ".csv": true, ".txt": true, ".md": true,
	".doc": true, ".docx": true, ".xls": true, ".xlsx": true, ".ppt": true, ".pptx": true,
}

// IsImageExt 判斷是否為圖片副檔名
func IsImageExt(ext string) bool {
	return imageExts[strings.ToLower(ext)]
}

// IsAllowedUpload 判斷是否為允許上傳的副檔名（圖片或附件）
func IsAllowedUpload(ext string) bool {
	ext = strings.ToLower(ext)
	return imageExts[ext] || attachExts[ext]
}

// buildTree 遞迴建立指定目錄的檔案樹。
// dirPath 為目前掃描的絕對路徑，relPath 為相對於 Root 的路徑。
func buildTree(dirPath, relPath string) (*FileNode, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, err
	}

	node := &FileNode{
		Name:     filepath.Base(dirPath),
		Path:     relPath,
		IsDir:    true,
		Children: []*FileNode{},
	}

	for _, entry := range entries {
		name := entry.Name()
		// 略過隱藏檔案與資料夾（以 . 開頭）
		if strings.HasPrefix(name, ".") {
			continue
		}
		// 略過自動管理的附件目錄 assets（不在檔案樹中呈現）
		if entry.IsDir() && name == "assets" {
			continue
		}

		childRel := name
		if relPath != "" {
			childRel = relPath + "/" + name
		}
		childAbs := filepath.Join(dirPath, name)

		if entry.IsDir() {
			child, err := buildTree(childAbs, childRel)
			if err != nil {
				return nil, err
			}
			node.Children = append(node.Children, child)
		} else if IsAllowedFile(name) {
			node.Children = append(node.Children, &FileNode{
				Name:  name,
				Path:  childRel,
				IsDir: false,
			})
		}
	}

	// 排序：資料夾在前、檔案在後，同類型再依名稱排序
	sort.Slice(node.Children, func(i, j int) bool {
		a, b := node.Children[i], node.Children[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})

	return node, nil
}
