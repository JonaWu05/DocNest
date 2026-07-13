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
npm run test:known-bugs
```

這兩個命令目前預期失敗。後續階段每修好一項，就移除對應 Go build tag，或把前端案例移到 `scripts/tests/` 的正式套件，讓它成為永久回歸測試。

目前記錄的紅燈規格包括：

- 共編 init 逾時必須安全退回單機模式。
- 非同步共編存檔成功前必須保留 pending 狀態。
- 等價文件路徑必須共用同一個共編房間。
- ACL 同群組內採最長前綴。
- 並發建立檔案只能有一個成功者。
- 回收筒 metadata 失敗時必須 rollback。
