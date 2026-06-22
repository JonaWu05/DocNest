package main

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
	Writable bool        `json:"writable"`           // 目前使用者是否可寫（供前端隱藏編輯/刪除操作；由 filterTree 標記）
	Children []*FileNode `json:"children,omitempty"` // 子節點（僅資料夾有）
}

// safeResolve 將使用者傳入的相對路徑解析為絕對路徑，並驗證其確實位於 DOC_ROOT 底下，
// 以防止路徑穿越攻擊（path traversal，例如 ../../etc/passwd）。
// 回傳清理後的絕對路徑；若不合法則回傳錯誤。
func safeResolve(userPath string) (string, error) {
	// 統一斜線方向，並去除前後空白
	cleaned := strings.TrimSpace(userPath)
	cleaned = strings.ReplaceAll(cleaned, "\\", "/")

	// 不允許空路徑
	if cleaned == "" {
		return "", os.ErrInvalid
	}

	// 將相對路徑接到根目錄後，再用 filepath.Clean 正規化（會解析掉 ./ 與 ../）
	joined := filepath.Join(docRoot, cleaned)
	absPath, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}

	// 核心安全檢查：確認最終路徑仍在 DOC_ROOT 範圍內
	// 使用前綴比對，並補上分隔符避免 /docs 與 /docs-evil 這類誤判
	rootWithSep := docRoot + string(os.PathSeparator)
	if absPath != docRoot && !strings.HasPrefix(absPath, rootWithSep) {
		return "", os.ErrPermission
	}

	return absPath, nil
}

// isAllowedFile 判斷檔案副檔名是否為允許的文件類型（.md 或 .txt）
func isAllowedFile(name string) bool {
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

// isImageExt 判斷是否為圖片副檔名
func isImageExt(ext string) bool {
	return imageExts[strings.ToLower(ext)]
}

// isAllowedUpload 判斷是否為允許上傳的副檔名（圖片或附件）
func isAllowedUpload(ext string) bool {
	ext = strings.ToLower(ext)
	return imageExts[ext] || attachExts[ext]
}

// buildTree 遞迴建立指定目錄的檔案樹。
// dirPath 為目前掃描的絕對路徑，relPath 為相對於 DOC_ROOT 的路徑。
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
		// 略過自動管理的附件目錄 assets（內含上傳的圖片/附件，不在檔案樹中呈現）
		if entry.IsDir() && name == "assets" {
			continue
		}

		// 組出子項目的相對路徑（統一使用 / 分隔，方便前端與 API 使用）
		childRel := name
		if relPath != "" {
			childRel = relPath + "/" + name
		}
		childAbs := filepath.Join(dirPath, name)

		if entry.IsDir() {
			// 資料夾：遞迴處理後一律納入（含空資料夾，方便剛建立的資料夾立即顯示）
			child, err := buildTree(childAbs, childRel)
			if err != nil {
				return nil, err
			}
			node.Children = append(node.Children, child)
		} else if isAllowedFile(name) {
			// 檔案：僅納入允許的副檔名
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
			return a.IsDir // 資料夾排前面
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})

	return node, nil
}
