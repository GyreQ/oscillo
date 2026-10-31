// 统计视图「成本分析」：时间范围切换 + KPI（环比）+ 按日 SVG 双段柱图 + 分组占比表 + 工具排名。
AI.views = AI.views || {};
AI.views.stats = (() => {
  const F = AI.fmt;
  const RANGES = [
    { label: "今天", days: 1 },
    { label: "7 天", days: 7 },
    { label: "30 天", days: 30 },
    { label: "全部", days: 0 },
  ];
  let el = null, days = 30, tip = null;

  async function render(container) {
    el = container;
    await reload();
  }

  async function reload() {
    // days*2 一次拿两个周期，KPI 环比用（"全部"无法对比）
    const [s, s2] = await Promise.all([
      AI.api.stats(days).catch(() => null),
      days ? AI.api.stats(days * 2).catch(() => null) : Promise.resolve(null),
    ]);
    if (!s || !s.totals) {
      el.innerHTML = `<div class="view-inner"><div class="empty-state"><b>统计加载失败</b></div></div>`;
      return;
    }
    const t = s.totals;
    const prev = prevTotals(s2);

    el.innerHTML = `
      <div class="view-inner">
        <div class="view-h">
          <h1>统计</h1>
          <span class="sub">按内置价格表估算成本（可用 ~/.ai-trace/prices.json 覆盖）</span>
          <div class="chips" style="margin-left:auto">
            ${RANGES.map(r => `<button class="chip ${r.days === days ? "on" : ""}" data-days="${r.days}">${r.label}</button>`).join("")}
          </div>
        </div>

        <div class="stats mb">
          <div class="kpi"><div class="kpi-top"><b>${F.num(t.requests)}<span class="u">次</span></b></div><span>请求</span>
            <small class="${t.errors ? "down" : ""}">${kpiDelta(t.requests, prev && prev.requests, F.num)
              || (t.errors ? `${t.errors} 次错误 · ${Math.round(t.errors / Math.max(t.requests, 1) * 100)}%` : "没有错误")}</small></div>
          <div class="kpi"><div class="kpi-top"><b>${F.tok(t.input + t.output)}</b></div><span>Token</span>
            <small>${kpiDelta(t.input + t.output, prev ? prev.input + prev.output : null, F.tok)
              || `${F.tok(t.input)} 入 · ${F.tok(t.output)} 出 · ${F.tok(t.cacheRead)} 命中缓存`}</small></div>
          <div class="kpi"><div class="kpi-top"><b>${F.money(t.costUsd)}</b></div><span>成本</span>
            <small>${kpiDelta(t.costUsd, prev ? prev.costUsd : null, F.money)
              || (t.hasPrice ? "已含全部模型价格" : "部分模型无价格，实际更高")}</small></div>
          <div class="kpi"><div class="kpi-top"><b>${F.num(t.tools)}<span class="u">次</span></b></div><span>工具调用</span>
            <small>${s.tools && s.tools.length ? `最常用 ${F.esc(s.tools[0].name)} ×${s.tools[0].count}` : "—"}</small></div>
        </div>

        <div class="chart mb">
          <div class="chart-h"><b>按日用量</b>
            <span class="hint">每根 = 输入（石板）+ 输出（金）· 悬停看明细</span>
          </div>
          ${chartHTML(fillDays(s.byDay || []))}
        </div>

        <div class="grid-2">
          <div class="list">
            <div class="row head"><b>按模型</b></div>
            ${bucketTable(s.byModel || [], false)}
          </div>
          <div class="list">
            <div class="row head"><b>按 agent</b></div>
            ${bucketTable(s.byAgent || [], true)}
          </div>
        </div>

        ${(s.tools || []).length ? `
        <div class="list mt">
          <div class="row head"><b>工具频次</b><span class="sub" style="margin-left:auto">Top 10</span></div>
          <div class="rank">
            ${(s.tools || []).slice(0, 10).map(toolRank).join("")}
          </div>
        </div>` : ""}
      </div>`;

    el.querySelectorAll(".chip").forEach(c => {
      c.onclick = async () => { days = +c.dataset.days; await reload(); };
    });
    bindChart();
  }

  // 上一周期汇总（从 days*2 的按日数据里切）；凑不齐返回 null
  function prevTotals(s2) {
    if (!s2 || !s2.byDay || !s2.byDay.length) return null;
    const cutoff = new Date(Date.now() - days * 86400_000);
    const p = new Date(Date.now() - 2 * days * 86400_000);
    const inPrev = s2.byDay.filter(b => {
      const d = new Date(b.key + "T00:00:00");
      return d >= p && d < cutoff;
    });
    if (!inPrev.length) return null;
    return inPrev.reduce((a, b) => ({
      requests: a.requests + b.requests,
      input: a.input + b.input,
      output: a.output + b.output,
      costUsd: a.costUsd + b.costUsd,
    }), { requests: 0, input: 0, output: 0, costUsd: 0 });
  }

  function kpiDelta(cur, prev, fmt) {
    const d = F.delta(cur, prev, fmt);
    return d ? `<span class="${d.cls}">${d.text}</span>` : null;
  }

  // ---- 按日图：手写 SVG，y 轴刻度 + 网格 + hover 列高亮 ----------------------
  // 补零：选中范围内的无数据日期也占一列（30 天就是 30 根，不再"看着像 7 天"）
  function fillDays(byDay) {
    const map = new Map(byDay.map(b => [b.key, b]));
    const out = [];
    if (!byDay.length) return out;
    const fmtKey = d => {
      const p = x => String(x).padStart(2, "0");
      return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
    };
    if (!days) { // 全部：首尾之间补零
      const first = new Date(byDay[byDay.length - 1].key + "T00:00:00");
      const last = new Date(byDay[0].key + "T00:00:00");
      for (let d = new Date(first); d <= last; d.setDate(d.getDate() + 1)) {
        out.push(map.get(fmtKey(d)) || zeroDay(fmtKey(d)));
      }
    } else {
      for (let i = days - 1; i >= 0; i--) {
        const d = new Date(Date.now() - i * 86400_000);
        out.push(map.get(fmtKey(d)) || zeroDay(fmtKey(d)));
      }
    }
    return out;
  }

  function zeroDay(key) {
    return { key, requests: 0, errors: 0, input: 0, output: 0, costUsd: 0 };
  }

  // 好看的 y 轴上限：1/2/5 × 10^k 向上取整
  function niceMax(v) {
    if (v <= 5) return 5;
    const p = Math.pow(10, Math.floor(Math.log10(v)));
    for (const m of [1, 2, 5, 10]) {
      if (m * p >= v) return m * p;
    }
    return 10 * p;
  }

  function chartHTML(byDay) {
    if (byDay.length < 2) {
      return `<div class="empty-state">单日范围没有趋势——切到更长的时间范围看按日曲线</div>`;
    }
    const W = 1000, H = 190;
    const padL = 46, padR = 6, padT = 8, padB = 20;
    const plotW = W - padL - padR, plotH = H - padT - padB;
    const max = niceMax(Math.max(...byDay.map(d => d.input + d.output), 1));
    const colW = plotW / byDay.length;
    const barW = Math.min(colW * .62, 30);
    const y = v => padT + plotH - (v / max) * plotH;

    // 网格 + y 刻度（4 条）
    let grid = "";
    for (let i = 0; i <= 4; i++) {
      const v = max / 4 * i, gy = y(v);
      grid += `<line class="cgrid" x1="${padL}" x2="${W - padR}" y1="${gy}" y2="${gy}"/>
        <text class="cylab" x="${padL - 6}" y="${gy + 3}" text-anchor="end">${F.tok(v)}</text>`;
    }
    // 柱（双段：输入在下，输出在上；输出段保底 2.2 可见）+ hover 命中区
    let bars = "", hits = "";
    byDay.forEach((d, i) => {
      const cx = padL + colW * i + colW / 2;
      const tot = d.input + d.output;
      const hTot = tot / max * plotH;
      const hOut = d.output ? Math.min(Math.max(d.output / max * plotH, 2.2), hTot) : 0;
      const hIn = Math.max(hTot - hOut, 0);
      bars += `<rect class="cbar-out" x="${(cx - barW / 2).toFixed(1)}" y="${(padT + plotH - hTot).toFixed(1)}" width="${barW.toFixed(1)}" height="${hOut.toFixed(1)}" rx="1.5"/>`;
      bars += `<rect class="cbar-in" x="${(cx - barW / 2).toFixed(1)}" y="${(padT + plotH - hTot + hOut).toFixed(1)}" width="${barW.toFixed(1)}" height="${hIn.toFixed(1)}" rx="1.5"/>`;
      hits += `<rect class="chit" x="${(padL + colW * i).toFixed(1)}" y="${padT}" width="${colW.toFixed(1)}" height="${plotH}"
        data-k="${d.key}" data-in="${d.input}" data-out="${d.output}" data-req="${d.requests}" data-err="${d.errors}"/>`;
    });
    // x 刻度：最多 6 个，均匀取
    let xlabs = "";
    const step = Math.max(1, Math.round(byDay.length / 6));
    byDay.forEach((d, i) => {
      if (i % step === 0 || i === byDay.length - 1) {
        xlabs += `<text class="cxlab" x="${(padL + colW * i + colW / 2).toFixed(1)}" y="${H - 6}" text-anchor="middle">${F.esc(d.key.slice(5))}</text>`;
      }
    });
    return `<svg class="chart-plot grow" viewBox="0 0 ${W} ${H}">${grid}${bars}${hits}${xlabs}</svg>`;
  }

  function bindChart() {
    tip = document.getElementById("chartTip") || makeTip();
    el.querySelectorAll(".chit").forEach(hit => {
      hit.onmousemove = (e) => {
        tip.innerHTML = `<b>${F.esc(hit.dataset.k)}</b>
          <div class="row"><span><i class="sw" style="background:var(--dv-in)"></i>输入</span><span>${F.tok(+hit.dataset.in)}</span></div>
          <div class="row"><span><i class="sw" style="background:var(--dv-out)"></i>输出</span><span>${F.tok(+hit.dataset.out)}</span></div>
          <div class="row"><span>请求</span><span>${hit.dataset.req}${+hit.dataset.err ? `（${hit.dataset.err} 错误）` : ""}</span></div>`;
        tip.style.display = "block";
        tip.style.left = Math.min(e.clientX + 14, innerWidth - 180) + "px";
        tip.style.top = (e.clientY + 14) + "px";
      };
      hit.onmouseleave = () => { tip.style.display = "none"; };
    });
  }

  function makeTip() {
    const t = document.createElement("div");
    t.className = "chart-tip";
    t.id = "chartTip";
    document.body.appendChild(t);
    return t;
  }

  // 分组表：请求列内嵌占比条（以最大组为基准）
  function bucketTable(buckets, isAgent) {
    if (!buckets.length) return `<div class="empty-state">无数据</div>`;
    const maxReq = Math.max(...buckets.map(b => b.requests), 1);
    return `<div class="tbl-wrap"><table class="tbl"><thead><tr>
      <th>${isAgent ? "Agent" : "模型"}</th><th class="r">请求</th>
      <th class="r">输入</th><th class="r">输出</th><th class="r">成本</th>
    </tr></thead><tbody>
    ${buckets.map(b => `<tr>
      <td class="mono">${F.esc(isAgent ? F.agentName(b.key) : (b.key || "未知模型"))}</td>
      <td class="r"><span class="meter"><i class="dim" style="width:${(b.requests / maxReq * 100).toFixed(0)}%"></i></span> ${F.num(b.requests)}${b.errors ? ` <span class="badge err">${b.errors}错</span>` : ""}</td>
      <td class="r">${F.tok(b.input)}</td>
      <td class="r">${F.tok(b.output)}</td>
      <td class="r">${b.hasPrice ? F.money(b.costUsd) : "—"}</td>
    </tr>`).join("")}
    </tbody></table></div>`;
  }

  function toolRank(t, i, arr) {
    const max = Math.max(...arr.map(x => x.count), 1);
    return `<div class="rank-item">
      <span class="rk-name">${F.esc(t.name)}</span>
      <span class="meter lg"><i style="width:${(t.count / max * 100).toFixed(0)}%"></i></span>
      <span class="rk-val">×${F.num(t.count)}</span>
    </div>`;
  }

  return { render };
})();
