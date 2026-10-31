// 主题：跟随系统，手动切换存 localStorage。在 CSS 加载前执行避免闪烁。
(() => {
  const saved = localStorage.getItem("ai-trace-theme");
  if (saved === "light" || saved === "dark") {
    document.documentElement.setAttribute("data-theme", saved);
  }
})();
