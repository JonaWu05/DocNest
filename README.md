# Markdown 協作編輯器

[![CI](https://github.com/JonaWu05/DocNest/actions/workflows/ci.yml/badge.svg)](https://github.com/JonaWu05/DocNest/actions/workflows/ci.yml)

以 Go 為後端、原生 JavaScript（ES 模組）為前端的自架 Markdown 文件系統，支援即時多人協作、線上狀態、儲存衝突偵測與權限登入。

## 功能

- **Markdown 編輯**：以 [EasyMDE](https://github.com/Ionaru/easy-markdown-editor) 為編輯器，提供「預覽 / 編輯 / 分割」三種模式、語法工具列、深色模式。
- **檔案管理**：左側樹狀檢視，支援新增 / 重新命名 / 移動 / 刪除檔案與資料夾，並可調整側欄寬度。
- **附件與圖片**：拖放或貼上即可上傳，內建附件庫管理，連結自動換算為相對路徑。
- **文件目錄（TOC）**：依標題自動產生，點擊跳轉。
- **匯出 PDF**：透過瀏覽器列印對話框另存為 PDF。
- **即時協作**：
  - 線上狀態（Presence）顯示誰在線、誰正在看或編輯哪個檔案；點「在線 N 人」可展開成員清單。
  - 他人儲存檔案時即時通知；正在編輯者可選擇載入最新或保留自己的版本。
  - **儲存衝突偵測（樂觀鎖）**：避免兩人同時編輯時後者覆蓋前者的變更。
- **登入驗證**：本地帳號（bcrypt）與 Discord OAuth，統一簽發 JWT。

## 技術棧

| | |
|---|---|
| 後端 | Go 1.26、[gin](https://github.com/gin-gonic/gin)、[gorilla/websocket](https://github.com/gorilla/websocket)、[golang-jwt](https://github.com/golang-jwt/jwt) |
| 前端 | 原生 JavaScript（ES 模組）、EasyMDE、[marked](https://github.com/markedjs/marked)、[DOMPurify](https://github.com/cure53/DOMPurify)、FontAwesome |
| 即時 | WebSocket（單一 goroutine 的 Hub 模式管理連線）|

> 前端第三方相依一律由後端 `/static/vendor` 在地提供（pinned 版本），不依賴外部 CDN。


## 快速開始

需求：Go 1.26+。

```bash
# 1. 取得程式碼
git clone https://github.com/JonaWu05/DocNest.git
cd DocNest

# 2. 建立設定檔
cp .env.example .env
cp accounts.example.json accounts.json

# 3. 產生帳號密碼的 bcrypt hash，填入 accounts.json 的 local
go run ./cmd/hashpw '你的密碼'

# 4. 編輯 .env 設定 JWT_SECRET；編輯 accounts.json 填入帳號（見下方設定說明）

# 5. 啟動
go run .
```

啟動後瀏覽 <http://localhost:8080>。文件預設存放於 `./docs`（`DOC_ROOT`）。

## 設定（.env）

| 變數 | 必填 | 說明 |
|---|---|---|
| `JWT_SECRET` | ✅ | JWT 簽章密鑰；產生：`openssl rand -base64 32` |
| `DOC_ROOT` | — | 文件根目錄，預設 `./docs` |
| `PORT` | — | 服務埠號，預設 `8080` |
| `APP_TITLE` | — | 網頁標題（瀏覽器分頁與登入頁大標），預設「Markdown 編輯器」|
| `LOGIN_BG` | — | 登入頁自訂背景圖；留空則用內建暗色漸層。可填 `/static/...` 路徑或外部圖片 URL |
| `DEFAULT_DOC` | — | 登入後自動開啟的首頁文件（相對 `DOC_ROOT`）|
| `JWT_EXPIRE_HOURS` | — | JWT 有效時數，預設 `24` |
| `ALLOWED_ORIGINS` | — | 允許的跨來源網域（CORS 與 WebSocket 共用），逗號分隔；留空為開發模式（允許所有來源）|
| `TRUSTED_PROXIES` | — | 信任的反向代理來源（IP 或 CIDR，逗號分隔）；架在反向代理後方時設定，才能取得真實客戶端 IP。留空為不信任任何代理 |
| `HOST` | — | 服務綁定位址；留空＝綁所有介面（`0.0.0.0`）。開發可設 `127.0.0.1` 僅本機 |
| `DISCORD_CLIENT_ID` / `DISCORD_CLIENT_SECRET` / `DISCORD_REDIRECT_URI` | — | Discord OAuth（選填，全部設定才啟用）；登入白名單見 `accounts.json` |
| `ACCOUNTS_FILE` / `PERMISSIONS_FILE` | — | 帳號 / 權限設定檔路徑，預設 `./accounts.json`、`./permissions.json` |

> `.env` 含密鑰，已列入 `.gitignore`，請勿提交。`docs/` 為執行期資料（等同各自的資料庫目錄），亦不納入版控，僅保留 `welcome.md` 作為範本。

## 帳號與權限（accounts.json / permissions.json）

- **`accounts.json`**：本地帳號（`local`：`帳號` → bcrypt hash）與 Discord 登入白名單（`discord_allowed`：User ID 陣列）。範本為 `accounts.example.json`；產生密碼 hash：`go run ./cmd/hashpw '你的密碼'`。
- **`permissions.json`**：群組 + 路徑前綴的存取控制。範本為 `permissions.example.json`；名為 `admins` 群組的成員即**管理員**（可進入管理頁；無此檔則無任何管理員）。
- **管理頁 `/admin`**：管理員登入後點右上角「管理」進入，可**免手改檔、免重啟**地新增／刪除本地帳號、重設密碼、增減 Discord 白名單、把成員指派到既有群組；變動即時生效。頁面以認證 cookie 做伺服器端守門，非管理員無法進入。仍可用頁內「重載設定」重新載入手動改過的檔案。
- **自助改密碼**：本地帳號登入後可點右上角「改密碼」修改自己的密碼（需驗舊密碼）。
- 兩檔均含部署資料（密碼 hash、成員身分），已列入 `.gitignore`，請勿提交。


## 專案結構

```
.
├── main.go              # 進入點：載入設定、建立各服務（DI）、設定 gin 與路由、啟動
├── internal/
│   ├── config/          # 設定載入（環境變數）
│   ├── store/           # 路徑安全、副檔名白名單、檔案樹、原子寫檔
│   ├── authz/           # 權限分組判斷 + 群組成員編輯（可熱重載）
│   ├── auth/            # JWT、登入限流、Local/Discord 登入、帳號管理、cookie 守門、/admin API
│   ├── hub/             # WebSocket Hub（presence / 即時通知）
│   ├── collab/          # 即時共編房間（Yjs update / awareness 中繼）
│   ├── files/           # 檔案 CRUD、原始檔服務、資源回收筒
│   ├── filewatch/       # 外部改檔偵測（輪詢目前開著的檔）
│   ├── upload/          # 附件上傳與列舉
│   └── httpx/           # HTTP 共用小工具
├── cmd/hashpw/          # 產生 bcrypt 密碼 hash 的小工具
└── web/                 # 前端（index.html、admin.html、styles.css、js/ 模組、vendor/ 在地相依）
```



## UniEntry 模式

DocNest 預設維持獨立的 `standalone` 認證。內網部署可改由 UniEntry 集中登入與授權：

```env
AUTH_MODE=portal
PORTAL_URL=http://172.24.15.21:8081
JWT_SECRET=<與 UniEntry 相同>
```

Portal 模式不讀取 `accounts.json` 或 `permissions.json`，也不提供本地帳號、Discord、密碼及群組管理 API。DocNest 只驗證 UniEntry JWT，並讀取 `app_permissions["docnest"]`：

- `pages.read` / `pages.write`：整個文件根目錄。
- `pages.read:<路徑前綴>` / `pages.write:<路徑前綴>`：指定路徑及其後代。
- `pages.write` 隱含讀取；未知、格式錯誤或缺少的權限一律拒絕。

請同時在 UniEntry `apps.yaml` 登記 DocNest 的完整網址，並讓瀏覽器以相同 host（可不同 port）存取兩個服務，才能共享 host-only 的 `auth_token` cookie。權限異動後，既有 JWT 需重新登入或等待到期才會取得新 claims。

[MIT License](LICENSE) 
