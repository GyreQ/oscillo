// 技能沉淀视图：从录制的请求里解析出的技能/专家包。
// #/skills 列表 · #/skills/<name> 技能包浏览（左：目录树 / 右：文件预览）
AI.views = AI.views || {};
AI.views.skills = (() => {
  const F = AI.fmt;

  async function render(container) {
    container.innerHTML = `<div class="view-inner">
      <div class="view-h">
        <h1>技能</h1><span class="sub">从请求流量里沉淀的技能 / 专家包 · 使用即归档</span>
      </div><div class="list"><div class="empty-state">加载中…</div></div></div>`;

    const resp = await AI.api.skills().catch(() => ({ skills: [] }));
    const list = resp.skills || [];
    if (!list.length) {
      container.querySelector(".list").innerHTML = `<div class="empty-state">
        <b>还没有沉淀到技能</b>在 Agent 会话里调用技能 / 专家包后，这里会自动归档其完整提示词</div>`;
      return;
    }
    container.querySelector(".list").innerHTML = list.map(s => `
      <div class="row click" data-name="${F.esc(s.name)}">
        <div class="who">
          <div class="name mono">${F.esc(s.name)}${s.source ? ' <span class="badge">本地包</span>' : ""}</div>
          <div class="sub">${s.agents && s.agents.length ? F.esc(s.agents.map(F.agentName).join(" · ")) : "—"}
            ${s.models && s.models.length ? " · " + F.esc(s.models.join(" / ")) : ""}
            ${s.versions > 1 ? ` · ${s.versions} 个版本` : ""}</div>
        </div>
        <div class="sess-num">
          <b>${s.usages} 次</b>
          <div class="sub">${F.bytes(s.size)} · ${F.ago(s.lastSeen)}</div>
        </div>
      </div>`).join("");
    container.querySelectorAll(".row[data-name]").forEach(item => {
      item.onclick = () => {
        location.hash = `#/skills/${encodeURIComponent(item.dataset.name)}`;
      };
    });
  }

  return { render };
})();

// ---- 单技能：目录树 + 预览 ----
AI.views.skillDetail = (() => {
  const F = AI.fmt;
  let selected = null;

  // 文件扩展名 → 预览类型徽标
  const EXT_KIND = {
    md: "MD", txt: "TXT", py: "PY", sh: "SH", js: "JS", ts: "TS", json: "JSON",
    html: "HTML", css: "CSS", yaml: "YML", yml: "YML", xml: "XML", go: "GO",
    png: "IMG", jpg: "IMG", jpeg: "IMG", gif: "IMG", svg: "SVG", webp: "IMG", ico: "IMG",
  };

  function extOf(path) {
    const m = /\.([a-z0-9]+)$/i.exec(path || "");
    return m ? m[1].toLowerCase() : "";
  }

  async function render(container, name) {
    selected = null;
    container.innerHTML = `<div class="view-inner"><div class="empty-state">加载中…</div></div>`;

    const [detail, tree] = await Promise.all([
      AI.api.skillDetail(name).catch(() => null),
      AI.api.get(`/api/skills/${encodeURIComponent(name)}/tree`).catch(() => null),
    ]);
    if (!detail && !tree) {
      container.innerHTML = `<div class="view-inner"><div class="empty-state"><b>技能加载失败</b>不存在或网络错误</div></div>`;
      return;
    }
    const entries = (tree && tree.entries) || [];
    const hasLocal = !!(tree && tree.hasLocal);

    container.innerHTML = `
      <div class="view-inner">
        <div class="view-h" style="align-items:center">
          <a class="backlink" href="#/skills">← 技能</a>
          <h1 class="mono" style="font-size:14.5px">${F.esc(name)}</h1>
          ${hasLocal ? '<span class="badge">本地技能包</span>' : '<span class="badge">仅流量沉淀</span>'}
          <span class="sub">${detail && detail.history && detail.history.length > 1 ? detail.history.length + " 个版本 · " : ""}${hasLocal && tree.source ? "源：" + F.esc(tree.source) : "无本地目录，正文来自请求流量"}</span>
          <a class="btn primary" style="margin-left:auto" href="/api/skills/${encodeURIComponent(name)}/export">⤓ 导出技能包</a>
        </div>
        <div class="skill-wrap">
          <div class="skill-tree" id="skillTree"></div>
          <div class="skill-view" id="skillView">
            <div class="empty-state" style="padding:40px 0">选择左侧文件预览</div>
          </div>
        </div>
      </div>`;

    const treeEl = container.querySelector("#skillTree");
    buildTree(treeEl, entries, 0, name);
    // 自动选中：SKILL.md → agents 下主文件 → README → 第一个文件
    const first = findFirst(entries, name);
    if (first) loadFile(container, name, first);
  }

  // 递归渲染目录树
  function buildTree(parent, entries, depth, skillName) {
    for (const e of entries) {
      if (e.dir) {
        const head = document.createElement("div");
        head.className = "tnode dir open";
        head.innerHTML = `<span class="tw"></span><span class="tname">${F.esc(e.name)}</span>
          <span class="tcount">${(e.children || []).length}</span>`;
        const kids = document.createElement("div");
        kids.className = "tkids";
        buildTree(kids, e.children || [], depth + 1, skillName);
        head.onclick = () => {
          head.classList.toggle("open");
          kids.classList.toggle("hidden");
        };
        parent.appendChild(head);
        parent.appendChild(kids);
      } else {
        const node = document.createElement("div");
        node.className = "tnode file";
        node.dataset.path = e.path;
        const ext = extOf(e.path);
        node.innerHTML = `<span class="tbadge ${ext ? "b-" + ext : ""}">${F.esc((EXT_KIND[ext] || ext || "?").toUpperCase())}</span>
          <span class="tname">${F.esc(e.name)}</span><span class="tsize">${F.bytes(e.size)}</span>`;
        node.onclick = () => {
          document.querySelectorAll(".tnode.file.sel").forEach(x => x.classList.remove("sel"));
          node.classList.add("sel");
          loadFile(node.closest(".view-inner"), skillName, e.path);
        };
        parent.appendChild(node);
      }
    }
  }

  // 默认打开哪个文件
  function findFirst(entries, skillName) {
    const flat = [];
    const walk = es => (es || []).forEach(e => e.dir ? walk(e.children) : flat.push(e.path));
    walk(entries);
    const score = p => {
      if (p === "SKILL.md") return 0;
      if (/^agents\/[^/]+\.md$/.test(p)) return 1;
      if (p === "README.md") return 2;
      if (/\.md$/.test(p)) return 3;
      return 4;
    };
    if (!flat.length) return null;
    return flat.sort((a, b) => score(a) - score(b) || a.localeCompare(b))[0];
  }

  // 加载并预览一个文件
  async function loadFile(container, name, path) {
    selected = path;
    const view = container.querySelector("#skillView");
    if (!view) return;
    view.innerHTML = `<div class="empty-state" style="padding:40px 0">加载中…</div>`;
    const escName = encodeURIComponent(name), escPath = encodeURIComponent(path);
    const d = await AI.api.get(`/api/skills/${escName}/file?p=${escPath}`).catch(() => null);
    if (selected !== path) return;
    if (!d) {
      view.innerHTML = `<div class="empty-state" style="padding:40px 0">文件读取失败（不存在或非文本）</div>`;
      return;
    }

    const head = `<div class="pv-head">
      <span class="mono pv-path">${F.esc(path)}</span>
      <span class="sub">${F.bytes(d.size)} · ${{ text: "文本", image: "图片", binary: "二进制" }[d.kind] || d.kind}</span>
    </div>`;
    if (d.kind === "image") {
      view.innerHTML = head + `<div class="pv-img"><img src="/api/skills/${escName}/file?p=${escPath}&raw=1" alt="${F.esc(path)}" loading="lazy"></div>`;
    } else if (d.kind === "binary") {
      view.innerHTML = head + `<div class="empty-state" style="padding:40px 0">二进制文件，请导出技能包后查看</div>`;
    } else if (d.kind === "text") {
      const ext = extOf(path);
      if (ext === "md") {
        view.innerHTML = head + `<div class="md">${mdToHtml(d.content || "")}</div>`;
      } else {
        view.innerHTML = head + `<pre class="pv-code">${F.esc(d.content || "")}</pre>`;
      }
    } else {
      view.innerHTML = head + `<div class="empty-state" style="padding:40px 0">（空文件）</div>`;
    }
  }

  // ---- 迷你 Markdown 渲染（转义后再做结构转换，安全无外链资源） ----
  function mdToHtml(md) {
    const esc = F.esc;
    const inline = t => esc(t)
      .replace(/`([^`]+)`/g, '<code>$1</code>')
      .replace(/\*\*([^*]+)\*\*/g, "<b>$1</b>")
      .replace(/(^|[^*])\*([^*\n]+)\*/g, "$1<i>$2</i>")
      .replace(/\[([^\]]+)\]\(([^)]+)\)/g, '<span class="md-link" title="$2">$1</span>');
    let fmHTML = "";
    // frontmatter（--- name: ... ---）渲染成元信息块
    const fm = /^---\r?\n([\s\S]*?)\r?\n---\r?\n?/.exec(md.replace(/^\r?\n+/, ""));
    if (fm) {
      md = md.replace(/^\r?\n+/, "").slice(fm[0].length);
      const rows = fm[1].split("\n").map(ln => {
        const m = /^([A-Za-z_][\w-]*)\s*:\s*(.*)$/.exec(ln.trim());
        if (!m) return "";
        return `<div class="fm-row"><span class="fm-key">${esc(m[1])}</span><span class="fm-val">${inline(m[2])}</span></div>`;
      }).filter(Boolean).join("");
      fmHTML = `<div class="fm">${rows}</div>`;
    }
    const lines = md.replace(/\r\n/g, "\n").split("\n");
    let html = "", i = 0, listStack = [];
    const closeLists = () => { while (listStack.length) html += listStack.pop() === "ul" ? "</ul>" : "</ol>"; };

    while (i < lines.length) {
      const ln = lines[i];
      // 围栏代码块
      if (/^```/.test(ln.trim())) {
        closeLists();
        const buf = [];
        i++;
        while (i < lines.length && !/^```/.test(lines[i].trim())) buf.push(lines[i++]);
        i++;
        html += `<pre class="md-pre">${esc(buf.join("\n"))}</pre>`;
        continue;
      }
      const h = /^(#{1,4})\s+(.*)$/.exec(ln);
      if (h) { closeLists(); html += `<h${h[1].length}>${inline(h[2])}</h${h[1].length}>`; i++; continue; }
      if (/^\s*(---|\*\*\*)\s*$/.test(ln)) { closeLists(); html += "<hr>"; i++; continue; }
      const bq = /^>\s?(.*)$/.exec(ln);
      if (bq) {
        closeLists();
        const buf = [bq[1]];
        while (i + 1 < lines.length && /^>\s?/.test(lines[i + 1])) buf.push(lines[++i].replace(/^>\s?/, ""));
        html += `<blockquote>${inline(buf.join(" "))}</blockquote>`; i++; continue;
      }
      const ul = /^\s*[-*+]\s+(.+)$/.exec(ln);
      const ol = /^\s*\d+[.)]\s+(.+)$/.exec(ln);
      if (ul || ol) {
        const want = ul ? "ul" : "ol";
        if (listStack[listStack.length - 1] !== want) { closeLists(); html += want === "ul" ? "<ul>" : "<ol>"; listStack.push(want); }
        html += `<li>${inline((ul || ol)[1])}</li>`;
        i++; continue;
      }
      if (ln.trim() === "") { closeLists(); i++; continue; }
      // 普通段落：合并到空行
      const buf = [ln];
      while (i + 1 < lines.length && lines[i + 1].trim() !== "" &&
        !/^(#{1,4}\s|```|>|\s*[-*+]\s|\s*\d+[.)]\s)/.test(lines[i + 1])) buf.push(lines[++i]);
      closeLists();
      html += `<p>${inline(buf.join(" "))}</p>`;
      i++;
    }
    closeLists();
    return fmHTML + html;
  }

  return { render };
})();
