// Package store 負責文件根目錄底下的路徑安全檢查、副檔名白名單與檔案樹建立。
package store

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
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
}

// New 建立繫結指定根目錄的 Store。
func New(root string) *Store {
	return &Store{Root: root}
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
