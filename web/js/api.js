// 後端 API 呼叫的共用封裝。
import { API_BASE } from "./state.js";
import { authFetch, ensureOk } from "./auth.js";

// uploadFile 上傳檔案（圖片 / 附件）的共用函式。
// dir 為 assets 樹底下的目的地資料夾；未給時後端預設存到根 assets（拖放 / 貼上）。
export async function uploadFile(file, dir) {
  const fd = new FormData();
  fd.append("file", file);
  if (dir !== undefined && dir !== null) fd.append("dir", dir);
  const res = await authFetch(API_BASE + "/api/upload", { method: "POST", body: fd });
  return (await ensureOk(res)).json(); // { path（相對 DOC_ROOT）, name, isImage }
}
