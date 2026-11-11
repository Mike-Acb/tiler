"use strict";

// ============ 基础工具 ============
const $ = (sel, root = document) => root.querySelector(sel);
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const PALETTE = ["#4fb3a6", "#e0a84f", "#8d8ff0", "#e2685f", "#6cc27a", "#d27ac8", "#5aa2e8", "#c9b458"];
const PHASE_TEXT = { Running: "执行中", Succeeded: "成功", Failed: "失败", Pending: "等待中" };
const STATE_TEXT = { running: "运行", done: "完成", failed: "失败", waiting: "等待" };
const KIND_TEXT = { download: "下载", pack: "打包", all: "下载+打包" };

const store = {
  get(k) { try { return sessionStorage.getItem(k); } catch { return null; } },
  set(k, v) { try { v == null ? sessionStorage.removeItem(k) : sessionStorage.setItem(k, v); } catch { /* 隐私模式下忽略 */ } },
  // 预览来源这类偏好跨会话保留
  getLocal(k) { try { return localStorage.getItem(k); } catch { return null; } },
  setLocal(k, v) { try { localStorage.setItem(k, v); } catch { /* 忽略 */ } },
};

const state = {
  info: { writable: false, dryRun: false, namespace: "" },
  token: store.get("tc-token"),
  layers: [],
  selected: null,       // layer id
  detail: null,         // /api/layers/{id}
  visible: new Set(),   // 地图上显示的图层 id
  src: store.getLocal("tc-src") || "output", // 预览来源：output / pmtiles / tileserver
  loaded: new Map(),    // id -> 已加载图层的「来源:数据版本」，变了就重建
  lrsHidden: new Set(),
  tab: "regions",
  draft: null,          // 待派发区域 FeatureCollection
  copy: null,           // 复制已配置区域：{ layer, title, key, cm, min, max }
  targets: new Set(),   // 「派发到图层」勾选的图层 id
  multiResult: null,    // 多图层派发结果 [{ t, ok, job?, msg? }]
  draw: { mode: null, points: [], start: null },
  jobs: [],
  showLegacy: false,
  activeJob: null,
  log: { job: null, pod: null, container: null, ctrl: null, auto: true },
};

function toast(msg, isError = false) {
  const el = $("#toast");
  el.textContent = msg;
  el.className = "toast" + (isError ? " error" : "");
  el.hidden = false;
  clearTimeout(toast.t);
  toast.t = setTimeout(() => (el.hidden = true), isError ? 6000 : 3000);
}

async function api(path, opts = {}) {
  const headers = { ...(opts.headers || {}) };
  if (opts.body && typeof opts.body !== "string") {
    headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(opts.body);
  }
  if (opts.auth) {
    if (!state.token && !(await unlock())) throw new Error("需要口令");
    headers.Authorization = "Bearer " + state.token;
  }
  const res = await fetch(path, { ...opts, headers });
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = text; }
  if (res.status === 401 && opts.auth) {
    setToken(null);
    throw new Error("口令错误或已失效，请重新解锁");
  }
  if (!res.ok) throw new Error((data && data.error) || text || res.statusText);
  return data;
}

const fmtSize = (n) => {
  if (n == null) return "";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return n.toFixed(n >= 100 || i === 0 ? 0 : 1) + " " + u[i];
};
const fmtTime = (t) => {
  if (!t) return "";
  const d = new Date(t);
  const pad = (x) => String(x).padStart(2, "0");
  return `${d.getMonth() + 1}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
};
const fmtAgo = (t) => {
  if (!t) return "";
  const s = (Date.now() - new Date(t)) / 1000;
  if (s < 60) return "刚刚";
  if (s < 3600) return Math.floor(s / 60) + " 分钟前";
  if (s < 86400) return Math.floor(s / 3600) + " 小时前";
  return Math.floor(s / 86400) + " 天前";
};
const fmtDur = (a, b) => {
  if (!a) return "";
  const s = Math.max(0, ((b ? new Date(b) : new Date()) - new Date(a)) / 1000);
  if (s < 60) return Math.round(s) + " 秒";
  if (s < 3600) return Math.round(s / 60) + " 分钟";
  if (s < 172800) return (s / 3600).toFixed(1) + " 小时";
  return Math.round(s / 86400) + " 天";
};

// ============ 口令 ============
function setToken(t) {
  state.token = t;
  store.set("tc-token", t);
  renderLock();
}

function renderLock() {
  const btn = $("#lock-btn");
  if (!state.info.writable) {
    btn.textContent = "只读（服务端未配置口令）";
    btn.disabled = true;
    return;
  }
  btn.disabled = false;
  btn.textContent = state.token ? "已解锁 · 退出" : "解锁写操作";
}

function unlock() {
  return new Promise((resolve) => {
    const dlg = $("#auth-dlg");
    const input = $("#auth-input");
    const err = $("#auth-err");
    input.value = "";
    err.textContent = "";
    dlg.showModal();
    const onSubmit = async (e) => {
      if (e.submitter && e.submitter.value === "cancel") { cleanup(); resolve(false); return; }
      e.preventDefault();
      try {
        const res = await fetch("/api/auth", { method: "POST", headers: { Authorization: "Bearer " + input.value } });
        if (!res.ok) {
          const d = await res.json().catch(() => ({}));
          err.textContent = d.error || "口令错误";
          return;
        }
        setToken(input.value);
        dlg.close();
        cleanup();
        resolve(true);
      } catch (ex) { err.textContent = ex.message; }
    };
    const onClose = () => { cleanup(); resolve(!!state.token); };
    const cleanup = () => { dlg.removeEventListener("submit", onSubmit); dlg.removeEventListener("close", onClose); };
    dlg.addEventListener("submit", onSubmit);
    dlg.addEventListener("close", onClose, { once: true });
  });
}

$("#lock-btn").addEventListener("click", () => {
  if (state.token) { setToken(null); toast("已退出写操作"); } else unlock();
});

function confirmDialog(title, bodyHTML, okText = "确认") {
  return new Promise((resolve) => {
    const dlg = $("#confirm-dlg");
    $("#confirm-title").textContent = title;
    $("#confirm-body").innerHTML = bodyHTML;
    $("#confirm-ok").textContent = okText;
    dlg.returnValue = "";
    dlg.addEventListener("close", () => resolve(dlg.returnValue === "ok"), { once: true });
    dlg.showModal();
  });
}

// ============ 地图 ============
const protocol = new pmtiles.Protocol();
maplibregl.addProtocol("pmtiles", protocol.tile);

const map = new maplibregl.Map({
  container: "map",
  style: {
    version: 8,
    sources: {},
    layers: [{ id: "bg", type: "background", paint: { "background-color": "#0b0d10" } }],
  },
  center: [110, 32],
  zoom: 3.4,
  attributionControl: { compact: true },
  dragRotate: false,
});
map.touchZoomRotate.disableRotation();
map.addControl(new maplibregl.NavigationControl({ showCompass: false }), "top-right");
map.addControl(new maplibregl.ScaleControl({ unit: "metric" }), "bottom-right");

const mapReady = new Promise((r) => map.on("load", r));

function updateHud(e) {
  const c = e && e.lngLat ? e.lngLat : map.getCenter();
  $("#map-hud").textContent = `z ${map.getZoom().toFixed(1)}  ·  ${c.lng.toFixed(4)}, ${c.lat.toFixed(4)}`;
}
map.on("move", () => updateHud());
map.on("mousemove", updateHud);

const tileSourceId = (id) => "tiles-" + id;
const absUrl = (u) => new URL(u, location.href).href;
// MapLibre 的瓦片模板要保留 {z}/{x}/{y}，new URL 会把花括号转义
const tileTpl = (path) => absUrl(path).replace(/%7B/g, "{").replace(/%7D/g, "}");

// ---- 预览来源 ----
// A 下载目录：控制台读 PVC 里的 z/x/y 文件，能看到还没打包的下载结果
// B pmtiles.js：浏览器直读 pmtiles（配置了公开地址就直连 MinIO，否则经控制台中转）
// C 瓦片服务：经控制台反向代理读 go-pmtiles serve 的 z/x/y
const SOURCES = [
  { id: "output", key: "A", name: "下载目录" },
  { id: "pmtiles", key: "B", name: "pmtiles.js 直读" },
  { id: "tileserver", key: "C", name: "瓦片服务" },
];

function sourceReady(src) {
  const sto = state.info.storage || {};
  if (src === "output") return true;
  if (src === "pmtiles") return !!sto.minioReady;
  if (src === "tileserver") return !!(sto.tileServer && sto.tileServer.value);
  return false;
}

// 某图层在某来源下有没有数据；返回不可用原因，可用时返回空串
function unavailableReason(layer, src = state.src) {
  if (!sourceReady(src)) return "未配置该预览来源";
  if (layer.format === "pbf" || layer.confFormat === "pbf") return "矢量瓦片（pbf）暂不支持预览";
  if (src === "output") return layer.output ? "" : "下载目录里还没有该图层";
  if (src === "pmtiles") return layer.pmtiles ? "" : "MinIO 上还没有 pmtiles（尚未打包）";
  if (src === "tileserver") {
    if (!layer.tileServerPath) return "minioPath 不在瓦片服务前缀下";
    if (state.info.storage.minioReady && !layer.pmtiles) return "MinIO 上还没有 pmtiles（尚未打包）";
    return "";
  }
  return "未知来源";
}

// 数据版本：变了就重新加载图层，并用 v= 绕开浏览器缓存
function layerVersion(layer, src = state.src) {
  if (src === "output") {
    const j = layer.lastJob;
    return j && j.completed ? new Date(j.completed).getTime() : 0;
  }
  return layer.pmtiles ? new Date(layer.pmtiles.modified).getTime() : 0;
}

function sourceSpec(layer) {
  const v = layerVersion(layer);
  const id = encodeURIComponent(layer.id);
  if (state.src === "output") {
    return {
      type: "raster", tileSize: 256, minzoom: 0, maxzoom: layer.confMax,
      tiles: [tileTpl(`/tiles/output/${id}/{z}/{x}/{y}?v=${v}`)],
      attribution: layer.title + " · 下载目录",
    };
  }
  if (state.src === "pmtiles") {
    return { type: "raster", tileSize: 256, url: "pmtiles://" + absUrl(layer.pmtilesUrl), attribution: layer.title + " · pmtiles" };
  }
  return {
    type: "raster", tileSize: 256, minzoom: 0, maxzoom: layer.max,
    tiles: [tileTpl(`/tile-server/${layer.tileServerPath}/{z}/{x}/{y}.${layer.confFormat}?v=${v}`)],
    attribution: layer.title + " · 瓦片服务",
  };
}

async function ensureTileLayer(layer) {
  await mapReady;
  const sid = tileSourceId(layer.id);
  if (map.getSource(sid)) return;
  const why = unavailableReason(layer);
  if (why) throw new Error(why);
  map.addSource(sid, sourceSpec(layer));
  map.addLayer({ id: sid, type: "raster", source: sid, paint: { "raster-fade-duration": 120 } }, firstOverlayId());
  state.loaded.set(layer.id, state.src + ":" + layerVersion(layer));
  reorderTileLayers();
}

function removeTileLayer(id) {
  const sid = tileSourceId(id);
  if (map.getLayer(sid)) map.removeLayer(sid);
  if (map.getSource(sid)) map.removeSource(sid);
  state.loaded.delete(id);
}

// 栅格层按清单顺序叠放：清单里靠后的（通常是注记）在上
function reorderTileLayers() {
  const before = firstOverlayId();
  for (const l of state.layers) {
    const sid = tileSourceId(l.id);
    if (map.getLayer(sid)) map.moveLayer(sid, before);
  }
}
function firstOverlayId() {
  const ids = map.getStyle().layers.map((l) => l.id);
  return ids.find((id) => id.startsWith("lrs-") || id.startsWith("draft-"));
}

// 切换来源或数据更新后，按当前来源重建已打开的图层
function refreshTileLayers(force = false) {
  for (const id of state.visible) {
    const layer = state.layers.find((l) => l.id === id);
    if (!layer) continue;
    const want = state.src + ":" + layerVersion(layer);
    if (!force && state.loaded.get(id) === want) continue;
    removeTileLayer(id);
    if (!unavailableReason(layer)) ensureTileLayer(layer).catch((e) => toast(`${layer.title}：${e.message}`, true));
  }
}

function setSource(src) {
  if (src === state.src || !sourceReady(src)) return;
  state.src = src;
  store.setLocal("tc-src", src);
  refreshTileLayers(true);
  const missing = [...state.visible].map((id) => state.layers.find((l) => l.id === id)).filter((l) => l && unavailableReason(l));
  if (missing.length) toast(`${missing.map((l) => l.title).join("、")} 在「${SOURCES.find((x) => x.id === src).name}」下没有数据`);
  renderSourceSwitch();
  renderLayers();
}

async function toggleVisible(id, on) {
  const layer = state.layers.find((l) => l.id === id);
  if (!layer) return;
  if (on) {
    state.visible.add(id);
    try { await ensureTileLayer(layer); } catch (e) { state.visible.delete(id); toast(`${layer.title}：${e.message}`, true); }
  } else {
    state.visible.delete(id);
    removeTileLayer(id);
  }
  renderLayers();
}

// ---- 已配置区域（lrs） ----
function clearLrsLayers() {
  for (const l of map.getStyle().layers.slice()) if (l.id.startsWith("lrs-")) map.removeLayer(l.id);
  for (const id of Object.keys(map.getStyle().sources)) if (id.startsWith("lrs-")) map.removeSource(id);
}

async function drawLrs() {
  await mapReady;
  clearLrsLayers();
  if (!state.detail) return;
  const before = map.getLayer("draft-fill") ? "draft-fill" : undefined;
  state.detail.lrs.forEach((x, i) => {
    if (!x.data || state.lrsHidden.has(i)) return;
    const sid = "lrs-" + i;
    const color = PALETTE[i % PALETTE.length];
    map.addSource(sid, { type: "geojson", data: x.data });
    map.addLayer({ id: sid + "-fill", type: "fill", source: sid, paint: { "fill-color": color, "fill-opacity": 0.08 } }, before);
    map.addLayer({ id: sid + "-line", type: "line", source: sid, paint: { "line-color": color, "line-width": 1.5, "line-opacity": 0.9 } }, before);
  });
}

// ---- 草稿区域与绘制 ----
function setupDraftLayers() {
  map.addSource("draft", { type: "geojson", data: emptyFC() });
  map.addSource("draft-guide", { type: "geojson", data: emptyFC() });
  map.addLayer({ id: "draft-fill", type: "fill", source: "draft", paint: { "fill-color": "#ffffff", "fill-opacity": 0.14 } });
  map.addLayer({ id: "draft-line", type: "line", source: "draft", paint: { "line-color": "#ffffff", "line-width": 2 } });
  map.addLayer({ id: "draft-guide-line", type: "line", source: "draft-guide", paint: { "line-color": "#ffffff", "line-width": 1.5, "line-dasharray": [2, 2] } });
  map.addLayer({ id: "draft-guide-pt", type: "circle", source: "draft-guide", filter: ["==", "$type", "Point"], paint: { "circle-radius": 4, "circle-color": "#0b0d10", "circle-stroke-color": "#fff", "circle-stroke-width": 2 } });
}
mapReady.then(setupDraftLayers);

const emptyFC = () => ({ type: "FeatureCollection", features: [] });

function setDraft(fc) {
  state.draft = fc;
  if (map.getSource("draft")) map.getSource("draft").setData(fc || emptyFC());
  renderEstimate();
  renderDispatchState();
}

function setGuide(features) {
  if (map.getSource("draft-guide")) map.getSource("draft-guide").setData({ type: "FeatureCollection", features });
}

function startDraw(mode) {
  stopDraw();
  state.draw = { mode, points: [], start: null };
  map.getCanvas().classList.add("drawing");
  map.doubleClickZoom.disable();
  const hint = $("#draw-hint");
  hint.textContent = mode === "rect" ? "按住鼠标拖出矩形 · Esc 取消" : "单击加点 · 双击或回车完成 · 点首个点闭合 · Esc 取消";
  hint.hidden = false;
  renderTools();
}

function stopDraw() {
  state.draw = { mode: null, points: [], start: null };
  map.getCanvas().classList.remove("drawing");
  map.dragPan.enable();
  setTimeout(() => map.doubleClickZoom.enable(), 0);
  $("#draw-hint").hidden = true;
  setGuide([]);
  renderTools();
}

const rectFC = (a, b) => {
  const [x1, x2] = [Math.min(a.lng, b.lng), Math.max(a.lng, b.lng)];
  const [y1, y2] = [Math.min(a.lat, b.lat), Math.max(a.lat, b.lat)];
  return { type: "FeatureCollection", features: [{ type: "Feature", properties: {}, geometry: { type: "Polygon", coordinates: [[[x1, y1], [x2, y1], [x2, y2], [x1, y2], [x1, y1]]] } }] };
};
const clampLL = (ll) => ({ lng: Math.max(-180, Math.min(180, ll.lng)), lat: Math.max(-85, Math.min(85, ll.lat)) });
const round6 = (n) => Math.round(n * 1e6) / 1e6;

map.on("mousedown", (e) => {
  if (state.draw.mode !== "rect") return;
  e.preventDefault();
  map.dragPan.disable();
  state.draw.start = clampLL(e.lngLat);
});
map.on("mousemove", (e) => {
  const d = state.draw;
  if (d.mode === "rect" && d.start) {
    setGuide(rectFC(d.start, clampLL(e.lngLat)).features);
  } else if (d.mode === "poly" && d.points.length) {
    const pts = [...d.points, [e.lngLat.lng, e.lngLat.lat]];
    setGuide([
      { type: "Feature", geometry: { type: "LineString", coordinates: pts } },
      ...d.points.map((p) => ({ type: "Feature", geometry: { type: "Point", coordinates: p } })),
    ]);
  }
});
map.on("mouseup", (e) => {
  const d = state.draw;
  if (d.mode !== "rect" || !d.start) return;
  const end = clampLL(e.lngLat);
  const a = map.project(d.start), b = map.project(end);
  if (Math.abs(a.x - b.x) < 4 || Math.abs(a.y - b.y) < 4) { d.start = null; setGuide([]); map.dragPan.enable(); return; }
  const fc = rectFC(d.start, end);
  fc.features[0].geometry.coordinates[0] = fc.features[0].geometry.coordinates[0].map(([x, y]) => [round6(x), round6(y)]);
  stopDraw();
  setDraft(fc);
});
map.on("click", (e) => {
  const d = state.draw;
  if (d.mode !== "poly") return;
  const ll = clampLL(e.lngLat);
  if (d.points.length >= 3) {
    const first = map.project({ lng: d.points[0][0], lat: d.points[0][1] });
    if (Math.hypot(first.x - e.point.x, first.y - e.point.y) < 10) { finishPoly(); return; }
  }
  d.points.push([round6(ll.lng), round6(ll.lat)]);
});
map.on("dblclick", (e) => {
  if (state.draw.mode !== "poly") return;
  e.preventDefault();
  // 双击会先触发两次 click，多加的最后一个点去掉
  state.draw.points.pop();
  finishPoly();
});
document.addEventListener("keydown", (e) => {
  if (!state.draw.mode) return;
  if (e.key === "Escape") stopDraw();
  if (e.key === "Enter" && state.draw.mode === "poly") finishPoly();
});
function finishPoly() {
  const pts = state.draw.points;
  if (pts.length < 3) { toast("多边形至少需要 3 个点", true); return; }
  const ring = [...pts, pts[0]];
  stopDraw();
  setDraft({ type: "FeatureCollection", features: [{ type: "Feature", properties: {}, geometry: { type: "Polygon", coordinates: [ring] } }] });
}

// 导入的 GeoJSON 归一成只含 Polygon / MultiPolygon 的 FeatureCollection
function normalizeImport(obj) {
  let feats = [];
  if (obj.type === "FeatureCollection") feats = obj.features || [];
  else if (obj.type === "Feature") feats = [obj];
  else if (obj.type) feats = [{ type: "Feature", properties: {}, geometry: obj }];
  const out = [];
  for (const f of feats) {
    const g = f && f.geometry;
    if (!g) continue;
    if (g.type === "Polygon" || g.type === "MultiPolygon") out.push({ type: "Feature", properties: f.properties || {}, geometry: g });
    else if (g.type === "GeometryCollection") for (const sub of g.geometries || []) if (sub.type === "Polygon" || sub.type === "MultiPolygon") out.push({ type: "Feature", properties: {}, geometry: sub });
  }
  if (!out.length) throw new Error("文件里没有 Polygon / MultiPolygon");
  return { type: "FeatureCollection", features: out };
}

function bboxOf(fc) {
  let x1 = Infinity, y1 = Infinity, x2 = -Infinity, y2 = -Infinity;
  const walk = (c) => {
    if (typeof c[0] === "number") { x1 = Math.min(x1, c[0]); x2 = Math.max(x2, c[0]); y1 = Math.min(y1, c[1]); y2 = Math.max(y2, c[1]); }
    else c.forEach(walk);
  };
  for (const f of fc.features || []) if (f.geometry) walk(f.geometry.coordinates);
  return isFinite(x1) ? [x1, y1, x2, y2] : null;
}

function fitTo(fc) {
  const b = bboxOf(fc);
  if (b) map.fitBounds([[b[0], b[1]], [b[2], b[3]]], { padding: 60, maxZoom: 14, duration: 600 });
}

// 按外接矩形估算瓦片数（上限，实际按多边形覆盖会更少）
function estimateTiles(fc, zmin, zmax) {
  const b = bboxOf(fc);
  if (!b) return 0;
  const lat = (v) => Math.max(-85.0511, Math.min(85.0511, v));
  let total = 0;
  for (let z = zmin; z <= zmax; z++) {
    const n = 2 ** z;
    const tx = (lng) => Math.min(n - 1, Math.floor(((lng + 180) / 360) * n));
    const ty = (la) => {
      const r = (lat(la) * Math.PI) / 180;
      return Math.min(n - 1, Math.floor(((1 - Math.log(Math.tan(r) + 1 / Math.cos(r)) / Math.PI) / 2) * n));
    };
    total += (tx(b[2]) - tx(b[0]) + 1) * (ty(b[1]) - ty(b[3]) + 1);
  }
  return total;
}

// ============ 数据加载 ============
async function loadInfo() {
  try {
    state.info = await api("/api/info");
  } catch (e) { toast("连接服务失败：" + e.message, true); }
  $("#ns-chip").textContent = "命名空间 " + (state.info.namespace || "?");
  $("#dry-chip").hidden = !state.info.dryRun;
  const sto = state.info.storage || {};
  $("#output-chip").textContent = "下载目录 " + ((sto.outputDir && sto.outputDir.value) || "?");
  const mc = $("#minio-chip");
  mc.classList.toggle("off", !sto.upload);
  mc.textContent = sto.upload ? `上传 MinIO · ${sto.minioBucket.value}` : sto.minioReady ? "MinIO 只读（不能打包）" : "未配置 MinIO · 只下载";
  mc.title = sto.uploadReason || "打包后的 pmtiles 上传到 MinIO";
  if (!sourceReady(state.src)) state.src = "output";
  if (!state.info.writable) setToken(null);
  renderLock();
  renderSourceSwitch();
}

// 地图左上角的预览来源切换，下面一行说明实际读的是哪里
function renderSourceSwitch() {
  const sto = state.info.storage || {};
  const seg = $("#src-seg");
  const sub = {
    output: "读 PVC 里的 z/x/y 文件",
    pmtiles: !sto.minioReady ? "未配置 MinIO" : sto.pmtilesPublicUrl && sto.pmtilesPublicUrl.value ? "浏览器直连 MinIO" : "经控制台中转 MinIO",
    tileserver: sto.tileServer && sto.tileServer.value ? "go-pmtiles serve · 反向代理" : "未配置 TILE_SERVER_URL",
  };
  seg.innerHTML = SOURCES.map((x) => `
    <button type="button" role="radio" aria-checked="${x.id === state.src}" class="${x.id === state.src ? "on" : ""}" data-src="${x.id}" ${sourceReady(x.id) ? "" : "disabled"}>
      <b>${x.key} ${x.name}</b><small>${sub[x.id]}</small>
    </button>`).join("");
  seg.querySelectorAll("button").forEach((b) => b.addEventListener("click", () => setSource(b.dataset.src)));

  const l = currentLayer() || state.layers.find((x) => state.visible.has(x.id));
  const name = l ? l.tileName : "<目录>";
  const lid = l ? l.id : "<图层>";
  const urlEl = $("#src-url");
  if (state.src === "output") {
    urlEl.innerHTML = `<span class="k">浏览器请求</span> /tiles/output/${esc(lid)}/{z}/{x}/{y}<br><span class="arrow">→</span> <span class="k">控制台读</span> ${esc(sto.outputDir ? sto.outputDir.value : "")}/${esc(name)}/{z}/{x}/{y}.${esc(l ? l.confFormat : "fmt")}`;
  } else if (state.src === "pmtiles") {
    urlEl.innerHTML = l && l.pmtilesUrl && /^https?:/.test(l.pmtilesUrl)
      ? `<span class="k">浏览器 pmtiles.js 直读</span> ${esc(l.pmtilesUrl)}`
      : `<span class="k">浏览器 pmtiles.js 读</span> /pmtiles/${esc(lid)}.pmtiles<br><span class="arrow">→</span> <span class="k">控制台转发 Range 请求到 MinIO</span> ${esc(sto.minioBucket ? sto.minioBucket.value : "")}/${esc(l ? l.pmtilesKey : "")}`;
  } else {
    const path = l && l.tileServerPath ? l.tileServerPath : "<瓦片集>";
    urlEl.innerHTML = `<span class="k">浏览器请求</span> /tile-server/${esc(path)}/{z}/{x}/{y}.${esc(l ? l.confFormat : "fmt")}<br><span class="arrow">→</span> <span class="k">反向代理到</span> ${esc(sto.tileServer ? sto.tileServer.value : "")}/${esc(path)}/…`;
  }
}

async function loadLayers() {
  try {
    state.layers = await api("/api/layers");
  } catch (e) {
    $("#layer-list").innerHTML = `<div class="empty">${esc(e.message)}</div>`;
    return;
  }
  // 下载完成 / 打包上传后数据版本变了：已显示的图层按当前来源重建
  refreshTileLayers();
  renderLayers();
  if (state.selected) { renderPanelHead(); renderTargets(); renderDispatchState(); }
}

async function selectLayer(id) {
  if (state.selected !== id) {
    state.selected = id;
    state.detail = null;
    state.lrsHidden.clear();
    state.activeJob = null;
    state.copy = null;
    state.targets = new Set([id]);
    state.multiResult = null;
    setDraft(null);
  }
  renderLayers();
  renderPanel();
  const layer = state.layers.find((l) => l.id === id);
  if (layer && !unavailableReason(layer) && !state.visible.has(id)) toggleVisible(id, true);
  try {
    const detail = await api("/api/layers/" + encodeURIComponent(id));
    if (state.selected !== id) return;
    state.detail = detail;
    renderPanel();
    drawLrs();
  } catch (e) { toast(e.message, true); }
  loadJobs();
}

async function loadJobs() {
  if (!state.selected) return;
  const id = state.selected;
  try {
    const jobs = (await api("/api/jobs" + (state.showLegacy ? "" : "?layer=" + encodeURIComponent(id)))) || [];
    if (state.selected !== id) return;
    state.jobs = state.showLegacy ? jobs.filter((j) => j.layer === id || !j.managed) : jobs;
    if (state.tab === "jobs") renderJobs();
  } catch (e) { /* 轮询失败不打扰 */ }
}

// ============ 渲染：图层列表 ============
function renderLayers() {
  const list = $("#layer-list");
  if (!state.layers.length) { list.innerHTML = '<div class="empty">图层清单为空</div>'; return; }
  const groups = new Map();
  for (const l of state.layers) {
    const g = l.group || "未分组";
    if (!groups.has(g)) groups.set(g, []);
    groups.get(g).push(l);
  }
  list.innerHTML = "";
  for (const [g, items] of groups) {
    const wrap = document.createElement("div");
    wrap.className = "layer-group";
    wrap.innerHTML = `<div class="layer-group-title">${esc(g)}</div>`;
    for (const l of items) {
      const el = document.createElement("div");
      el.className = "layer-item" + (l.id === state.selected ? " active" : "");
      const job = l.lastJob;
      const running = job && (job.phase === "Running" || job.phase === "Pending");
      const side = running || (job && !l.pendingPack)
        ? `<span class="dot ${job.phase}"></span>${PHASE_TEXT[job.phase] || job.phase}`
        : l.pendingPack ? '<span class="tag warn">待打包</span>' : "";
      const sub = l.tileName ? `${l.name || l.tileName} · ${l.format} · z${l.min}-${l.max}` : l.configMap;
      const warnN = (l.missing || []).length + (l.mismatch || []).length;
      const why = unavailableReason(l);
      // 各来源是否有数据：只列出已配置的来源
      const avail = SOURCES.filter((x) => sourceReady(x.id)).map((x) =>
        `<span class="${unavailableReason(l, x.id) ? "" : "on"}" title="${x.key} ${x.name}：${esc(unavailableReason(l, x.id) || "有数据")}">${x.key}</span>`).join("");
      el.innerHTML = `
        <button class="eye ${state.visible.has(l.id) ? "on" : ""}" type="button" title="${esc(why || "在地图上显示")}" ${why ? "disabled" : ""}>
          <svg width="12" height="12" viewBox="0 0 24 24" aria-hidden="true"><path fill="currentColor" d="M12 5C6 5 2 12 2 12s4 7 10 7 10-7 10-7-4-7-10-7Zm0 11a4 4 0 1 1 0-8 4 4 0 0 1 0 8Z"/></svg>
        </button>
        <div style="min-width:0">
          <div class="layer-name">${esc(l.title || l.id)}</div>
          <div class="layer-sub">${warnN ? `<span class="meta-warn" title="${esc([...(l.missing || []).map((m) => "pmtiles 缺少：" + m), ...(l.mismatch || [])].join("\n"))}">!</span>` : ""}${esc(sub)}</div>
        </div>
        <div class="layer-side">${side}<div>${l.pmtiles ? fmtSize(l.pmtiles.size) : state.info.storage && state.info.storage.minioReady ? "未打包" : ""}</div><div class="avail">${avail}</div></div>
        ${l.error ? `<div class="layer-err">${esc(l.error)}</div>` : ""}`;
      el.addEventListener("click", () => selectLayer(l.id));
      $(".eye", el).addEventListener("click", (e) => { e.stopPropagation(); toggleVisible(l.id, !state.visible.has(l.id)); });
      wrap.appendChild(el);
    }
    list.appendChild(wrap);
  }
}

// ============ 渲染：右侧面板 ============
const fromTag = (from) => (from === "configmap" ? '<span class="from cm" title="pmtiles 里没有，按 ConfigMap 显示">配置</span>' : from === "pmtiles" ? '<span class="from pm" title="读自 pmtiles 文件头 / metadata">pmtiles</span>' : "");
function currentLayer() { return state.layers.find((l) => l.id === state.selected); }

function renderPanel() {
  const panel = $("#panel");
  const l = currentLayer();
  if (!l) { panel.innerHTML = '<div class="empty pad">在左侧选择一个图层</div>'; return; }
  panel.innerHTML = `
    <div class="panel-head" id="panel-head"></div>
    <div id="panel-banners"></div>
    <nav class="tabs" role="tablist">
      <button class="tab ${state.tab === "regions" ? "active" : ""}" data-tab="regions" type="button">区域与派发</button>
      <button class="tab ${state.tab === "jobs" ? "active" : ""}" data-tab="jobs" type="button">任务</button>
      <button class="tab ${state.tab === "storage" ? "active" : ""}" data-tab="storage" type="button">存储</button>
      <button class="tab ${state.tab === "conf" ? "active" : ""}" data-tab="conf" type="button">配置</button>
    </nav>
    <div id="tab-body"></div>`;
  panel.querySelectorAll(".tab").forEach((b) => b.addEventListener("click", () => {
    state.tab = b.dataset.tab;
    renderPanel();
    if (state.tab === "jobs") loadJobs();
  }));
  renderPanelHead();
  if (state.tab === "regions") renderRegions();
  else if (state.tab === "jobs") renderJobs();
  else if (state.tab === "storage") renderStorage();
  else renderConf();
  renderSourceSwitch();
}

const FROM_TEXT = { env: "环境变量", json: "layers.json", default: "默认值", none: "未配置" };
function cfgRow(name, setting, display) {
  const st = setting || { from: "none" };
  const v = display !== undefined ? display : st.value;
  return `<tr><td>${esc(name)}</td><td>${v ? `<code>${esc(v)}</code>` : '<span class="muted">—</span>'}<span class="src-tag ${st.from}">${FROM_TEXT[st.from] || st.from}</span></td></tr>`;
}

function renderStorage() {
  const sto = state.info.storage || {};
  const l = currentLayer();
  $("#tab-body").innerHTML = `
    <div class="section"><h3>下载存储（主）</h3>
      <table class="cfg">
        ${cfgRow("OUTPUT_DIR", sto.outputDir)}
        <tr><td>PVC</td><td><code>${esc(sto.outputPVC || "")}</code><span class="src-tag json">layers.json</span></td></tr>
        ${l ? `<tr><td>本图层目录</td><td><code>${esc(l.tileName)}</code> ${l.output ? "" : '<span class="muted">（还没有下载过）</span>'}</td></tr>` : ""}
      </table>
      <p class="hint">下载 Job 写入这里；控制台只读挂载，预览 A 直接读这里的 z/x/y 文件。</p>
    </div>
    <div class="section"><h3>上传存储（可选）· MinIO</h3>
      <table class="cfg">
        ${cfgRow("MINIO_ENDPOINT", sto.minioEndpoint)}
        ${sto.minioReady ? cfgRow("MINIO_BUCKET", sto.minioBucket) + cfgRow("MINIO_ACCESS_KEY", sto.minioAccessKey) : ""}
        <tr><td>打包上传</td><td>${sto.upload ? '<code>可用</code>' : `<span class="muted">${esc(sto.uploadReason || "不可用")}</span>`}</td></tr>
        ${l && sto.minioReady ? `<tr><td>本图层对象</td><td><code>${esc(l.pmtilesKey)}</code> ${l.pmtiles ? `<span class="muted">${fmtSize(l.pmtiles.size)} · ${fmtTime(l.pmtiles.modified)}</span>` : '<span class="muted">（尚未打包）</span>'}</td></tr>` : ""}
      </table>
      <p class="hint">${sto.upload ? "打包后的 pmtiles 上传到这里；预览 B 读它。" : "没配置时只下载，不显示打包上传和预览 B。"}</p>
    </div>
    <div class="section"><h3>预览来源</h3>
      <table class="cfg">
        <tr><td>A 下载目录</td><td><code>随下载存储启用</code></td></tr>
        ${cfgRow("B PMTILES_PUBLIC_URL", sto.pmtilesPublicUrl)}
        ${sto.minioReady && !(sto.pmtilesPublicUrl && sto.pmtilesPublicUrl.value) ? '<tr><td></td><td class="muted">没配公开地址：B 经控制台 /pmtiles/ 中转</td></tr>' : ""}
        ${cfgRow("C TILE_SERVER_URL", sto.tileServer)}
        ${sto.tileServer && sto.tileServer.value ? cfgRow("C TILE_SERVER_PREFIX", sto.tileServerPrefix) : ""}
        ${l && l.tileServerPath ? `<tr><td>本图层瓦片集</td><td><code>${esc(l.tileServerPath)}</code></td></tr>` : ""}
      </table>
      <p class="hint">取值顺序：环境变量 → layers.json → 默认值，标签表示当前生效值的来源。</p>
    </div>`;
}

function renderPanelHead() {
  const l = currentLayer();
  const head = $("#panel-head");
  if (!l || !head) return;
  head.innerHTML = `
    <h2>${esc(l.title || l.id)}</h2>
    <div class="kv">
      <span>名称 <code>${esc(l.name || "?")}</code>${fromTag(l.nameFrom)}</span>
      <span>格式 <code>${esc(l.format || "?")}</code>${fromTag(l.formatFrom)}</span>
      <span>层级 <code>${l.min}-${l.max}</code>${fromTag(l.zoomFrom)}</span>
    </div>
    <div class="kv" style="margin-top:4px">
      <span>目录 <code>${esc(l.tileName || "?")}</code></span>
      <span>配置层级 <code>${l.confMin}-${l.confMax}</code></span>
      <span>区域 <code>${l.lrsCount}</code></span>
      ${l.meta && l.meta.tileCount ? `<span>瓦片 <code>${Number(l.meta.tileCount).toLocaleString()}</code></span>` : ""}
    </div>
    <div class="kv" style="margin-top:4px">
      <span>pmtiles <code>${l.pmtiles ? fmtSize(l.pmtiles.size) + " · " + fmtTime(l.pmtiles.modified) : state.info.storage && state.info.storage.minioReady ? "未打包" : "未配置 MinIO"}</code></span>
      <span>下载目录 <code>${l.output ? "有" : "无"}</code></span>
    </div>`;
  const banners = $("#panel-banners");
  let html = "";
  if (l.error) html += `<div class="banner danger">${esc(l.error)}</div>`;
  if (l.missing && l.missing.length) html += `<div class="banner">pmtiles 缺少 ${l.missing.map(esc).join("、")}，以上按 ConfigMap 显示（标「配置」的项）。多半是早期打包时瓦片目录没有 metadata.json；重新下载并打包后会补齐。</div>`;
  if (l.mismatch && l.mismatch.length) html += `<div class="banner">pmtiles 与配置不一致：${l.mismatch.map(esc).join("；")}</div>`;
  if (l.sharedWith && l.sharedWith.length) html += `<div class="banner">与 ${l.sharedWith.map(esc).join("、")} 共用目录 <b>${esc(l.tileName)}</b>，同一时间只能有一个任务；pmtiles 的 bounds 以最后一次执行的配置为准。</div>`;
  const running = l.lastJob && (l.lastJob.phase === "Running" || l.lastJob.phase === "Pending");
  if (running) html += `<div class="banner">任务 ${esc(l.lastJob.name)} ${PHASE_TEXT[l.lastJob.phase]}，结束前不能再派发。</div>`;
  if (l.pendingPack && !running) html += `
    <div class="banner pack">
      <div>下载 <b>${esc(l.pendingPack.job)}</b> 已于 ${fmtTime(l.pendingPack.since)} 完成，线上 pmtiles 还是${l.pmtiles ? " " + fmtTime(l.pmtiles.modified) + " 的版本" : "空的"}。确认下载结果后再打包上传。</div>
      <div class="banner-actions">
        <button class="btn small" type="button" data-view-job="${esc(l.pendingPack.job)}">看下载日志</button>
        <button class="btn small primary" type="button" data-pack>打包上传</button>
      </div>
    </div>`;
  banners.innerHTML = html;
  banners.querySelector("[data-pack]")?.addEventListener("click", packLayer);
  banners.querySelector("[data-view-job]")?.addEventListener("click", (e) => {
    state.tab = "jobs";
    renderPanel();
    loadJobs().then(() => openJob(e.target.dataset.viewJob));
  });
}

function renderRegions() {
  const body = $("#tab-body");
  const d = state.detail;
  const l = currentLayer();
  const lrsHTML = !d ? '<div class="muted">加载中…</div>' : d.lrs.map((x, i) => `
      <div class="lrs-item ${state.lrsHidden.has(i) ? "off" : ""} ${isCopySource(x) ? "src" : ""}" data-i="${i}" title="单击定位，Alt+单击显示/隐藏">
        <span class="swatch" style="background:${PALETTE[i % PALETTE.length]}"></span>
        <span class="lrs-key">${esc(x.key)}${x.missing ? ' <span class="lrs-missing">缺少 geojson</span>' : ""}</span>
        <span class="lrs-zoom">z${x.min}-${x.max}</span>
        ${x.missing ? "<span></span>" : `<button class="btn small copy-btn" type="button" data-copy="${i}" title="把这个区域带到下方表单，派发到其他图层">复制到…</button>`}
      </div>`).join("");
  const zmin = Math.max(0, (l && l.confMax) ? Math.min(l.confMax + 1, 18) : 13);
  const proxyEnabled = state.info.proxy;
  body.innerHTML = `
    <div class="section">
      <h3>已配置区域 <span class="muted" style="letter-spacing:0;font-weight:400">单击定位 · Alt+单击隐藏</span></h3>
      ${lrsHTML || '<div class="muted">没有 lrs</div>'}
    </div>
    <div class="section" id="new-region">
      <h3>新增区域</h3>
      ${state.copy ? `
      <div class="copy-src">
        <div>复制 <b>${esc(state.copy.title)}</b> 的已配置区域 <code>${esc(state.copy.key)}</code>
          <div class="hint">原层级 z${state.copy.min}-${state.copy.max}，可以改；在下面勾选要派发的图层。</div></div>
        <button class="icon-btn" id="copy-clear" type="button" title="不复制了，改为自己圈选" aria-label="取消复制">✕</button>
      </div>` : ""}
      <div class="tools" id="tools" ${state.copy ? "hidden" : ""}>
        <button class="btn small" data-mode="rect" type="button">▭ 矩形</button>
        <button class="btn small" data-mode="poly" type="button">⬠ 多边形</button>
        <button class="btn small" id="import-btn" type="button">导入 GeoJSON</button>
        <button class="btn small ghost" id="clear-btn" type="button">清除</button>
        <input type="file" id="import-file" accept=".geojson,.json,application/geo+json,application/json" hidden>
      </div>
      <label class="field"><span>区域名称</span><input id="f-name" maxlength="60" placeholder="例如 hangzhou-west"></label>
      <div class="row">
        <label class="field"><span>起始层级</span><input id="f-min" type="number" min="0" max="22" value="${zmin}"></label>
        <label class="field"><span>结束层级</span><input id="f-max" type="number" min="0" max="22" value="${Math.min(zmin + 2, 22)}"></label>
      </div>
      <div class="estimate" id="estimate"><span>先在地图上圈选区域</span></div>
      <div class="targets" id="targets"></div>
      <label class="field"><span>下载代理</span>
        <select id="f-proxy">
          <option value="default">按图层默认${l && l.proxy ? "（tiler-proxy-config）" : "（直连）"}</option>
          <option value="direct">直连，不走代理</option>
          <option value="pool" ${proxyEnabled ? "" : "disabled"}>代理池</option>
          <option value="upstream" ${proxyEnabled ? "" : "disabled"}>固定上游代理</option>
        </select>
      </label>
      <label class="field" id="f-proxy-url-wrap" hidden><span id="f-proxy-url-label">代理地址</span><input id="f-proxy-url" spellcheck="false"></label>
      <p class="hint" id="proxy-hint"></p>
      ${state.info.storage && state.info.storage.upload
        ? '<label class="check autopack"><input type="checkbox" id="f-autopack"> 下载完成后自动打包上传</label><p class="hint" id="autopack-hint"></p>'
        : `<p class="hint">${esc((state.info.storage && state.info.storage.uploadReason) || "未配置上传存储")}。下载结果在下载目录里，可在地图左上角切到「A 下载目录」查看。</p>`}
      <div class="actions">
        <button class="btn primary" id="dispatch-btn" type="button">派发下载</button>
        <button class="btn" id="rerun-btn" type="button">重新下载</button>
      </div>
      <div id="multi-result"></div>
      <p class="hint">新区域会追加到各图层 ConfigMap 的 [[lrs]]，geojson 写入对应的 geojson ConfigMap（共用的只写一份）；已下载的瓦片会跳过。</p>
    </div>`;

  body.querySelectorAll("[data-copy]").forEach((b) => b.addEventListener("click", (e) => {
    e.stopPropagation();
    startCopy(Number(b.dataset.copy));
  }));
  $("#copy-clear")?.addEventListener("click", () => cancelCopy());
  body.querySelectorAll(".lrs-item").forEach((el) => el.addEventListener("click", (e) => {
    const i = Number(el.dataset.i);
    if (e.altKey) {
      state.lrsHidden.has(i) ? state.lrsHidden.delete(i) : state.lrsHidden.add(i);
      el.classList.toggle("off");
      drawLrs();
    } else if (d.lrs[i].data) fitTo(d.lrs[i].data);
  }));
  body.querySelectorAll("[data-mode]").forEach((b) => b.addEventListener("click", () => {
    state.draw.mode === b.dataset.mode ? stopDraw() : startDraw(b.dataset.mode);
  }));
  $("#import-btn").addEventListener("click", () => $("#import-file").click());
  $("#import-file").addEventListener("change", async (e) => {
    const file = e.target.files[0];
    e.target.value = "";
    if (!file) return;
    try {
      const fc = normalizeImport(JSON.parse(await file.text()));
      setDraft(fc);
      fitTo(fc);
      const nameInput = $("#f-name");
      if (!nameInput.value) nameInput.value = file.name.replace(/\.(geo)?json$/i, "");
      renderDispatchState();
    } catch (ex) { toast("导入失败：" + ex.message, true); }
  });
  $("#clear-btn").addEventListener("click", () => { stopDraw(); setDraft(null); });
  ["#f-min", "#f-max", "#f-name"].forEach((s) => $(s).addEventListener("input", () => { renderEstimate(); renderTargets(); renderDispatchState(); }));
  $("#f-proxy").addEventListener("change", renderProxyField);
  $("#f-proxy-url").addEventListener("input", renderDispatchState);
  $("#f-autopack")?.addEventListener("change", renderAutopackHint);
  $("#dispatch-btn").addEventListener("click", dispatchRegion);
  $("#rerun-btn").addEventListener("click", rerun);
  if (state.copy) {
    $("#f-name").value = regionName(state.copy.key);
    $("#f-min").value = state.copy.min;
    $("#f-max").value = state.copy.max;
  }
  renderTools();
  renderEstimate();
  renderTargets();
  renderMultiResult();
  renderProxyField();
  renderAutopackHint();
  renderDispatchState();
}

// ---- 复制已配置区域 / 派发到多个图层 ----
const regionName = (key) => key.replace(/\.geojson$/i, "").replace(/-\d{8}-\d{6}$/, "");
const isCopySource = (x) => !!state.copy && state.copy.layer === state.selected && state.copy.key === x.key;

function startCopy(i) {
  const l = currentLayer(), x = state.detail && state.detail.lrs[i];
  if (!l || !x || !x.data) return;
  stopDraw();
  state.copy = { layer: l.id, title: l.title || l.id, key: x.key, cm: l.geojsonConfigMap, min: x.min, max: x.max };
  state.targets = new Set();
  state.multiResult = null;
  renderRegions();
  setDraft(x.data);
  fitTo(x.data);
  $("#new-region").scrollIntoView({ behavior: "smooth", block: "start" });
}

function cancelCopy() {
  state.copy = null;
  state.targets = new Set(state.selected ? [state.selected] : []);
  setDraft(null);
  renderRegions();
}

// 目标图层不能派发的原因；空串表示可以
function targetBlock(t) {
  if (!t.tileName) return "配置读取失败";
  if (t.lastJob && (t.lastJob.phase === "Running" || t.lastJob.phase === "Pending")) return "有任务在执行";
  if (state.copy && t.geojsonConfigMap === state.copy.cm) {
    const { zmin, zmax } = zoomRange();
    if ((t.regions || []).some((r) => r.key === state.copy.key && r.min === zmin && r.max === zmax)) return "已有此区域";
  }
  return "";
}

const pickedTargets = () => state.layers.filter((t) => state.targets.has(t.id) && !targetBlock(t));

function renderTargets() {
  const box = $("#targets");
  if (!box) return;
  const { zmax, valid } = zoomRange();
  const groups = [...new Set(state.layers.map((t) => t.group || "其他"))];
  const n = pickedTargets().length;
  box.innerHTML = `
    <div class="targets-head"><span>派发到图层 · 已选 ${n}</span>
      <span><button class="btn small ghost" type="button" data-pick="group">同组全选</button><button class="btn small ghost" type="button" data-pick="none">清空</button></span></div>
    ${groups.map((g) => `<div class="targets-group">${esc(g)}</div>` + state.layers.filter((t) => (t.group || "其他") === g).map((t) => {
      const why = targetBlock(t);
      const up = !why && state.draft && valid && t.confMax != null && zmax > t.confMax ? `<span class="t-note warn" title="tm.max 会从 ${t.confMax} 提到 ${zmax}">z${t.confMax} → z${zmax}</span>` : "";
      return `<label class="t-row ${why ? "dis" : ""}">
        <input type="checkbox" data-target="${esc(t.id)}" ${state.targets.has(t.id) && !why ? "checked" : ""} ${why ? "disabled" : ""}>
        <span class="t-name">${esc(t.title || t.id)}${t.id === state.selected ? ' <span class="muted">（当前）</span>' : ""}</span>
        ${why ? `<span class="t-note ${why === "有任务在执行" ? "run" : ""}">${why}</span>` : up}
      </label>`;
    }).join("")).join("")}`;
  box.querySelectorAll("[data-target]").forEach((c) => c.addEventListener("change", () => {
    c.checked ? state.targets.add(c.dataset.target) : state.targets.delete(c.dataset.target);
    renderTargets();
    renderDispatchState();
  }));
  box.querySelector('[data-pick="group"]').addEventListener("click", () => {
    const g = (currentLayer() || {}).group || "其他";
    state.layers.filter((t) => (t.group || "其他") === g && !targetBlock(t)).forEach((t) => state.targets.add(t.id));
    renderTargets();
    renderDispatchState();
  });
  box.querySelector('[data-pick="none"]').addEventListener("click", () => {
    state.targets.clear();
    renderTargets();
    renderDispatchState();
  });
}

function renderMultiResult() {
  const box = $("#multi-result");
  if (!box) return;
  const rs = state.multiResult;
  if (!rs || !rs.length) { box.innerHTML = ""; return; }
  box.innerHTML = `<div class="multi-result">${rs.map((r) => `
    <div class="mr-row">
      <span>${esc(r.t.title || r.t.id)}</span>
      ${r.ok
        ? `<span class="mr-ok">${r.dryRun ? "演练通过" : "已派发"} <button class="link" type="button" data-open-job="${esc(r.job)}" data-layer="${esc(r.t.id)}">${esc(r.job)}</button></span>`
        : `<span class="mr-err" title="${esc(r.msg)}">失败：${esc(r.msg)}</span>`}
    </div>`).join("")}</div>`;
  box.querySelectorAll("[data-open-job]").forEach((b) => b.addEventListener("click", async () => {
    const job = b.dataset.openJob;
    await selectLayer(b.dataset.layer);
    state.tab = "jobs";
    renderPanel();
    await loadJobs();
    openJob(job);
  }));
}

function renderAutopackHint() {
  const el = $("#autopack-hint");
  if (!el) return;
  el.textContent = $("#f-autopack").checked
    ? "下载结束后在同一个 Job 里直接打包，覆盖线上 pmtiles，中间不再确认。"
    : "只下载到目录。完成后图层会标记「待打包」，确认下载结果后再点「打包上传」。";
}

const autoPack = () => !!$("#f-autopack")?.checked;
const packText = (on) => (!(state.info.storage && state.info.storage.upload)
  ? "只下载（未配置上传存储）"
  : on ? "下载完成后自动打包上传" : "只下载，完成后手动确认打包");

function renderTools() {
  document.querySelectorAll("#tools [data-mode]").forEach((b) => b.classList.toggle("on", b.dataset.mode === state.draw.mode));
}

function zoomRange() {
  const zmin = parseInt($("#f-min")?.value, 10), zmax = parseInt($("#f-max")?.value, 10);
  return { zmin, zmax, valid: Number.isInteger(zmin) && Number.isInteger(zmax) && zmin >= 0 && zmax <= 22 && zmin <= zmax };
}

function renderEstimate() {
  const el = $("#estimate");
  if (!el) return;
  if (!state.draft) { el.className = "estimate"; el.innerHTML = "<span>先在地图上圈选区域</span>"; return; }
  const { zmin, zmax, valid } = zoomRange();
  if (!valid) { el.className = "estimate"; el.innerHTML = "<span>层级范围无效（0-22，起始 ≤ 结束）</span>"; return; }
  const n = estimateTiles(state.draft, zmin, zmax);
  el.className = "estimate" + (n > 2_000_000 ? " big" : "");
  el.innerHTML = `<span>按外接矩形估算（上限）</span><b>≈ ${n.toLocaleString()} 张</b>`;
}

function renderProxyField() {
  const mode = $("#f-proxy").value;
  const wrap = $("#f-proxy-url-wrap"), input = $("#f-proxy-url"), hint = $("#proxy-hint");
  wrap.hidden = mode !== "pool" && mode !== "upstream";
  if (mode === "pool") {
    $("#f-proxy-url-label").textContent = "代理池 API（留空用 tiler-proxy-config）";
    input.placeholder = "http://proxy-pool:5010 或 https://…/get?key=…";
    hint.textContent = "填代理池地址（jhao104/proxy_pool）或带 ? 的提取链接（如青果），sidecar 取代理、测试能否返回瓦片，失败自动切换；代理账号读 Secret 里的 PROXY_AUTH。";
  } else if (mode === "upstream") {
    $("#f-proxy-url-label").textContent = "上游代理地址";
    input.placeholder = "socks5://user:pass@10.0.0.1:1080";
    hint.textContent = "所有瓦片请求经 sidecar 转发到这个上游；支持 http / https / socks5 / socks5h。地址会写进 Job 的环境变量。";
  } else if (mode === "direct") {
    hint.textContent = "下载器直接访问瓦片源，不起代理 sidecar。";
  } else {
    hint.textContent = "";
  }
  renderDispatchState();
}

function proxyPayload() {
  const mode = $("#f-proxy").value;
  const url = ($("#f-proxy-url").value || "").trim();
  return mode === "pool" || mode === "upstream" ? { mode, url } : { mode };
}

function proxyLabel(p) {
  const l = currentLayer();
  if (p.mode === "default") return l && l.proxy ? "按图层默认（tiler-proxy-config）" : "直连";
  if (p.mode === "direct") return "直连";
  const masked = p.url ? p.url.replace(/\/\/[^@/]+@/, "//***@") : "";
  return (p.mode === "pool" ? "代理池 " : "固定上游 ") + (masked || "（tiler-proxy-config）");
}

function busyReason() {
  const l = currentLayer();
  if (!state.info.writable) return "服务端未配置口令，写操作已禁用";
  if (!l || !l.tileName) return "图层配置读取失败";
  if (l.lastJob && (l.lastJob.phase === "Running" || l.lastJob.phase === "Pending")) return "已有任务在执行";
  return "";
}

function renderDispatchState() {
  const btn = $("#dispatch-btn"), rerunBtn = $("#rerun-btn");
  if (!btn) return;
  const busy = busyReason();
  const { valid } = zoomRange();
  const p = proxyPayload();
  const proxyOk = !(p.mode === "upstream" && !p.url);
  const name = ($("#f-name").value || "").trim();
  const n = pickedTargets().length;
  const ro = state.info.writable ? "" : "服务端未配置口令，写操作已禁用";
  btn.disabled = !!ro || !state.draft || !valid || !name || !proxyOk || !n;
  btn.textContent = n > 1 ? `派发到 ${n} 个图层` : "派发下载";
  btn.title = ro || (!state.draft ? "先圈选区域" : !name ? "填写区域名称" : !valid ? "层级范围无效" : !proxyOk ? "填写上游代理地址" : !n ? "勾选要派发的图层" : "");
  rerunBtn.disabled = !!busy || !proxyOk;
  rerunBtn.title = busy || "不改配置，按当前 conf.toml 补齐缺失瓦片";
}

async function dispatchRegion() {
  const targets = pickedTargets();
  if (!targets.length) return;
  const { zmin, zmax } = zoomRange();
  const name = $("#f-name").value.trim();
  const proxy = proxyPayload();
  const ap = autoPack();
  const n = estimateTiles(state.draft, zmin, zmax);
  const copy = state.copy;
  // 每个图层的 geojson 去向：同一个 geojson ConfigMap 只写一次，其余引用
  const written = new Set();
  const rows = targets.map((t) => {
    let geo;
    if (copy && t.geojsonConfigMap === copy.cm) geo = `引用 ${esc(t.geojsonConfigMap)} 里的 ${esc(copy.key)}`;
    else if (written.has(t.geojsonConfigMap)) geo = `引用同一份 geojson（${esc(t.geojsonConfigMap)}）`;
    else { written.add(t.geojsonConfigMap); geo = `写入 ${esc(t.geojsonConfigMap)}`; }
    const up = zmax > t.confMax ? ` <span class="tag warn">tm.max ${t.confMax} → ${zmax}</span>` : "";
    return `<tr><td>${esc(t.title || t.id)}</td><td>追加 [[lrs]] z${zmin}-${zmax}${up}<div class="muted">${esc(t.configMap)} · ${geo}</div></td></tr>`;
  }).join("");
  const ok = await confirmDialog(targets.length > 1 ? `派发到 ${targets.length} 个图层` : "派发下载", `
    <dl class="summary">
      <dt>区域</dt><dd>${esc(name)}${copy ? `（复制自${esc(copy.title)}的 ${esc(copy.key)}）` : ""}</dd>
      <dt>层级</dt><dd>z${zmin} - z${zmax}</dd>
      <dt>估算</dt><dd>每个图层 ≤ ${n.toLocaleString()} 张瓦片</dd>
      <dt>代理</dt><dd>${esc(proxyLabel(proxy))}</dd>
      <dt>打包</dt><dd>${packText(ap)}</dd>
    </dl>
    <table class="plan"><thead><tr><th>图层</th><th>会做什么</th></tr></thead><tbody>${rows}</tbody></table>
    <p class="hint">每个图层各建一个 Job，互不影响；某个图层失败不影响其他图层，结果逐个列出。${state.info.dryRun ? "<br>当前是演练模式，不会真正写入。" : ""}</p>`, "派发");
  if (!ok) return;
  const btn = $("#dispatch-btn");
  btn.disabled = true;
  btn.textContent = "派发中…";
  // 复制时各图层都引用源区域；新画的区域第一个图层写入后，其余图层引用它
  let from = copy ? { layer: copy.layer, key: copy.key } : null;
  const results = [];
  for (const t of targets) {
    const body = { name, min: zmin, max: zmax, proxy, autoPack: ap };
    if (from) body.from = from; else body.geojson = state.draft;
    try {
      const res = await api(`/api/layers/${encodeURIComponent(t.id)}/regions`, { method: "POST", auth: true, body });
      results.push({ t, ok: true, job: res.job, dryRun: res.dryRun, res });
      if (!from && !res.dryRun) from = { layer: t.id, key: res.geojsonKey };
    } catch (e) {
      results.push({ t, ok: false, msg: e.message });
      if (!state.token) break; // 没解锁或口令失效，后面的也会失败
    }
  }
  btn.textContent = "派发下载";
  const okN = results.filter((r) => r.ok).length;
  if (targets.length === 1 && okN === 1) {
    const res = results[0].res;
    toast(res.dryRun ? `演练通过：将创建 ${res.job}` : `已派发 ${res.job}`);
    state.copy = null;
    setDraft(null);
    await afterDispatch(res);
    return;
  }
  if (targets.length === 1) { toast(results[0].msg, true); renderDispatchState(); return; }
  const failed = results.filter((r) => !r.ok);
  toast(`已派发 ${okN} 个图层${failed.length ? `，失败 ${failed.length} 个` : ""}`, failed.length > 0);
  if (!failed.length) {
    state.copy = null;
    setDraft(null);
    state.targets = new Set(state.selected ? [state.selected] : []);
  } else {
    // 保留区域和复制来源，只勾上失败的图层，处理完原因后可直接重试
    state.targets = new Set(failed.map((r) => r.t.id));
  }
  state.multiResult = results;
  await loadLayers();
  if (okN && results.some((r) => r.ok && r.t.id === state.selected)) {
    const detail = await api("/api/layers/" + encodeURIComponent(state.selected)).catch(() => null);
    if (detail) { state.detail = detail; drawLrs(); }
  }
  if (state.tab === "regions") renderRegions();
}

async function rerun() {
  const l = currentLayer();
  const proxy = proxyPayload();
  const ok = await confirmDialog("重新下载", `
    <dl class="summary">
      <dt>图层</dt><dd>${esc(l.title)}（${esc(l.tileName)}）</dd>
      <dt>配置</dt><dd>${esc(l.configMap)}，${l.lrsCount} 段 lrs，z${l.confMin}-${l.confMax}</dd>
      <dt>代理</dt><dd>${esc(proxyLabel(proxy))}</dd>
      <dt>打包</dt><dd>${packText(autoPack())}</dd>
    </dl>
    <p class="hint">不修改配置。已有瓦片会跳过，只补齐缺失的。</p>`, "下载");
  if (!ok) return;
  try {
    const res = await api(`/api/layers/${encodeURIComponent(l.id)}/jobs`, { method: "POST", auth: true, body: { proxy, autoPack: autoPack() } });
    toast(res.dryRun ? `演练通过：将创建 ${res.job}` : `已派发 ${res.job}`);
    await afterDispatch(res);
  } catch (e) { toast(e.message, true); }
}

async function packLayer() {
  const l = currentLayer();
  if (!l) return;
  const busy = busyReason();
  if (busy) { toast(busy, true); return; }
  const ok = await confirmDialog("打包上传", `
    <dl class="summary">
      <dt>图层</dt><dd>${esc(l.title)}（目录 ${esc(l.tileName)}）</dd>
      ${l.pendingPack ? `<dt>下载</dt><dd>${esc(l.pendingPack.job)} · ${fmtTime(l.pendingPack.since)} 完成</dd>` : ""}
      <dt>目标</dt><dd>${esc(l.pmtilesKey)}</dd>
      <dt>线上</dt><dd>${l.pmtiles ? fmtSize(l.pmtiles.size) + " · " + fmtTime(l.pmtiles.modified) : "尚未生成"}</dd>
    </dl>
    <p class="hint">会把整个目录（不只是新区域）重新打包成 pmtiles，并覆盖 MinIO 上的同名文件。目录大时耗时较长（GB 级约 1 小时），期间该目录不能再派发任务。${state.info.dryRun ? "<br>当前是演练模式，不会真正执行。" : ""}</p>`, "打包上传");
  if (!ok) return;
  try {
    const res = await api(`/api/layers/${encodeURIComponent(l.id)}/pack`, { method: "POST", auth: true });
    toast(res.dryRun ? `演练通过：将创建 ${res.job}` : `已派发打包 ${res.job}`);
    await afterDispatch(res);
  } catch (e) { toast(e.message, true); }
}

async function afterDispatch(res) {
  if (res.dryRun) return;
  await loadLayers();
  const detail = await api("/api/layers/" + encodeURIComponent(state.selected)).catch(() => null);
  if (detail) { state.detail = detail; drawLrs(); }
  state.tab = "jobs";
  renderPanel();
  await loadJobs();
  openJob(res.job);
}

// ---- 任务 ----
function renderJobs() {
  const body = $("#tab-body");
  if (!body || state.tab !== "jobs") return;
  const items = state.jobs.map((j) => `
    <div class="job-item ${state.activeJob === j.name ? "active" : ""}" data-name="${esc(j.name)}">
      <div class="job-top">
        <span class="job-name"><span class="dot ${j.phase}"></span>${j.kind ? `<span class="tag">${KIND_TEXT[j.kind] || j.kind}</span>` : ""}${esc(j.name)}</span>
        <span class="job-time">${fmtAgo(j.created)}</span>
      </div>
      <div class="job-meta">${PHASE_TEXT[j.phase] || j.phase}${j.started ? " · 用时 " + fmtDur(j.started, j.completed) : ""}${j.managed ? "" : " · 旧 Job"}</div>
      ${j.reason ? `<div class="job-meta">${esc(j.reason)}</div>` : ""}
      ${j.proxy ? `<div class="job-meta muted">代理：${esc(j.proxy)}</div>` : ""}
      ${j.message && j.phase === "Failed" ? `<div class="job-meta" style="color:var(--danger)">${esc(j.message)}</div>` : ""}
      ${pendingJob() === j.name ? `<div class="job-ops"><button class="btn small primary" type="button" data-pack>打包上传</button><span class="hint" style="margin:0">这次下载还没打包</span></div>` : ""}
      <div class="job-containers" data-containers></div>
    </div>`).join("");
  body.innerHTML = `
    <div class="section">
      <h3>
        <span>任务</span>
        <label class="check" style="letter-spacing:0;font-weight:400"><input type="checkbox" id="legacy-toggle" ${state.showLegacy ? "checked" : ""}> 含旧 Job</label>
      </h3>
      ${items || '<div class="muted">还没有任务</div>'}
    </div>`;
  $("#legacy-toggle").addEventListener("change", (e) => { state.showLegacy = e.target.checked; loadJobs(); });
  body.querySelectorAll(".job-item").forEach((el) => el.addEventListener("click", () => openJob(el.dataset.name)));
  body.querySelectorAll("[data-pack]").forEach((b) => b.addEventListener("click", (e) => { e.stopPropagation(); packLayer(); }));
  if (state.activeJob) refreshJobDetail();
}

const pendingJob = () => currentLayer()?.pendingPack?.job;

const finished = (job) => job.phase === "Succeeded" || job.phase === "Failed";

async function refreshJobDetail() {
  const name = state.activeJob;
  if (!name) return;
  let d;
  try { d = await api("/api/jobs/" + encodeURIComponent(name)); } catch { return; }
  if (state.activeJob !== name) return;
  const el = document.querySelector(`.job-item[data-name="${CSS.escape(name)}"] [data-containers]`);
  const pod = d.pods[0];
  if (el) {
    el.innerHTML = pod
      ? pod.containers.map((c) => `<span class="cstate ${c.state}" title="${esc(c.detail || "")}">${esc(c.name)} ${STATE_TEXT[c.state] || c.state}${c.restarts ? " ↻" + c.restarts : ""}</span>`).join("")
      : `<span class="cstate">${finished(d.job) ? "Pod 已清理，日志不可用" : "Pod 尚未创建"}</span>`;
    const item = el.closest(".job-item");
    if (d.job.managed && !item.querySelector(".job-ops.del")) {
      const ops = document.createElement("div");
      ops.className = "job-ops del";
      ops.innerHTML = `<button class="btn small danger" type="button">${d.job.phase === "Running" || d.job.phase === "Pending" ? "终止并删除" : "删除"}</button>`;
      ops.querySelector("button").addEventListener("click", (e) => { e.stopPropagation(); deleteJob(name, d.job.phase); });
      item.appendChild(ops);
    }
  }
  renderLogTabs(d);
}

async function deleteJob(name, phase) {
  const running = phase === "Running" || phase === "Pending";
  const ok = await confirmDialog(running ? "终止任务" : "删除任务",
    `<p>${running ? "会立即停止下载并删除" : "删除"} <code>${esc(name)}</code> 及其 Pod，日志随之消失。已下载的瓦片保留在目录里。</p>`, running ? "终止" : "删除");
  if (!ok) return;
  try {
    const res = await api("/api/jobs/" + encodeURIComponent(name), { method: "DELETE", auth: true });
    toast(res.dryRun ? "演练通过：将删除 " + name : "已删除 " + name);
    if (!res.dryRun) {
      if (state.log.job === name) closeLog();
      state.activeJob = null;
      await loadLayers();
      await loadJobs();
    }
  } catch (e) { toast(e.message, true); }
}

// ---- 配置 ----
function renderConf() {
  const body = $("#tab-body");
  const d = state.detail;
  body.innerHTML = `<div class="section"><h3>conf.toml（${esc(currentLayer().configMap)}）</h3>${d ? `<pre class="conf">${esc(d.conf)}</pre>` : '<div class="muted">加载中…</div>'}<p class="hint">tk / key / access_token 已隐去。</p></div>`;
}

// ============ 日志 ============
function openJob(name) {
  state.activeJob = name;
  if (state.tab === "jobs") renderJobs();
  $("#log-drawer").hidden = false;
  $("#log-job").textContent = name;
  if (state.log.job !== name) {
    stopLog();
    state.log = { job: name, pod: null, container: null, ctrl: null, auto: true };
    $("#log-body").textContent = "";
    $("#log-tabs").innerHTML = "";
  }
  refreshJobDetail();
}

// 默认看正在跑的容器：download 优先，其次 sync，最后 proxy
function pickContainer(pod) {
  const order = ["download", "sync", "proxy"];
  const running = pod.containers.filter((c) => c.state === "running").sort((a, b) => order.indexOf(a.name) - order.indexOf(b.name));
  if (running.length) return running[0].name;
  const failed = pod.containers.find((c) => c.state === "failed");
  if (failed) return failed.name;
  const done = pod.containers.filter((c) => c.state === "done");
  return done.length ? done[done.length - 1].name : pod.containers[0]?.name;
}

function renderLogTabs(d) {
  if (state.log.job !== d.job.name) return;
  const pod = d.pods[0];
  const tabs = $("#log-tabs");
  if (!pod) {
    $("#log-pod").textContent = "";
    tabs.innerHTML = "";
    $("#log-body").textContent = finished(d.job) ? "该任务已结束，Pod 已被清理，日志不可用。" : "等待 Pod 创建…";
    return;
  }
  $("#log-pod").textContent = pod.name + (pod.node ? " @ " + pod.node : "");
  // 没手动选过容器时跟着阶段走：download 结束后自动切到 sync
  const want = pickContainer(pod);
  if (state.log.pod !== pod.name || !state.log.container || (state.log.auto && want !== state.log.container)) {
    state.log.pod = pod.name;
    state.log.container = want;
    startLog();
  }
  tabs.innerHTML = pod.containers.map((c) => `
    <button class="log-tab ${c.name === state.log.container ? "active" : ""}" data-c="${esc(c.name)}" type="button">
      <span class="dot ${({ running: "Running", done: "Succeeded", failed: "Failed", waiting: "Pending" })[c.state]}"></span>${esc(c.name)}${c.kind === "sidecar" ? " · sidecar" : ""}
    </button>`).join("");
  tabs.querySelectorAll(".log-tab").forEach((b) => b.addEventListener("click", () => {
    state.log.container = b.dataset.c;
    state.log.auto = false;
    tabs.querySelectorAll(".log-tab").forEach((x) => x.classList.toggle("active", x === b));
    startLog();
  }));
}

function stopLog() {
  if (state.log.ctrl) state.log.ctrl.abort();
  state.log.ctrl = null;
}

const LOG_LIMIT = 1_500_000;

async function startLog() {
  stopLog();
  const { job, pod, container } = state.log;
  if (!job || !pod || !container) return;
  const ctrl = new AbortController();
  state.log.ctrl = ctrl;
  const body = $("#log-body");
  body.textContent = "";
  const follow = $("#log-follow").checked;
  const qs = new URLSearchParams({ pod, container, tail: "1000", follow: follow ? "1" : "0" });
  try {
    const res = await fetch(`/api/jobs/${encodeURIComponent(job)}/logs?${qs}`, { signal: ctrl.signal });
    if (!res.ok) {
      const d = await res.json().catch(() => ({}));
      body.textContent = "读取日志失败：" + (d.error || res.statusText);
      return;
    }
    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let pending = "";
    let scheduled = false;
    const flush = () => {
      scheduled = false;
      if (!pending) return;
      const atBottom = body.scrollHeight - body.scrollTop - body.clientHeight < 40;
      body.append(pending);
      pending = "";
      if (body.textContent.length > LOG_LIMIT) body.textContent = "…（较早的日志已截断）\n" + body.textContent.slice(-LOG_LIMIT * 0.8);
      if (atBottom) body.scrollTop = body.scrollHeight;
    };
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      pending += dec.decode(value, { stream: true });
      if (!scheduled) { scheduled = true; requestAnimationFrame(flush); }
    }
    flush();
    if (state.log.ctrl === ctrl && follow) body.append("\n— 日志流结束 —\n");
  } catch (e) {
    if (e.name !== "AbortError") body.append("\n读取中断：" + e.message + "\n");
  }
}

function closeLog() {
  stopLog();
  state.log = { job: null, pod: null, container: null, ctrl: null, auto: true };
  $("#log-drawer").hidden = true;
}

$("#log-close").addEventListener("click", closeLog);
$("#log-follow").addEventListener("change", startLog);
$("#refresh-layers").addEventListener("click", () => { loadLayers(); if (state.selected) loadJobs(); });

// ============ 启动 ============
(async function boot() {
  await loadInfo();
  await loadLayers();
  const first = state.layers.find((l) => !unavailableReason(l)) || state.layers[0];
  if (first) selectLayer(first.id);
  // 轮询：图层状态 15s，选中图层的任务 5s
  setInterval(loadLayers, 15000);
  setInterval(() => {
    if (!state.selected) return;
    loadJobs();
    if (state.activeJob) refreshJobDetail();
  }, 5000);
})();
