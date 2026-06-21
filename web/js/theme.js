// 深色 / 淺色主題切換。
import { state } from "./state.js";
import { themeBtn } from "./dom.js";

export function applyTheme(theme) {
  const dark = theme === "dark";
  document.body.classList.toggle("dark", dark);
  themeBtn.innerHTML = dark ? '<i class="fa fa-sun-o"></i>' : '<i class="fa fa-moon-o"></i>';
  localStorage.setItem("theme", dark ? "dark" : "light");
  // EasyMDE 已建立時刷新，使編輯器顏色立即套用
  if (state.easyMDE) setTimeout(() => state.easyMDE.codemirror.refresh(), 0);
}
