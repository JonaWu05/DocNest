// 共用的 Markdown 解析：以目前文件內容 lex 一次並快取，供預覽渲染與 TOC 共用。
// 避免每次內容變更時各自全文解析（原本 preview 的 marked.parse 與 toc 的 marked.lexer
// 會各跑一趟 lexer）。內容未變時回傳同一份 token 串列。
import { state } from "./state.js";

let cachedContent = null;
let cachedTokens = null;

// 取得目前文件內容的 token 串列；內容與上次相同時回傳快取，避免重複 lex。
// 回傳的陣列保留 marked 掛在其上的 links 屬性，供 marked.parser 解析參照式連結。
export function currentTokens() {
  const content = state.currentContent || "";
  if (content === cachedContent && cachedTokens) return cachedTokens;
  cachedTokens = marked.lexer(content);
  cachedContent = content;
  return cachedTokens;
}
