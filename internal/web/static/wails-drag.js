// Wails 桌面壳的窗口拖拽桥（仅 ?desktop=1 时由 app.js 动态加载）。
//
// Wails v3 的 --wails-draggable 拖拽逻辑内置于其 runtime.js，而 runtime.js
// 只有经 Wails 自身 asset server 提供的页面才会加载；本页由 ai-trace 自己的
// HTTP 服务提供（desktop/main.go 里 WebviewWindow 加载的是外部 URL），拿不到
// runtime，于是顶栏的 --wails-draggable:drag 是"死样式"，窗口无法拖动。
//
// 这里按 runtime 相同的消息契约自行实现（见 wails v3 源码
// bundledassets/runtime.debug.js 的 drag.ts 与 pkg/application/webview_window.go
// HandleMessage 的 "wails:drag" 分支）：命中拖拽区后向原生端发
// "wails:drag"，原生端收到后 performWindowDrag 接管移动。浏览器模式下
// 桥不存在，脚本整体为空操作，无副作用。

// 原生消息桥：macOS/Linux 走 webkit messageHandlers，Windows 走 WebView2
function invoke(msg) {
  try {
    if (window.webkit?.messageHandlers?.external) {
      window.webkit.messageHandlers.external.postMessage(msg);
    } else if (window.chrome?.webview) {
      window.chrome.webview.postMessage(msg);
    }
  } catch { /* 桥不可用时静默 */ }
}

// 与 Wails runtime 的 isDraggableEvent 一致：解析事件目标元素（非 HTMLElement
// 时上溯一层，覆盖顶栏 logo 的 SVG），其计算样式 --wails-draggable 为 "drag"
// 且坐标落在元素边界内（自定义属性会沿 DOM 继承，nav/actions 上的
// no-drag 覆盖即在这条路径上生效）。
function draggable(e) {
  let t = e.target;
  if (!(t instanceof HTMLElement)) {
    t = t instanceof Node ? t.parentElement : document.body;
    if (!(t instanceof HTMLElement)) return false;
  }
  const s = getComputedStyle(t);
  return s.getPropertyValue("--wails-draggable").trim() === "drag"
    && e.offsetX >= 0 && e.offsetX < t.clientWidth
    && e.offsetY >= 0 && e.offsetY < t.clientHeight;
}

let canDrag = false;   // mousedown 命中拖拽区（尚未开始拖动）
let dragging = false;  // 已向原生端发起拖动（只发一次，此后由原生接管）

window.addEventListener("mousedown", (e) => {
  // detail !== 1 跳过双击的第二击，避免与双击缩放竞争（与 runtime 行为一致）
  canDrag = e.button === 0 && e.detail === 1 && draggable(e);
}, true);

// 在 mousemove 而非 mousedown 时发起拖动：区分"点击"与"按住拖动"
window.addEventListener("mousemove", () => {
  if (canDrag && !dragging) {
    dragging = true;
    invoke("wails:drag");
  }
}, true);

window.addEventListener("mouseup", () => {
  canDrag = false;
  dragging = false;
}, true);

// 双击拖拽区 = 标题栏语义（默认最大化/还原）
window.addEventListener("dblclick", (e) => {
  if (window !== window.top || !draggable(e)) return;
  e.stopImmediatePropagation();
  e.preventDefault();
  invoke("wails:drag:doubleclick");
}, true);
