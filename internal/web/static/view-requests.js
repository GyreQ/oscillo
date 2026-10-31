// 请求页双模式：「用户请求」= 一轮用户输入触发的 N 次模型调用（用户视角，
// 默认）；「模型调用」= 每行一次 LLM API 请求（模型侧排障视图）。
// 状态表达反转：成功 = 安静小点，错误 = 红徽章 + 行 rail；未识别流量整行退层。
// 支持 #/requests?status=err 直达（概览页错误入口）。
AI.views = AI.views || {};
AI.views.requests = (() => {
  const F = AI.fmt;
  const state = {
    mode: localStorage.getItem("ai-trace-req-mode") || "turns",
    day: F.today(), agent: "", status: "", q: "",
  };
  let el = null, allRecs = [], allTurns = [], totalCount = 0;

  async function render(container) {
    el = container;
    // hash 查询参数可作为初始过滤（#/requests?status=err）
    const hq = new URLSearchParams((location.hash.split("?")[1] || ""));
    if (hq.get("status")) state.status = hq.get("status");
    const [daysResp] = await Promise.all([AI.api.days().catch(() => ({ days: [] }))]);
    const days = daysResp.days || [];
    window.__daysCache = days; // renderAll 的日期下拉用
    if (!days.includes(state.day)) state.day = days[0] || F.today();
    await load();
  }

  async function load() {
    if (state.mode === "turns") {
      const resp = await AI.api.turns({ day: state.day }).catch(() => ({ turns: [] }));
      allTurns = resp.turns || [];
    } else {
      const resp = await AI.api.records({ day: state.day }).catch(() => ({ records: [] }));
      allRecs = resp.records || [];
      totalCount = resp.total != null ? resp.total : allRecs.length;
    }
    renderAll();
  }

  function setMode(m) {
    if (state.mode === m) return;
    state.mode = m;
    localStorage.setItem("ai-trace-req-mode", m);
    // 状态过滤语义随模式切换：轮次只有 err，数字状态码对轮次无意义
    if (m === "turns" && state.status && state.status !== "err") state.status = "";
    load();
  }

  function agents() {
    const src = state.mode === "turns" ? allTurns.map(t => t.agent) : allRecs.map(r => r.agent);
    return [...new Set(src.filter(Boolean))];
  }

  // 未识别记录数（清理入口的出现条件与提示文案用）
  function noiseCount() {
    return state.mode === "turns"
      ? allTurns.filter(t => !t.agent || t.agent === "unknown").length
      : allRecs.filter(r => !r.agent || r.agent === "unknown").length;
  }

  function filterActive() {
    return !!(state.q.trim() || state.agent || state.status);
  }

  function renderAll() {
    if (!el) return;
    const days = window.__daysCache || [];
    const turns = state.mode === "turns";
    const noise = noiseCount();

    el.innerHTML = `
      <div class="view-inner">
        <div class="view-h">
          <h1>请求</h1>
          <span class="sub">${turns
            ? "用户视角：一行 = 一次用户输入（实际触发多次模型调用），点行看整轮链路"
            : "模型视角：网关录制的每一次 LLM 调用 · 点击行看完整回放"}</span>
          <button type="button" class="btn danger" id="resetAll" style="margin-left:auto" title="只删除录制数据（请求记录、请求体、流量统计），技能沉淀、配置与证书保留">清空历史</button>
        </div>
        <div class="filters">
          <div class="mode-seg" id="modeSeg">
            <button type="button" data-m="turns" class="${turns ? "on" : ""}">用户请求</button>
            <button type="button" data-m="calls" class="${turns ? "" : "on"}">模型调用</button>
          </div>
          <input type="text" id="fq" placeholder="搜索用户输入、会话、错误信息…" value="${F.esc(state.q)}">
          <select id="fday">${days.map(d => `<option value="${d}" ${d === state.day ? "selected" : ""}>${d}</option>`).join("")}</select>
          <select id="fagent">
            <option value="">全部 agent</option>
            ${agents().map(a => `<option value="${F.esc(a)}" ${a === state.agent ? "selected" : ""}>${F.esc(F.agentName(a))}${a === "unknown" ? `（${noise}）` : ""}</option>`).join("")}
          </select>
          <select id="fstatus">
            <option value="">全部状态</option>
            <option value="err" ${state.status === "err" ? "selected" : ""}>仅错误</option>
            ${turns ? "" : ["200", "400", "401", "429", "500"].map(s =>
              `<option value="${s}" ${state.status === s ? "selected" : ""}>${s}</option>`).join("")}
          </select>
          <span class="filter-count" id="fcount"></span>
        </div>
        <div class="list" id="reqList"></div>
        ${!turns && totalCount > allRecs.length ? `<p class="trunc-note">仅显示前 ${allRecs.length} 条，共 ${F.num(totalCount)} 条——用过滤条件缩小范围</p>` : ""}
      </div>`;

    bind();
    rerender();
  }

  function filtered() {
    if (state.mode === "turns") {
      const q = state.q.trim().toLowerCase();
      return allTurns.filter(t => {
        if (state.agent && t.agent !== state.agent) return false;
        if (state.status === "err" && !t.errors) return false;
        if (q) {
          const hay = [t.text, t.session, t.agent].join(" ").toLowerCase();
          if (!hay.includes(q)) return false;
        }
        return true;
      });
    }
    const q = state.q.trim().toLowerCase();
    return allRecs.filter(r => {
      if (state.agent && r.agent !== state.agent) return false;
      if (state.status === "err" && !(r.error || (r.status >= 400))) return false;
      if (state.status && state.status !== "err" && String(r.status) !== state.status) return false;
      if (q) {
        const hay = [r.path, r.model, r.session, r.agent, r.error].join(" ").toLowerCase();
        if (!hay.includes(q)) return false;
      }
      return true;
    });
  }

  // 耗时比例条的基准：本批最大耗时（跨数量级可比）
  let durMax = 1;
  function tableHTML(recs) {
    durMax = Math.max(...recs.map(r => r.durationMs || 0), 1);
    return `<div class="tbl-wrap"><table class="tbl"><thead><tr>
      <th>时间</th><th>Agent</th><th>模型</th><th>状态</th>
      <th class="r">耗时</th><th class="r">首字节</th><th class="r">输入</th><th class="r">输出</th><th>工具</th>
    </tr></thead><tbody>
    ${recs.map(r => {
      const u = r.usage;
      const prompt = u ? (u.input + u.cache_read + u.cache_write) : null;
      const noise = !r.agent || r.agent === "unknown";
      const err = r.error || r.status >= 400;
      const tools = F.toolList(r.toolCalls);
      return `<tr data-id="${r.id}"${err ? ' class="err"' : ""}${noise ? ' class="noise"' : ""}>
        <td class="mono dim">${F.time(r.time)}</td>
        <td>${noise ? '<span class="dim">未识别</span>' : F.esc(F.agentName(r.agent))}${r.subagent ? ' <span class="badge">子代理</span>' : ""}</td>
        <td class="mono dim">${F.esc(r.model || "—")}</td>
        <td>${F.statusBadge(r.status, r.error)}</td>
        <td class="r">${meterHTML(r.durationMs)} ${F.dur(r.durationMs)}</td>
        <td class="r dim">${r.ttftMs ? F.dur(r.ttftMs) : "—"}</td>
        <td class="r">${F.tok(prompt)}</td>
        <td class="r">${F.tok(u ? u.output : null)}</td>
        <td class="dim">${tools ? F.esc(tools) : "—"}</td>
      </tr>`;
    }).join("")}
    </tbody></table></div>`;
  }

  function meterHTML(ms) {
    if (!ms) return "";
    const w = Math.min(100, ms / durMax * 100);
    const cls = ms > 60_000 ? "warn" : "";
    return `<span class="meter"><i class="${cls}" style="width:${w.toFixed(0)}%"></i></span>`;
  }

  // 用户请求轮次行：与 会话 页的 .row 同款布局
  function turnRowHTML(t) {
    const u = t.usage || {};
    const err = t.errors > 0;
    const noise = !t.agent || t.agent === "unknown";
    const tools = (t.tools || []).join("、");
    return `<div class="row click${err ? " err" : ""}${noise ? " noise" : ""}" data-turn="${F.esc(t.id)}">
      <div class="who">
        <div class="name">${F.esc(t.text || "（无用户文本）")}${t.subagent ? ' <span class="badge">子代理</span>' : ""}</div>
        <div class="sub">${F.time(t.time)} · ${noise ? '<span class="dim">未识别</span>' : F.esc(F.agentName(t.agent))}
          · ${t.calls} 次调用${tools ? " · " + F.esc(tools) : ""}</div>
      </div>
      <div class="sess-num">
        ${err ? `<span class="badge err">${t.errors} 次失败</span>` : ""}
        <b>${F.tok(u.input + u.cache_read + u.cache_write)} 入 / ${F.tok(u.output)} 出</b>
        <div class="sub">${F.dur(t.durationMs)}</div>
      </div>
    </div>`;
  }

  function turnsHTML(turns) {
    return turns.length ? turns.map(turnRowHTML).join("")
      : `<div class="empty-state"><b>没有用户请求</b>
        ${allTurns.length ? "调整过滤条件试试" : `这一天没有录制数据。新开终端运行 <code>ai-trace run claude</code>`}</div>`;
  }

  function bind() {
    el.querySelectorAll("#modeSeg button").forEach(b => {
      b.onclick = () => setMode(b.dataset.m);
    });
    const q = el.querySelector("#fq");
    q.oninput = () => { state.q = q.value; rerender(); };
    el.querySelector("#fday").onchange = async (e) => {
      state.day = e.target.value;
      await load();
    };
    el.querySelector("#fagent").onchange = (e) => { state.agent = e.target.value; rerender(); };
    el.querySelector("#fstatus").onchange = (e) => { state.status = e.target.value; rerender(); };
    // 行点击走事件委托：rerender 重建行后无需重绑
    const card = el.querySelector("#reqList");
    card.onclick = (e) => {
      const turn = e.target.closest("[data-turn]");
      if (turn) { location.hash = `#/turn/${encodeURIComponent(turn.dataset.turn)}`; return; }
      const tr = e.target.closest("tr[data-id]");
      if (tr) { location.hash = `#/replay/${encodeURIComponent(tr.dataset.id)}`; }
    };
    // 一键清空全部录制数据（破坏性操作，页内弹窗二次确认——WKWebView 无原生 confirm）
    const rst = el.querySelector("#resetAll");
    if (rst) rst.onclick = async () => {
      const ok = await AI.ui.confirm(
        "清空全部录制记录？\n\n" +
        "将删除：所有请求记录、请求体、流量会话统计。\n" +
        "将保留：技能沉淀、配置、CA 证书、系统代理与 WorkBuddy 接管状态。\n" +
        "此操作不可恢复，确定重头再来？",
        { title: "清空历史", okText: "全部清空", danger: true });
      if (!ok) return;
      rst.textContent = "清空中…";
      rst.disabled = true;
      try {
        await AI.api.reset();
        location.hash = "#/requests";
        location.reload();
      } catch (e) {
        rst.textContent = "清空历史";
        rst.disabled = false;
        const foot = document.getElementById("footStatus");
        if (foot) foot.textContent = "清空失败：" + (e.message || e);
      }
    };
  }

  // 表尾：清理未识别流量（破坏性操作，安静放置，不与真实调用抢注意力）
  function bindCleanNoise() {
    const card = el.querySelector("#reqList");
    const noise = noiseCount();
    const hasRows = state.mode === "turns" ? allTurns.length : allRecs.length;
    if (!hasRows || !noise) return;
    const noun = state.mode === "turns" ? "轮" : "条";
    card.insertAdjacentHTML("beforeend", `<div class="row" style="min-height:0;padding:7px 14px;justify-content:flex-end">
      <button type="button" class="text clean-noise" title="删除系统代理误录的浏览器/App 流量（agent 为未识别的全部日期记录），统计随之修正">清理 ${noise} ${noun}未识别流量</button>
    </div>`);
    const clean = card.querySelector(".clean-noise");
    clean.onclick = async () => {
      clean.disabled = true;
      clean.textContent = "清理中…";
      try {
        const r = await AI.api.purge("unknown");
        const foot = document.getElementById("footStatus");
        if (foot) foot.textContent = `已清理 ${r.deleted} 条未识别流量，统计已同步`;
        if (state.agent === "unknown") state.agent = ""; // 该过滤项已不存在
        await load();
      } catch (e) {
        const foot = document.getElementById("footStatus");
        if (foot) foot.textContent = "清理失败：" + (e.message || e);
        clean.disabled = false;
        clean.textContent = `清理 ${noise} ${noun}未识别流量`;
      }
    };
  }

  // 不重建整页，只重画列表主体与计数（输入框不丢焦点）
  function rerender() {
    const card = el.querySelector("#reqList");
    if (!card) return;
    const count = el.querySelector("#fcount");
    if (state.mode === "turns") {
      const turns = filtered();
      if (count) count.textContent = `共 ${turns.length} 轮`;
      card.innerHTML = turnsHTML(turns);
    } else {
      const recs = filtered();
      if (count) count.textContent = `共 ${recs.length} 条`;
      card.innerHTML = recs.length ? tableHTML(recs)
        : `<div class="empty-state"><b>没有匹配的请求</b>
          ${allRecs.length ? "调整过滤条件试试" : `这一天没有录制数据。新开终端运行 <code>ai-trace run claude</code>`}</div>`;
    }
    bindCleanNoise();
  }

  return { render, setDay: (d) => { state.day = d; return load(); } };
})();
