// 页内交互组件。全局命名空间 AI.ui。
// 桌面客户端的 WKWebView 不实现 window.confirm/alert（静默返回 false），
// 破坏性操作的确认一律走这里的页内弹窗，浏览器与桌面表现一致。
AI.ui = (() => {

  // 确认弹窗：resolve(true)=确认，resolve(false)=取消（Esc / 点遮罩也算取消）。
  // opts: { title, okText, cancelText, danger }，message 支持 \n 换行（pre-line）。
  function confirm(message, opts) {
    opts = opts || {};
    return new Promise(resolve => {
      const mask = document.createElement("div");
      mask.className = "modal-mask";
      mask.innerHTML = `
        <div class="modal" role="dialog" aria-modal="true">
          ${opts.title ? `<h3>${AI.fmt.esc(opts.title)}</h3>` : ""}
          <div class="modal-msg">${AI.fmt.esc(message)}</div>
          <div class="modal-actions">
            <button type="button" class="btn" data-cancel>${AI.fmt.esc(opts.cancelText || "取消")}</button>
            <button type="button" class="btn ${opts.danger ? "danger" : "primary"}" data-ok>${AI.fmt.esc(opts.okText || "确定")}</button>
          </div>
        </div>`;
      document.body.appendChild(mask);

      const done = (val) => {
        mask.classList.add("out");
        setTimeout(() => mask.remove(), 130);
        document.removeEventListener("keydown", onKey);
        resolve(val);
      };
      const onKey = (e) => { if (e.key === "Escape") done(false); };

      mask.querySelector("[data-cancel]").onclick = () => done(false);
      mask.querySelector("[data-ok]").onclick = () => done(true);
      mask.onmousedown = (e) => { if (e.target === mask) done(false); };
      document.addEventListener("keydown", onKey);
      // 焦点默认放在「取消」上：危险操作按回车不会误触发
      mask.querySelector("[data-cancel]").focus();
    });
  }

  return { confirm };
})();
