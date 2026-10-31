// 概览视图「观测台」：今日 KPI（spark + 环比）→ 左 LIVE 实时流（主角）+ 右最近请求。
AI.views = AI.views || {};
AI.views.overview = (() => {
  const F = AI.fmt;
  let el = null, offLive = null, recentEl = null, refreshTimer = 0;
  let lastRecs = []; // 供 KPI spark 复用（live 完成后刷新）

  async function render(container) {
    el = container;
    offLive = AI.live.onUpdate(onLiveChange);
    const [stats, yStats, recs] = await Promise.all([
      AI.api.stats(1),
      AI.api.stats(2).catch(() => null),
      AI.api.records({ day: F.today() }).catch(() => ({ records: [] })),
    ]);
    lastRecs = recs.records || [];

    const t = stats.totals;
    const yday = yesterdayBucket(yStats);
    const recsChrono = [...lastRecs].reverse(); // 时间正序

    container.innerHTML = `
      <div class="view-inner">
        <div class="view-h">
          <h1>概览</h1>
          <span class="sub">今天发生了什么 · 网关录制 + 本地日志</span>
          ${t.errors ? `<a class="btn danger" style="margin-left:auto" href="#/requests?status=err">${F.num(t.errors)} 个错误 →</a>` : ""}
        </div>

        <div class="stats mb">
          ${kpi(F.num(t.requests), "请求",
            F.spark(recsChrono.map(r => r.usage ? (r.usage.input + r.usage.output) : 0)),
            deltaLine(t.requests, yday && yday.requests, F.num) || (t.requests ? `${F.num(t.tools)} 次工具调用` : "尚无流量"))}
          ${kpi(F.tok(t.input + t.output), "Token",
            F.spark(cumSeries(recsChrono.map(r => r.usage ? (r.usage.input + r.usage.output) : 0))),
            deltaLine(t.input + t.output, yday ? yday.input + yday.output : null, F.tok)
              || `${F.tok(t.input)} 输入 · ${F.tok(t.output)} 输出`)}
          ${kpi(F.money(t.costUsd), "成本", "",
            t.costUsd ? (t.hasPrice ? "按内置价格表估算" : "部分模型无价格，实际更高")
              : (t.hasPrice ? "今天 0 成本" : "部分模型无价格"))}
          ${kpi(F.num(t.errors), "错误", "",
            t.requests ? (t.errors ? "点击右上角查看" : "一切正常") : "—", t.errors ? "down" : "")}
        </div>

        <div class="grid-2" style="grid-template-columns:minmax(0,1.15fr) minmax(0,1fr)">
          <div class="card">
            <div class="card-h">实时<span class="hint">正在录制的请求，token 逐字出现</span></div>
            <div id="liveCard"></div>
          </div>
          <div class="list">
            <div class="row head" style="justify-content:space-between">
              <b>最近请求</b>
              <a class="text" href="#/requests">查看全部</a>
            </div>
            <div id="recentList"></div>
          </div>
        </div>
      </div>`;
    recentEl = container.querySelector("#recentList");
    renderRecent(lastRecs);
    renderLive();
  }

  function kpi(v, l, spark, sub, tone) {
    return `<div class="kpi">
      <div class="kpi-top"><b>${v}</b>${spark}</div>
      <span>${l}</span><small class="${tone || ""}">${sub || ""}</small>
    </div>`;
  }

  // 环比昨天（无昨日数据返回 null，回落到普通说明文案）
  function deltaLine(cur, prev, fmt) {
    const d = F.delta(cur, prev, fmt);
    return d ? `<span class="${d.cls === "warn" ? "warn" : d.cls}">${d.text.replace(" vs 上一周期", " vs 昨天")}</span>` : null;
  }

  // 累计曲线（token KPI 的 spark 用）
  function cumSeries(vals) {
    let s = 0;
    return vals.map(v => (s += v || 0));
  }

  function yesterdayBucket(twoDayStats) {
    if (!twoDayStats || !twoDayStats.byDay) return null;
    const d = new Date(Date.now() - 86400_000), p = x => String(x).padStart(2, "0");
    const key = `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
    return twoDayStats.byDay.find(b => b.key === key) || null;
  }

  // ---- 最近请求：紧凑列表（时间 / agent·模型·工具 / 耗时·输出）----
  function renderRecent(recs) {
    if (!recentEl) return;
    if (!recs.length) {
      recentEl.innerHTML = `<div class="empty-state"><b>今天还没有请求</b>
        新开终端运行 <code>ai-trace run claude</code>，流量出现后会立即显示在这里</div>`;
      return;
    }
    recentEl.innerHTML = recs.slice(0, 14).map(r => {
      const noise = !r.agent || r.agent === "unknown";
      const tools = F.toolList(r.toolCalls);
      const out = r.usage ? r.usage.output : null;
      const err = r.error || (r.status >= 400);
      return `
      <div class="row click${err ? " err" : ""}${noise ? " noise" : ""}" data-id="${r.id}">
        <span class="mono dim" style="flex:none">${F.time(r.time)}</span>
        <div class="who">
          <div class="name">${noise ? '<span class="dim">未识别</span>' : F.esc(F.agentName(r.agent))}${r.subagent ? ' <span class="badge">子代理</span>' : ""}
            ${err ? F.statusBadge(r.status, r.error) : ""}</div>
          <div class="sub">${F.esc(r.model || "—")}${tools ? " · " + F.esc(tools) : ""}</div>
        </div>
        <div class="sess-num"><b>${F.dur(r.durationMs)}</b>${out != null ? `<span class="sub">${F.tok(out)} 出</span>` : ""}</div>
      </div>`;
    }).join("");
    recentEl.querySelectorAll(".row[data-id]").forEach(row => {
      row.onclick = () => { location.hash = "#/replay/" + row.dataset.id; };
    });
  }

  // ---- LIVE 面板（签名时刻）----
  function renderLive() {
    const box = el && el.querySelector("#liveCard");
    if (!box) return;
    const actives = AI.live.actives();
    const dones = AI.live.dones().filter(d => !d.record || d.record.error).slice(0, 2);

    if (!actives.length && !dones.length) {
      box.innerHTML = `<div class="empty-state" style="padding:26px">现在没有正在录制的请求。
        ${AI.live.connected ? "网关在线，流量出现即显示。" : "网关未连接——到「请求」看历史记录"}</div>`;
      return;
    }

    box.innerHTML = actives.map(liveItem).join("") +
      dones.map(d => {
        const r = d.record || {};
        return `<div class="live-item done" data-id="${d.id}">
          <div class="live-head">
            <span class="dot err"></span>
            <b>${F.esc(F.agentName(d.agent))}</b>
            <span class="mono">${F.esc(d.model || r.model || "")}</span>
            ${r.error ? F.statusBadge(r.status, r.error) : F.statusBadge(r.status)}
            <span class="t">${F.dur(r.durationMs || (d.doneAt - d.startAt))}</span>
          </div></div>`;
      }).join("");

    box.querySelectorAll(".live-item[data-id]").forEach(item => {
      item.onclick = () => { location.hash = "#/replay/" + item.dataset.id; };
    });
  }

  function liveItem(r) {
    const secs = (Date.now() - r.startAt) / 1000;
    const body = (r.text || r.thinking || "");
    const shown = body.length > 600 ? body.slice(-600) : body; // 只展示尾部
    const meta = [];
    if (r.thinking) meta.push(`<span class="badge">思考 ${F.tok(r.thinking.length)} 字</span>`);
    if (r.tools.length) {
      meta.push(r.tools.map(t =>
        `<span class="badge acc">${F.esc(t.name || "工具")}${t.args ? " · 参数传入中" : ""}</span>`).join(" "));
    }
    if (r.usage) meta.push(`<span class="badge">已报告 ${F.tok(r.usage.output || 0)} 出</span>`);
    return `<div class="live-item">
      <div class="live-head">
        <span class="badge live"><span class="dot live"></span>录制中</span>
        <b>${F.esc(F.agentName(r.agent))}</b>
        <span class="mono">${F.esc(r.model || "")}</span>
        <span class="t">${secs.toFixed(0)}s · ${r.evCount} 事件</span>
      </div>
      <div class="live-body selectable">${F.esc(shown)}</div>
      ${meta.length ? `<div class="live-meta">${meta.join("")}</div>` : ""}
    </div>`;
  }

  function destroy() {
    if (offLive) offLive();
    clearTimeout(refreshTimer);
  }

  // live 变化：LIVE 面板立即刷新；若有请求完成，稍等账本落盘再刷新最近列表
  function onLiveChange() {
    if (!el || document.hidden) return;
    renderLive();
    if (AI.live.dones().some(d => Date.now() - d.doneAt < 2000)) {
      clearTimeout(refreshTimer);
      refreshTimer = setTimeout(async () => {
        try {
          const recs = await AI.api.records({ day: F.today() });
          lastRecs = recs.records || [];
          renderRecent(lastRecs);
        } catch {}
      }, 1500);
    }
  }

  return { render, destroy };
})();
