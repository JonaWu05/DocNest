# 測試指南

## 日常綠燈套件

```bash
go test ./...
npm test
```

`npm test` 依序檢查前端 bundle、共編游標渲染，以及 `scripts/tests/` 下的 Node 邏輯測試；CI 也執行同一組命令。

## 已知缺陷規格（目前已清空）

階段 0 先把尚未修復的規格放在獨立套件，避免日常 CI 直接變紅：

```bash
go test -tags knownbugs ./...
```

階段 1 至 4 的已知缺陷都已修復並移到正式套件；目前沒有獨立的紅燈規格。

後續若新增尚未修復的規格，可再以 `knownbugs` build tag 隔離；修復後應移回日常測試，避免回歸。
