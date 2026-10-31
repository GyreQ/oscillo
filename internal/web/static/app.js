// 应用入口：hash 路由 + 分段导航（thumb 滑块）+ REC 状态 + Capture + 主题。
AI.views = AI.views || {};

(() => {
  const ROUTES = [
    { hash: "#/overview", label: "概览", match: (h) => h === "#/overview" || h === "" },
    { hash: "#/requests", label: "请求", match: (h) => h.startsWith("#/requests") || h.startsWith("#/replay") || h.startsWith("#/diff") || h.startsWith("#/turn/") },
    { hash: "#/sessions", label: "会话", match: (h) => h.startsWith("#/session") || h.startsWith("#/tsession") },
    { hash: "#/skills",   label: "技能", match: (h) => h.startsWith("#/skills") },
    { hash: "#/stats",    label: "统计", match: (h) => h.startsWith("#/stats") },
  ];

  let current = null; // 当前视图的 destroy（若有）

  // ---- 分段导航：当前视图的 thumb 滑过去（magpie 的 seg 语言） ----
  function renderNav(hash) {
    const nav = document.getElementById("nav");
    nav.innerHTML = `<span class="thumb"></span>` + ROUTES.map(r =>
      `<a class="${r.match(hash) ? "on" : ""}" href="${r.hash}">${r.label}</a>`).join("");
    requestAnimationFrame(() => moveThumb(nav));
  }

  function moveThumb(nav) {
    const thumb = nav.querySelector(".thumb");
    const on = nav.querySelector("a.on");
    if (!thumb || !on) return;
    thumb.style.left = on.offsetLeft + "px";
    thumb.style.width = on.offsetWidth + "px";
  }

  function destroyCurrent() {
    if (current && current.destroy) {
      try { current.destroy(); } catch {}
    }
    current = null;
  }

  async function route() {
    const hash = location.hash || "#/overview";
    const view = document.getElementById("view");

    // 旧版兼容：?view=stats / #stats → 新路由
    const qv = new URLSearchParams(location.search).get("view");
    if (qv) { location.search = ""; location.hash = "#/" + qv; return; }
    if (/^#\w/.test(hash) && !hash.startsWith("#/")) { location.hash = "#/" + hash.slice(1); return; }

    renderNav(hash);
    destroyCurrent();
    document.title = "ai-trace";

    try {
      if (hash === "#/overview" || hash === "") {
        current = AI.views.overview; await current.render(view);
        document.title = "概览 · Oscillo";
      } else if (hash.startsWith("#/replay/")) {
        current = AI.views.replay;
        await current.render(view, decodeURIComponent(hash.slice("#/replay/".length)));
        document.title = "回放 · Oscillo";
      } else if (hash.startsWith("#/requests")) {
        current = AI.views.requests; await current.render(view);
        document.title = "请求 · Oscillo";
      } else if (hash.startsWith("#/turn/")) {
        current = AI.views.turn;
        await current.render(view, decodeURIComponent(hash.slice("#/turn/".length)));
        document.title = "用户请求 · Oscillo";
      } else if (hash.startsWith("#/diff")) {
        current = AI.views.diff;
        await current.render(view, new URLSearchParams(hash.split("?")[1] || ""));
        document.title = "上下文对比 · Oscillo";
      } else if (hash.startsWith("#/tsession/")) {
        current = AI.views.tsession;
        await current.render(view, decodeURIComponent(hash.slice("#/tsession/".length)));
        document.title = "流量会话 · Oscillo";
      } else if (hash.startsWith("#/session/")) {
        const rest = decodeURIComponent(hash.slice("#/session/".length)).split("/");
        current = AI.views.session;
        await current.render(view, rest[0], rest.slice(1).join("/"));
        document.title = "会话回放 · Oscillo";
      } else if (hash.startsWith("#/sessions")) {
        current = AI.views.sessions; await current.render(view);
        document.title = "会话 · Oscillo";
      } else if (hash.startsWith("#/skills/")) {
        current = AI.views.skillDetail;
        await current.render(view, decodeURIComponent(hash.slice("#/skills/".length)));
        document.title = "技能 · Oscillo";
      } else if (hash.startsWith("#/skills")) {
        current = AI.views.skills; await current.render(view);
        document.title = "技能 · Oscillo";
      } else if (hash.startsWith("#/stats")) {
        current = AI.views.stats; await current.render(view);
        document.title = "统计 · Oscillo";
      } else {
        view.innerHTML = `<div class="view-inner"><div class="empty-state"><b>没有这个页面</b><a class="btn" href="#/overview">回概览</a></div></div>`;
      }
    } catch (e) {
      view.innerHTML = `<div class="view-inner"><div class="empty-state"><b>出错了</b>${AI.fmt.esc(e.message || e)}</div></div>`;
    }
  }

  // ---- REC 状态：SSE 连接 = 网关在线 ----
  function initRec() {
    const rec = document.getElementById("recState");
    const text = document.getElementById("recText");
    const foot = document.getElementById("footStatus");
    AI.live.onConn(on => {
      rec.classList.toggle("on", on);
      text.textContent = on ? "录制中" : "未连接";
      foot.textContent = on ? "网关在线 · 实时录制中" : "网关未连接（serve 未启动？）";
    });
    // 兜底探活：SSE 不可用时每 5s 探一次 healthz
    setInterval(async () => {
      if (AI.live.connected) return;
      try {
        const r = await fetch("/healthz");
        rec.classList.toggle("on", r.ok);
        text.textContent = r.ok ? "在线" : "未连接";
        foot.textContent = r.ok ? "服务在线（实时流未连接）" : "网关未连接";
      } catch {
        rec.classList.remove("on");
        text.textContent = "未连接";
        foot.textContent = "网关未连接（serve 未启动？）";
      }
    }, 5000);
  }

  // ---- Capture 开关（MITM 系统代理；桌面客户端主要入口，浏览器也可用） ----
  function initCapture() {
    const btn = document.getElementById("capBtn");
    if (!btn) return;
    const refresh = (on) => {
      btn.classList.toggle("on", on);
      btn.title = on ? "系统代理已指向 MITM（127.0.0.1:8788），点击关闭并还原" : "开启系统代理 MITM 模式，录制桌面应用流量";
    };
    AI.api.captureState().then(s => { btn.hidden = false; refresh(!!s.on); }).catch(() => {});
    btn.onclick = async () => {
      btn.classList.add("busy");
      try {
        const on = btn.classList.contains("on");
        if (on) {
          const r = await AI.api.captureOff();
          refresh(false);
          btn.textContent = "Capture";
          document.getElementById("footStatus").textContent =
            r.restored ? `已还原系统代理 → ${r.restored}` : "已关闭系统代理";
        } else {
          const r = await AI.api.captureOn();
          refresh(true);
          btn.textContent = "Capture";
          document.getElementById("footStatus").textContent =
            "系统代理 → 127.0.0.1:8788（首次使用需信任 CA，见 ai-trace capture on 输出）";
        }
      } catch (e) {
        document.getElementById("footStatus").textContent = "Capture 切换失败：" + (e.message || e);
      }
      btn.classList.remove("busy");
    };
  }

  // ---- 主题切换 ----
  function initTheme() {
    const btn = document.getElementById("themeBtn");
    btn.onclick = () => {
      const root = document.documentElement;
      const dark = root.getAttribute("data-theme") === "dark"
        || (!root.getAttribute("data-theme") && matchMedia("(prefers-color-scheme: dark)").matches);
      const next = dark ? "light" : "dark";
      root.setAttribute("data-theme", next);
      localStorage.setItem("ai-trace-theme", next);
    };
  }

  // ---- 桌面客户端识别（Wails 壳加载 ?desktop=1）----
  if (new URLSearchParams(location.search).get("desktop") === "1") {
    document.body.classList.add("desktop");
    // 顶栏 --wails-draggable 需要桥接脚本把拖拽意图发给 Wails 原生端
    // （页面是外部 URL，Wails runtime 不会自动加载，见 wails-drag.js 头注释）
    const s = document.createElement("script");
    s.src = "wails-drag.js";
    document.head.appendChild(s);
  }

  window.addEventListener("hashchange", route);
  window.addEventListener("resize", () => {
    const nav = document.getElementById("nav");
    if (nav) moveThumb(nav);
  });
  initRec();
  initCapture();
  initTheme();
  AI.live.start();
  route();
})();
