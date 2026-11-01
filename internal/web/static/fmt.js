// 格式化与转义工具。全局命名空间 AI.fmt。
window.AI = window.AI || {};
AI.fmt = (() => {

  // HTML 转义：所有动态插值必须经过这里
  function esc(s) {
    return String(s ?? "").replace(/[&<>"']/g, c => ({
      "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
    }[c]));
  }

  // 千分位整数
  function num(n) {
    if (n == null || isNaN(n)) return "—";
    return Number(n).toLocaleString("en-US");
  }

  // token 数：1234 → 1.2k；5600000 → 5.6M
  function tok(n) {
    if (n == null || isNaN(n)) return "—";
    n = Number(n);
    if (Math.abs(n) >= 1e6) return (n / 1e6).toFixed(n % 1e6 ? 1 : 0) + "M";
    if (Math.abs(n) >= 1000) return (n / 1000).toFixed(n % 1000 ? 1 : 0) + "k";
    return String(n);
  }

  // 毫秒 → 人话：840 → 840ms；2900 → 2.9s；75000 → 1m15s
  function dur(ms) {
    if (ms == null) return "—";
    if (ms < 1000) return Math.round(ms) + "ms";
    if (ms < 60_000) return (ms / 1000).toFixed(ms < 10_000 ? 1 : 0) + "s";
    const m = Math.floor(ms / 60_000), s = Math.round((ms % 60_000) / 1000);
    return m + "m" + String(s).padStart(2, "0") + "s";
  }

  // 金额：小到 4 位小数，大到 2 位
  function money(v) {
    if (v == null || isNaN(v)) return "—";
    if (v === 0) return "$0";
    if (v < 0.01) return "$" + v.toFixed(4);
    if (v < 1) return "$" + v.toFixed(3);
    return "$" + v.toFixed(2);
  }

  // ISO 时间 → HH:mm:ss（本地时区）
  function time(iso) {
    if (!iso) return "—";
    const d = new Date(iso);
    if (isNaN(d)) return "—";
    return [d.getHours(), d.getMinutes(), d.getSeconds()]
      .map(x => String(x).padStart(2, "0")).join(":");
  }

  // ISO 时间 → MM-dd HH:mm
  function datetime(iso) {
    if (!iso) return "—";
    const d = new Date(iso);
    if (isNaN(d)) return "—";
    const p = x => String(x).padStart(2, "0");
    return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
  }

  // 相对时间："3 分钟前"
  function ago(iso) {
    if (!iso) return "—";
    const s = (Date.now() - new Date(iso).getTime()) / 1000;
    if (isNaN(s)) return "—";
    if (s < 60) return Math.max(1, Math.round(s)) + " 秒前";
    if (s < 3600) return Math.round(s / 60) + " 分钟前";
    if (s < 86400) return Math.round(s / 3600) + " 小时前";
    return Math.round(s / 86400) + " 天前";
  }

  // 字节数
  function bytes(n) {
    if (n == null) return "—";
    if (n >= 1e6) return (n / 1e6).toFixed(1) + "MB";
    if (n >= 1e3) return (n / 1e3).toFixed(1) + "KB";
    return n + "B";
  }

  // 状态标记：正常 = 安静的小点（title 挂状态码），错误 = 红徽章（title 挂全文）。
  // 全表 200 绿徽章是噪音——安静正常，才能让异常被看见。
  function statusBadge(status, err) {
    if (err) return `<span class="badge err" title="${esc(String(err))}">${esc(String(err).slice(0, 60))}</span>`;
    if (status == null) return '<span class="dim">—</span>';
    if (status >= 400) return `<span class="badge err">${status}</span>`;
    return `<span class="dot ok" title="${status}"></span>`;
  }

  // 工具调用去重计数：[{name}] → "WebSearch ×3、Read"。重复罗列同名工具没有信息量。
  function toolList(calls, max) {
    if (!calls || !calls.length) return null;
    const counts = new Map();
    for (const c of calls) {
      const n = c.name || "工具";
      counts.set(n, (counts.get(n) || 0) + 1);
    }
    const parts = [...counts.entries()].slice(0, max || 3)
      .map(([n, c]) => c > 1 ? `${n} ×${c}` : n);
    const more = counts.size - (max || 3);
    return parts.join("、") + (more > 0 ? ` 等${counts.size}种` : "");
  }

  // 百分比：a / b → 整数百分比字符串（b 为 0 返回 null）
  function pct(a, b) {
    if (!b) return null;
    return Math.round(a / b * 100) + "%";
  }

  // KPI 环比：返回 {text, cls}（cur vs prev）。prev 无数据返回 null。
  function delta(cur, prev, fmt) {
    fmt = fmt || num;
    if (prev == null || prev === 0) return null;
    const d = cur - prev;
    if (d === 0) return { text: "与上一周期持平", cls: "" };
    const p = Math.abs(Math.round(d / prev * 100));
    const arrow = d > 0 ? "↑" : "↓";
    return { text: `${arrow} ${fmt(Math.abs(d))}（${p}%）vs 上一周期`, cls: d > 0 ? "warn" : "up" };
  }

  // sparkline：数值序列 → 内联 SVG 折线（面积微填充）。空序列返回空串。
  function spark(vals, w, h) {
    w = w || 64; h = h || 20;
    const n = (vals || []).filter(v => v != null && !isNaN(v));
    if (n.length < 2) return "";
    const max = Math.max(...n, 1), min = Math.min(...n, 0);
    const span = max - min || 1;
    const pts = n.map((v, i) => {
      const x = i / (n.length - 1) * (w - 2) + 1;
      const y = h - 2 - (v - min) / span * (h - 4);
      return x.toFixed(1) + "," + y.toFixed(1);
    });
    return `<svg class="spark" width="${w}" height="${h}" viewBox="0 0 ${w} ${h}" aria-hidden="true">` +
      `<polyline points="${pts.join(" ")}" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"/>` +
      `</svg>`;
  }

  // stop reason 人话
  function stopReason(s) {
    const map = {
      end_turn: "正常结束", max_tokens: "达到长度上限", stop_sequence: "命中停止词",
      tool_use: "请求调用工具", aborted: "已中断",
    };
    return map[s] || s || "—";
  }

  // agent 显示名：专有名词保留原文，国产客户端用中文名。
  // unknown 单独处理成"未识别"——MITM 抓到的浏览器杂流量长这样，
  // 与其展示英文不如明确告诉用户"这条认不出来"。
  function agentName(a) {
    const map = {
      "claude-code": "Claude Code",
      "claude-desktop": "Claude Desktop",
      codex: "Codex",
      "gemini-cli": "Gemini CLI",
      cursor: "Cursor",
      opencode: "OpenCode",
      zcode: "ZCode",
      workbuddy: "WorkBuddy",
      pi: "Pi Agent",
      codebuddy: "CodeBuddy",
      aider: "Aider",
      cline: "Cline",
      "roo-cline": "Roo Cline",
      goose: "Goose",
      "kimi-cli": "Kimi CLI",
      "qwen-code": "Qwen Code",
      quark: "夸克",
      doubao: "豆包",
      curl: "curl（手动测试）",
    };
    if (a === "unknown" || a === "undefined" || !a) return "未识别";
    return map[a] || a;
  }

  // agent 图标：圆角色块 + 应用意象图形（SVG 线稿或字符），色相按应用区分。
  // 会话页的应用 Tab 和列表行共用。未收录的应用取显示名首字母，
  // 颜色从固定色盘按 id 稳定散列——同一应用每次渲染颜色一致。
  // 24 viewBox、stroke 2.1：缩到 12px 仍清晰（线宽等比 1.05px）。
  const AGENT_GLYPHS = {
    "claude-code": {
      c: "#d97757", // Claude 日芒 · 陶土橙
      svg: '<path d="M12 4.2v15.6M4.2 12h15.6M6.6 6.6l10.8 10.8M17.4 6.6 6.6 17.4" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round"/>',
    },
    "claude-desktop": {
      c: "#c2603f",
      svg: '<path d="M12 4.2v15.6M4.2 12h15.6M6.6 6.6l10.8 10.8M17.4 6.6 6.6 17.4" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round"/>',
    },
    codex: { // OpenAI 绿 · 终端提示符 >_
      c: "#10a37f",
      svg: '<path d="M6.5 7 11 11.5 6.5 16M12.5 17h5" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round"/>',
    },
    zcode: { // Z
      c: "#8b5cf6",
      svg: '<path d="M6.8 7.2h10.4L6.8 16.8h10.4" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round"/>',
    },
    workbuddy: { // 对话气泡
      c: "#0aa0a8",
      svg: '<path d="M12 4.6c4.4 0 8 2.8 8 6.3s-3.6 6.3-8 6.3c-1 0-1.98-.14-2.9-.4L5.4 18.9l1-3.1C4.9 14.6 4 12.9 4 10.9c0-3.5 3.6-6.3 8-6.3z" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round"/>',
    },
    pi: { t: "π", c: "#b4530a" },
    "gemini-cli": { // 四角星
      c: "#4285f4",
      svg: '<path d="M12 3c.5 5 4 8.5 9 9-5 .5-8.5 4-9 9-.5-5-4-8.5-9-9 5-.5 8.5-4 9-9z" fill="currentColor"/>',
    },
    cursor: { // 指针
      c: "#64748b",
      svg: '<path d="M6.3 4.4 18 10.9c.45.25.36.92-.15 1.02l-4.72.95 2.7 5.1a.55.55 0 0 1-.23.74l-1.98.94a.55.55 0 0 1-.73-.24l-2.7-5.1-3.55 3.55z" fill="currentColor"/>',
    },
    doubao: { t: "豆", c: "#3b82f6" },
    quark: { t: "夸", c: "#5b5bd6" },
    unknown: { t: "?", c: "#7c8090" },
  };
  const ICON_HUES = ["#0e9bd8", "#8b5cf6", "#0f9d58", "#e0970f", "#d63434", "#2e6bf5", "#0aa0a8", "#b4530a"];

  function agentIcon(a, lg) {
    const g = AGENT_GLYPHS[a] || (!a || a === "undefined" ? AGENT_GLYPHS.unknown : null);
    let inner, c;
    if (g) {
      inner = g.svg ? `<svg viewBox="0 0 24 24" aria-hidden="true">${g.svg}</svg>` : `<i>${g.t}</i>`;
      c = g.c;
    } else {
      // 兜底：显示名首字母 + 按 id 散列取色
      let h = 0;
      for (const ch of String(a)) h = (h * 31 + ch.codePointAt(0)) | 0;
      c = ICON_HUES[Math.abs(h) % ICON_HUES.length];
      inner = `<i>${esc([...agentName(a)][0].toUpperCase())}</i>`;
    }
    return `<span class="agent-ic${lg ? " lg" : ""}" style="--ic:${c}" aria-hidden="true">${inner}</span>`;
  }

  // 今天（本地时区）→ YYYY-MM-DD
  function today() {
    const d = new Date(), p = x => String(x).padStart(2, "0");
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
  }

  // 尝试美化 JSON（失败原样返回）。后端 json.RawMessage 是内嵌对象而非字符串。
  function prettyJSON(s) {
    if (s == null) return "";
    if (typeof s === "object") {
      try { return JSON.stringify(s, null, 2); } catch { return String(s); }
    }
    if (!s) return "";
    try { return JSON.stringify(JSON.parse(s), null, 2); } catch { return s; }
  }

  return { esc, num, tok, dur, money, time, datetime, ago, bytes, statusBadge, toolList, pct, delta, spark, stopReason, agentName, agentIcon, today, prettyJSON };
})();
