// codemirror 匯入的 shim：y-codemirror 只在游標渲染(awareness)用到 CodeMirror.Pos。
// EasyMDE 已自帶 CodeMirror 5 實例,為避免把整包 CM5 重複打進 bundle 並與 EasyMDE 的實例衝突,
// 打包時以本檔取代 'codemirror' 匯入。優先用全域 CodeMirror;無則提供最小 Pos(CM5 接受 {line,ch} 物件)。
const CM =
  typeof window !== "undefined" && window.CodeMirror
    ? window.CodeMirror
    : { Pos: function (line, ch) { return { line, ch }; } };

export default CM;
