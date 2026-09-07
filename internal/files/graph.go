// 結構圖（GET /api/graph）：掃描文件間的站內相對連結，回傳整個文件庫的節點與邊，供前端畫圖。
package files

import (
	"io"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/JonaWu05/DocNest/internal/httpx"
	"github.com/JonaWu05/DocNest/internal/store"
)

// graphLinkRE 比對 Markdown 連結語法 [文字](目標)，用來從文件內容抽取指向其他文件的相對連結。
var graphLinkRE = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)

// maxGraphScanSize 為單一文件供連結掃描讀取的上限，避免異常大檔拖慢整體掃描。
const maxGraphScanSize = 2 << 20 // 2 MB

// graphRootPath 是虛擬「根目錄」節點的 path 鍵值，代表 DOC_ROOT 本身，與 permissions.json
// 用空字串代表根目錄的既有慣例一致（真正的檔案/資料夾 path 不可能是空字串）。
const graphRootPath = ""

// GraphNode 為結構圖中的一個節點：一份可讀的文件，或一個（依讀取權可見的）資料夾。
// 根目錄本身也是一個節點（Path 為 graphRootPath），讓所有頂層項目都有所屬、圖不會斷成孤島。
// Title 可能為空字串（無標題），前端沿用檔案樹既有的顯示名稱規則（title 優先、否則去副檔名的
// 檔名）補上。
type GraphNode struct {
	Path  string `json:"path"`
	Title string `json:"title"`
	IsDir bool   `json:"isDir"`
}

// GraphEdge 為結構圖中的一條邊：
//   - Type "contain"：Target 是 Source 資料夾底下的直接子項目（資料夾或檔案）。
//   - Type "link"：Source 文件內有一個指向 Target 文件的站內相對連結。
//
// 前端可依 Type 分開繪製樣式，讓「真連結」（使用者主動建立、可能跨資料夾）比「單純歸屬同一
// 資料夾」更醒目，突顯圖比純資料夾樹多出來的資訊。
type GraphEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Type   string `json:"type"`
}

// Graph 處理 GET /api/graph：回傳整個文件庫（依讀取權過濾後）的節點與邊，供前端畫結構圖。
// 節點含資料夾與檔案；邊含「資料夾歸屬」與「文件間站內連結」兩種。
//
// 節點與邊都先套用與 ListFiles 相同的讀取權過濾（filterTree），不會讓使用者透過圖的結構
// 得知無權限檔案/資料夾的存在。index.md 不納入圖：它在 UI 慣例上代表所在資料夾首頁、預設對
// 使用者不可見，其資訊已經由所在資料夾節點本身承載。
//
// 連結解析比照前端 relativeFromDocDir 的慣例，一律視為「相對於來源文件所在資料夾」。解析結果
// 只要不落在「可讀檔案節點」集合內（不存在、無權限、越界、資料夾皆屬此類）就捨棄該邊——
// 圖不需要區分捨棄原因，使用者也看不到。
func (f *Files) Graph(c *gin.Context) {
	tree, err := f.store.CachedTree()
	if err != nil {
		httpx.ServerError(c, "無法讀取文件目錄", err)
		return
	}
	filtered := f.filterTree(tree.Children, c)

	nodes := []GraphNode{{Path: graphRootPath, Title: "根目錄", IsDir: true}}
	allowed := map[string]bool{} // 可被連結指向的節點（僅檔案），供 scanLinks 判斷連結目標合法性
	containEdges := []GraphEdge{}
	collectGraphNodes(filtered, graphRootPath, &nodes, allowed, &containEdges)

	edges := containEdges
	for _, n := range nodes {
		if n.IsDir {
			continue
		}
		edges = append(edges, f.scanLinks(n.Path, allowed)...)
	}

	c.JSON(http.StatusOK, gin.H{"nodes": nodes, "edges": edges})
}

// collectGraphNodes 遞迴走訪已過濾的檔案樹，收集資料夾與檔案節點；每個節點都會對其直接上層
// （parentPath，頂層代入 graphRootPath）記一條 contain 邊。同時記下 allowed 集合，
// 供 scanLinks 判斷連結目標是否為圖裡的合法節點。
func collectGraphNodes(fileNodes []*store.FileNode, parentPath string, nodes *[]GraphNode, allowed map[string]bool, containEdges *[]GraphEdge) {
	for _, n := range fileNodes {
		if n.IsDir {
			*nodes = append(*nodes, GraphNode{Path: n.Path, Title: n.Title, IsDir: true})
			*containEdges = append(*containEdges, GraphEdge{Source: parentPath, Target: n.Path, Type: "contain"})
			collectGraphNodes(n.Children, n.Path, nodes, allowed, containEdges)
			continue
		}
		if n.IsIndex {
			continue // index.md 代表所在資料夾首頁，其資訊已由資料夾節點本身承載，圖裡不重複出現
		}
		*nodes = append(*nodes, GraphNode{Path: n.Path, Title: n.Title})
		*containEdges = append(*containEdges, GraphEdge{Source: parentPath, Target: n.Path, Type: "contain"})
		allowed[n.Path] = true
	}
}

// scanLinks 讀取單一文件內容，抽出其中指向其他「可讀 .md 節點」的相對連結，組成邊。
// 讀取或解析失敗（檔案消失、越界等）一律回傳空結果，不中斷整體掃描。
func (f *Files) scanLinks(sourceRel string, allowed map[string]bool) []GraphEdge {
	absPath, err := f.store.SafeResolve(sourceRel)
	if err != nil {
		return nil
	}
	data, err := readCapped(absPath, maxGraphScanSize)
	if err != nil {
		return nil
	}

	sourceDir := path.Dir(sourceRel)
	var edges []GraphEdge
	seen := map[string]bool{} // 同一來源對同一目標只算一條邊，即使內文連結多次
	for _, m := range graphLinkRE.FindAllStringSubmatch(string(data), -1) {
		target := m[1]
		if strings.Contains(target, "://") || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
			continue // 外部網址、頁內錨點、mailto 都不算站內文件連結
		}
		target = strings.SplitN(target, "#", 2)[0] // 去掉錨點片段（如 a.md#section）
		if target == "" {
			continue
		}
		combined := target
		if sourceDir != "." {
			combined = sourceDir + "/" + target
		}
		targetAbs, err := f.store.SafeResolve(combined)
		if err != nil {
			continue
		}
		targetRel := f.store.RelOf(targetAbs)
		if targetRel == sourceRel || !allowed[targetRel] || seen[targetRel] {
			continue
		}
		seen[targetRel] = true
		edges = append(edges, GraphEdge{Source: sourceRel, Target: targetRel, Type: "link"})
	}
	return edges
}

// readCapped 讀取檔案內容，超過上限時仍只回傳前段（連結多半在文件前面就能掃到，非嚴謹需求）。
func readCapped(absPath string, limit int64) ([]byte, error) {
	file, err := os.Open(absPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, limit))
}
