# 測試指南

## 日常綠燈套件

```bash
go test ./...
npm test
```

`npm test` 依序檢查前端 bundle、共編游標渲染，以及 `scripts/tests/` 下的 Node 邏輯測試；CI 也執行同一組命令。

## 已知缺陷紅燈套件

階段 0 先把尚未修復的規格放在獨立套件，避免日常 CI 直接變紅：

```bash
go test -tags knownbugs ./...
```

這個命令目前預期失敗。前端階段 1 與共編路徑階段 2 已修復的案例都已移到正式套件；後續階段每修好一項 Go 缺陷，就移除對應 build tag 並轉入日常測試。

目前記錄的紅燈規格包括：

- ACL 同群組內採最長前綴。
- 並發建立檔案只能有一個成功者。
- 回收筒 metadata 失敗時必須 rollback。
