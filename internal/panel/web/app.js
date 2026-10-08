/* MeshConsole read-only panel (SPEC-M1b-b §4)
 * 纯原生 JS：15s 轮询 /api/panel/overview，就地 diff 更新（不重建带状态的
 * DOM，数字/状态点走过渡而非跳变）。所有动态文本经 textContent 写入，
 * 不用 innerHTML 拼数据 —— DB 内容视为不可信（DESIGN §7-8）。
 * 动效：单一 easing 签名（--ease）的延伸 —— 载入 stagger 在 CSS，数字 tween /
 * 刷新闪灯在这里且曲线与 CSS 完全同一条；prefers-reduced-motion 时全部直出。
 * 业务口径：异常服务数等由 /api/panel/overview 的 service_issues 直接输出
 * （300s 宽限在 meshview service 层统一，R19-#4），前端不做业务计算。 */
'use strict';

const POLL_MS = 15000;
const REDUCED = matchMedia('(prefers-reduced-motion: reduce)').matches;

const $ = (id) => document.getElementById(id);

/* ---------- token 派生（SVG 与 JS 需要完整色值 / easing 数值） ---------- */
// SVG 表现属性（stop-color/stroke）不接受 var()（CSS 变量在 presentation
// attribute 里非法，R19-#11）：启动时从 token 读 --primary 的 RGB 三元组拼
// 完整色值；读取失败回退到 glass.css 的字面量（token 单一源不变）。
const PRIMARY_RGB = (() => {
  try {
    const raw = getComputedStyle(document.body).getPropertyValue('--primary').trim();
    if (/^\d{1,3}\s+\d{1,3}\s+\d{1,3}$/.test(raw)) return raw;
  } catch (e) { /* 环境无 CSSOM 时走回退 */ }
  return '52 211 153';
})();

// JS easing 与 CSS 签名统一（R19-#11）：glass.css --ease 同一条曲线
// cubic-bezier(0.22, 1, 0.36, 1)。贝塞尔求值：牛顿迭代解 t 使 x(t)=x
// （该曲线 x(t) 在 [0,1] 严格单调，收敛稳定），再取 y(t)。
const EASE = (() => {
  const x1 = 0.22, y1 = 1, x2 = 0.36, y2 = 1;
  const cx = 3 * x1, bx = 3 * (x2 - x1) - cx, ax = 1 - cx - bx;
  const cy = 3 * y1, by = 3 * (y2 - y1) - cy, ay = 1 - cy - by;
  const sampleX = (t) => ((ax * t + bx) * t + cx) * t;
  const sampleY = (t) => ((ay * t + by) * t + cy) * t;
  const sampleDX = (t) => (3 * ax * t + 2 * bx) * t + cx;
  return function ease(x) {
    if (x <= 0) return 0;
    if (x >= 1) return 1;
    let t = x;
    for (let i = 0; i < 8; i++) {
      const err = sampleX(t) - x;
      if (Math.abs(err) < 1e-5) break;
      const d = sampleDX(t);
      if (Math.abs(d) < 1e-6) break;
      t -= err / d;
    }
    return sampleY(Math.min(1, Math.max(0, t)));
  };
})();

/* ---------- 状态映射：色点 + 文字双通道 ---------- */
function statusKind(kind, s) {
  // kind: 'node' | 'service' | 'agent'
  if (kind === 'node') return s === 'online' ? 'ok' : 'crit';
  switch (s) {
    case 'active': return 'ok';
    case 'inactive': case 'stale': return 'dim';
    case 'unknown': case 'unavailable': return 'warn';
    case 'failed': return 'crit';
    default: return 'dim';
  }
}
const KIND_CLASS = { ok: 'dot--ok', warn: 'dot--warn', crit: 'dot--crit', dim: 'dot--dim' };
// 状态点与状态文字一律原地改 className/textContent（R19-#11）：.dot 的
// background/box-shadow transition 只在元素不销毁重建时才能连续过渡。
// crit 且需呼吸（offline/failed 才值得动效；warn 静默常亮）。
function setDot(el, kind, breathe) {
  el.className = 'dot ' + (KIND_CLASS[kind] || 'dot--dim') + (breathe ? ' breathing' : '');
}
function setDotText(el, kind, text) {
  el.className = 'dot-text mono' + (kind === 'crit' ? ' status-crit-text' : kind === 'warn' ? ' status-warn-text' : '');
  el.textContent = text;
}
function buildStatusCell() {
  const wrap = document.createElement('span');
  wrap.className = 'status-cell';
  const dot = document.createElement('span');
  const t = document.createElement('span');
  wrap.appendChild(dot);
  wrap.appendChild(t);
  return { wrap, dot, t };
}
function fillStatusCell(cell, kind, text, breathe) {
  setDot(cell.dot, kind, breathe);
  setDotText(cell.t, kind, text);
}

/* ---------- 格式化 ---------- */
function relTime(ts, now) {
  if (!ts) return '—';
  const d = Math.max(0, (now - ts) * 1000);
  if (d < 60000) return Math.floor(d / 1000) + 's';
  if (d < 3600000) return Math.floor(d / 60000) + 'm' + String(Math.floor((d % 60000) / 1000)).padStart(2, '0') + 's';
  return Math.floor(d / 3600000) + 'h' + String(Math.floor((d % 3600000) / 60000)).padStart(2, '0') + 'm';
}
function fmtBytes(n) {
  if (n == null) return '—';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(1)) + ' ' + u[i];
}
function fmtPct(v) { return v == null ? '—' : v.toFixed(1); }

/* ---------- 数字 tween（与 CSS --ease 完全同一条曲线） ---------- */
function tweenNum(el, to, fmt) {
  fmt = fmt || ((v) => String(v));
  if (to == null || !isFinite(to)) { el._v = null; el.textContent = '—'; return; }
  const from = el._v == null ? to : el._v;
  el._v = to;
  if (REDUCED || from === to) { el.textContent = fmt(to); return; }
  const t0 = performance.now(), dur = 650;
  (function step(t) {
    const p = Math.min(1, (t - t0) / dur);
    const e = EASE(p);
    el.textContent = fmt(from + (to - from) * e);
    if (p < 1) requestAnimationFrame(step);
  })(t0);
}

/* ---------- sparkline（内联 SVG 手绘，空值断线） ---------- */
function sparkSvg() {
  const ns = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(ns, 'svg');
  svg.setAttribute('viewBox', '0 0 100 32');
  svg.setAttribute('preserveAspectRatio', 'none');
  const grad = document.createElementNS(ns, 'linearGradient');
  grad.id = 'sg' + Math.random().toString(36).slice(2, 8);
  for (const [off, op] of [['0', '.28'], ['1', '0']]) {
    const stop = document.createElementNS(ns, 'stop');
    stop.setAttribute('offset', off);
    // 完整色值（RGB 三元组派生自 --primary token；表现属性不吃 var()）。
    stop.setAttribute('stop-color', 'rgb(' + PRIMARY_RGB + ')');
    stop.setAttribute('stop-opacity', op);
    grad.appendChild(stop);
  }
  const defs = document.createElementNS(ns, 'defs');
  defs.appendChild(grad);
  svg.appendChild(defs);
  const area = document.createElementNS(ns, 'path');
  area.setAttribute('fill', 'url(#' + grad.id + ')');
  area.setAttribute('stroke', 'none');
  const line = document.createElementNS(ns, 'path');
  line.setAttribute('fill', 'none');
  line.setAttribute('stroke', 'rgb(' + PRIMARY_RGB + ' / 0.9)');
  line.setAttribute('stroke-width', '1.4');
  line.setAttribute('vector-effect', 'non-scaling-stroke');
  line.setAttribute('stroke-linejoin', 'round');
  svg.appendChild(area);
  svg.appendChild(line);
  return { svg, area, line };
}
function sparkPaint(sp, pts) {
  // pts: [{v: number|null}]，x 均分，v∈[0,100] → y∈[3,29]；null 断段（诚实呈现缺口）。
  const W = 100, H = 32, top = 3, bot = 29;
  const n = pts.length;
  let dLine = '', started = false, firstX = null, lastX = null;
  for (let i = 0; i < n; i++) {
    const v = pts[i].v;
    if (v == null || !isFinite(v)) { started = false; continue; }
    const x = n === 1 ? W : (i * W) / (n - 1);
    const y = bot - (Math.min(100, Math.max(0, v)) / 100) * (bot - top);
    dLine += (started ? ' L' : ' M') + x.toFixed(2) + ' ' + y.toFixed(2);
    if (!started && firstX == null) firstX = x;
    started = true;
    lastX = x;
  }
  sp.line.setAttribute('d', dLine || 'M0 0');
  if (firstX == null) {
    sp.area.setAttribute('d', 'M0 ' + H + ' L' + W + ' ' + H + ' Z');
    return;
  }
  const dArea = 'M' + firstX.toFixed(2) + ' ' + H + dLine.replace(' M', ' L') +
    ' L' + lastX.toFixed(2) + ' ' + H + ' Z';
  sp.area.setAttribute('d', dArea);
}

/* ---------- 列表就地 diff：按键复用行元素，顺序对齐 ---------- */
function syncList(container, items, keyFn, build, update) {
  if (items.length) {
    // 空态提示不在行复用 map 内，拿到数据即移除，避免与节点行同显（R21-#3）。
    container.querySelectorAll(':scope > .empty').forEach((e) => e.remove());
  }
  const map = container._rows || (container._rows = new Map());
  const seen = new Set();
  items.forEach((it, idx) => {
    const k = keyFn(it);
    seen.add(k);
    let row = map.get(k);
    if (!row) {
      row = build(it);
      row._key = k;
      map.set(k, row);
    }
    update(row, it, idx);
    container.appendChild(row); // 重新 append = 按序排列（移动已存在节点）
  });
  for (const [k, row] of map) {
    if (!seen.has(k)) { row.remove(); map.delete(k); }
  }
}

/* ---------- 仪表条 ---------- */
function renderReadout(ov) {
  // 异常服务数由 overview 直接输出（R19-#4）：300s 宽限/stale 簿记态等口径
  // 全部在 meshview service 层统一，前端只消费名单长度，不做业务计算。
  const issues = (ov.service_issues || []).length;
  const online = ov.nodes.filter((n) => n.node.status === 'online').length;
  const offline = ov.nodes.length - online;
  const tailnet = ov.tailnet ? ov.tailnet.tracked : 0;

  const gOn = $('g-online'), gOff = $('g-offline'), gIss = $('g-issues'), gTn = $('g-tailnet');
  tweenNum(gOn, online);
  tweenNum(gOff, offline);
  tweenNum(gIss, issues);
  tweenNum(gTn, tailnet);
  gOn.classList.toggle('is-zero', online === 0);
  gOff.classList.toggle('has-issue', offline > 0);
  gIss.classList.toggle('has-issue', issues > 0);
  gIss.classList.toggle('is-zero', issues === 0);
  gTn.classList.toggle('is-zero', tailnet === 0);
}

/* ---------- 节点卡 ---------- */
function kvCell(label, value) {
  const c = document.createElement('div');
  const l = document.createElement('span');
  l.className = 'label';
  l.textContent = label;
  const v = document.createElement('span');
  v.className = 'val';
  c.appendChild(l);
  c.appendChild(v);
  c._val = v;
  return c;
}

function buildLeadCard() {
  const card = document.createElement('section');
  card.className = 'glass-surface node-card';
  const aura = document.createElement('span');
  aura.className = 'aura';
  aura.setAttribute('aria-hidden', 'true');
  card.appendChild(aura);

  const head = document.createElement('div');
  head.className = 'node-head';
  const name = document.createElement('span');
  name.className = 'node-name';
  const status = buildStatusCell(); // 状态点/文字原地复用（刷新连续过渡）
  head.appendChild(name);
  head.appendChild(status.wrap);
  card.appendChild(head);
  card._name = name;
  card._status = status;

  const kv = document.createElement('div');
  kv.className = 'node-kv';
  for (const [key, label] of [['role', 'role'], ['osarch', 'os / arch'], ['hb', 'heartbeat'], ['uptime', 'uptime']]) {
    const cell = kvCell(label, '');
    kv.appendChild(cell);
    card['_' + key] = cell._val;
  }
  card.appendChild(kv);

  const sparks = document.createElement('div');
  sparks.className = 'spark-row';
  card._sparks = {};
  for (const key of ['cpu', 'mem']) {
    const block = document.createElement('div');
    block.className = 'spark-block glass-inset';
    block.style.padding = '10px 12px';
    const headRow = document.createElement('div');
    headRow.className = 'spark-head';
    const lbl = document.createElement('span');
    lbl.className = 'label';
    lbl.textContent = key === 'cpu' ? 'cpu' : 'memory';
    const val = document.createElement('span');
    val.className = 'spark-val';
    const valTxt = document.createTextNode('—');
    const valUnit = document.createElement('small');
    valUnit.textContent = '%';
    val.appendChild(valTxt);
    val.appendChild(valUnit);
    headRow.appendChild(lbl);
    headRow.appendChild(val);
    const sp = sparkSvg();
    block.appendChild(headRow);
    block.appendChild(sp.svg);
    sparks.appendChild(block);
    card._sparks[key] = { valTxt, sp };
  }
  card.appendChild(sparks);

  const disk = document.createElement('div');
  disk.className = 'disk-meter';
  const diskHead = document.createElement('div');
  diskHead.className = 'spark-head';
  const diskLbl = document.createElement('span');
  diskLbl.className = 'label';
  diskLbl.textContent = 'disk';
  const diskVal = document.createElement('span');
  diskVal.className = 'spark-val';
  diskHead.appendChild(diskLbl);
  diskHead.appendChild(diskVal);
  const bar = document.createElement('div');
  bar.className = 'disk-bar';
  const fill = document.createElement('i');
  bar.appendChild(fill);
  disk.appendChild(diskHead);
  disk.appendChild(bar);
  card.appendChild(disk);
  card._diskVal = diskVal;
  card._diskBar = bar;
  card._diskFill = fill;

  return card;
}

function updateLeadCard(card, ncard, now) {
  const n = ncard.node;
  card._name.textContent = n.name;
  fillStatusCell(card._status, statusKind('node', n.status), n.status, n.status === 'offline');
  card._role.textContent = n.role || '—';
  card._osarch.textContent = (n.os || '?') + ' / ' + (n.arch || '?');
  card._hb.textContent = relTime(n.last_seen, now);
  card._hb.title = n.last_seen ? 'last_seen ' + new Date(n.last_seen * 1000).toLocaleTimeString() : '';
  const up = ncard.latest && ncard.latest.uptime_s != null ? ncard.latest.uptime_s : null;
  card._uptime.textContent = up == null ? '—' : Math.floor(up / 3600) + 'h' + String(Math.floor((up % 3600) / 60)).padStart(2, '0') + 'm';

  const cpu = ncard.latest ? ncard.latest.cpu_pct : null;
  const memPct = memPctOf(ncard);
  tweenNum(card._sparks.cpu.valTxt, cpu, (v) => v.toFixed(1));
  tweenNum(card._sparks.mem.valTxt, memPct, (v) => v.toFixed(1));
  sparkPaint(card._sparks.cpu.sp, (ncard.spark || []).map((p) => ({ v: p.cpu_pct })));
  sparkPaint(card._sparks.mem.sp, (ncard.spark || []).map((p) => ({ v: p.mem_pct })));

  const du = ncard.latest ? ncard.latest.disk_used : null;
  const dt = ncard.latest ? ncard.latest.disk_total : null;
  const pct = du != null && dt ? (du / dt) * 100 : null;
  card._diskVal.textContent = pct == null ? '—' : fmtBytes(du) + ' / ' + fmtBytes(dt);
  card._diskFill.style.width = (pct == null ? 0 : Math.min(100, pct)).toFixed(1) + '%';
  card._diskBar.classList.toggle('warn', pct != null && pct >= 85);
}

function memPctOf(ncard) {
  const m = ncard.latest;
  if (!m || m.mem_used == null || !m.mem_total) return null;
  return (m.mem_used / m.mem_total) * 100;
}

/* ---------- 次节点小卡 / 服务 / agent / tailnet 行：
     结构只在 build 建一次，update 原地改值与 className —— 状态点过渡连续。 */
function buildMini() {
  const row = document.createElement('div');
  row.className = 'node-mini glass-inset glass-inset-hover';
  const dot = document.createElement('span');
  const name = document.createElement('span');
  name.className = 'node-name';
  const meta = document.createElement('span');
  meta.className = 'mini-meta';
  const cpu = document.createElement('span');
  cpu.className = 'mini-cpu';
  row.appendChild(dot);
  row.appendChild(name);
  row.appendChild(meta);
  row.appendChild(cpu);
  row._dot = dot;
  row._name = name;
  row._meta = meta;
  row._cpu = cpu;
  return row;
}
function updateMini(row, ncard, now) {
  const n = ncard.node;
  setDot(row._dot, statusKind('node', n.status), n.status === 'offline');
  row._name.textContent = n.name;
  row._meta.textContent = (n.role || 'node') + ' · hb ' + relTime(n.last_seen, now);
  const m = memPctOf(ncard);
  row._cpu.textContent = (ncard.latest && ncard.latest.cpu_pct != null ? ncard.latest.cpu_pct.toFixed(0) : '—') + '% cpu · ' + (m == null ? '—' : m.toFixed(0)) + '% mem';
}

function buildServiceRow() {
  const row = document.createElement('div');
  row.className = 'row row-services glass-inset glass-inset-hover';
  const dot = document.createElement('span');
  const name = document.createElement('span');
  name.className = 'row-name';
  const sub = document.createElement('span');
  sub.className = 'row-sub hide-sm';
  const chip = document.createElement('span');
  chip.className = 'type-chip';
  const st = document.createElement('span');
  const detail = document.createElement('span');
  detail.className = 'detail-line';
  row.appendChild(dot);
  row.appendChild(name);
  row.appendChild(sub);
  row.appendChild(chip);
  row.appendChild(st);
  row.appendChild(detail);
  row._dot = dot;
  row._name = name;
  row._sub = sub;
  row._chip = chip;
  row._st = st;
  row._detail = detail;
  return row;
}
function updateServiceRow(row, s) {
  const kind = statusKind('service', s.status);
  setDot(row._dot, kind, s.status === 'failed');
  row._name.textContent = s.name;
  row._name.title = s.node + ' / ' + s.name;
  row._sub.textContent = s.node;
  row._chip.textContent = s.type;
  setDotText(row._st, kind, s.status);
  row._detail.textContent = s.detail || '';
  row._detail.style.display = s.detail ? '' : 'none';
}

function buildAgentRow() {
  const row = document.createElement('div');
  row.className = 'row row-agents glass-inset glass-inset-hover';
  const dot = document.createElement('span');
  const name = document.createElement('span');
  name.className = 'row-name';
  const ver = document.createElement('span');
  ver.className = 'row-sub hide-sm';
  const node = document.createElement('span');
  node.className = 'row-sub';
  const chip = document.createElement('span');
  chip.className = 'type-chip';
  const st = document.createElement('span');
  const detail = document.createElement('span');
  detail.className = 'detail-line';
  row.appendChild(dot);
  row.appendChild(name);
  row.appendChild(ver);
  row.appendChild(node);
  row.appendChild(chip);
  row.appendChild(st);
  row.appendChild(detail);
  row._dot = dot;
  row._name = name;
  row._ver = ver;
  row._node = node;
  row._chip = chip;
  row._st = st;
  row._detail = detail;
  return row;
}
function updateAgentRow(row, a) {
  const kind = statusKind('agent', a.status);
  setDot(row._dot, kind, false);
  row._name.textContent = a.name;
  row._ver.textContent = a.version || '—';
  row._node.textContent = a.node;
  row._chip.textContent = a.type;
  setDotText(row._st, kind, a.status);
  row._detail.textContent = a.detail || '';
  row._detail.style.display = a.detail ? '' : 'none';
}

function buildTailnetRow() {
  const row = document.createElement('div');
  row.className = 'row row-tailnet glass-inset glass-inset-hover';
  const dot = document.createElement('span');
  const name = document.createElement('span');
  name.className = 'row-name';
  const ips = document.createElement('span');
  ips.className = 'row-sub hide-sm';
  const seen = document.createElement('span');
  seen.className = 'row-sub';
  row.appendChild(dot);
  row.appendChild(name);
  row.appendChild(ips);
  row.appendChild(seen);
  row._dot = dot;
  row._name = name;
  row._ips = ips;
  row._seen = seen;
  return row;
}
function updateTailnetRow(row, t, now) {
  setDot(row._dot, t.online ? 'ok' : 'dim', false);
  row._name.textContent = t.machine_name;
  row._ips.textContent = (t.ips || []).join(' · ');
  row._seen.textContent = 'seen ' + relTime(t.last_seen, now);
}

/* ---------- 主渲染 ---------- */
let leadName = null;

function render(ov) {
  const now = ov.generated_at;
  renderReadout(ov);

  // 主节点选择：在线优先 → 受管服务多者优先 → 名称稳定序；次节点小卡。
  const nodes = (ov.nodes || []).slice().sort((a, b) => {
    const oa = a.node.status === 'online' ? 0 : 1, ob = b.node.status === 'online' ? 0 : 1;
    if (oa !== ob) return oa - ob;
    const sa = ov.services.filter((s) => s.node === a.node.name).length;
    const sb = ov.services.filter((s) => s.node === b.node.name).length;
    if (sa !== sb) return sb - sa;
    return a.node.name.localeCompare(b.node.name);
  });
  const lead = nodes[0];
  const minis = nodes.slice(1);

  const leadSlot = $('lead-slot');
  if (!lead) {
    leadSlot.replaceChildren();
    leadName = null;
  } else if (lead.node.name !== leadName) {
    leadName = lead.node.name;
    leadSlot.replaceChildren(buildLeadCard());
  }
  if (lead) updateLeadCard(leadSlot.firstChild, lead, now);

  syncList($('minis-slot'), minis, (n) => n.node.name, buildMini, (row, n) => updateMini(row, n, now));

  syncList($('services-list'), ov.services || [], (s) => s.node + '/' + s.name, buildServiceRow, (row, s) => updateServiceRow(row, s));
  $('services-meta').textContent = (ov.services || []).length + ' tracked';

  syncList($('agents-list'), ov.agents || [], (a) => a.node + '/' + a.name, buildAgentRow, (row, a) => updateAgentRow(row, a));
  $('agents-meta').textContent = (ov.agents || []).length + ' found';

  const tn = ov.tailnet;
  if (tn && tn.nodes && tn.nodes.length) {
    syncList($('tailnet-list'), tn.nodes, (t) => t.id, buildTailnetRow, (row, t) => updateTailnetRow(row, t, now));
    $('tailnet-meta').textContent = tn.online + '/' + tn.tracked + ' online';
  } else {
    $('tailnet-list').replaceChildren();
    const e = document.createElement('p');
    e.className = 'empty';
    e.textContent = 'headscale integration disabled or not synced yet';
    $('tailnet-list').appendChild(e);
    $('tailnet-meta').textContent = 'headscale';
  }

  renderFreshness(ov);
}

function renderFreshness(ov) {
  const dot = $('fresh-dot'), text = $('fresh-text');
  const now = ov.generated_at;
  if (ov.freshness) {
    const age = now - ov.freshness;
    dot.className = 'dot ' + (age <= 90 ? 'dot--ok' : 'dot--warn');
    text.textContent = 'data age ' + relTime(ov.freshness, now) + ' · latest metric ' + new Date(ov.freshness * 1000).toLocaleTimeString();
  } else {
    dot.className = 'dot dot--dim';
    text.textContent = 'no metrics in store yet';
  }
  text.classList.remove('fresh-flash');
  void text.offsetWidth; // 重启动画
  text.classList.add('fresh-flash');
}

/* ---------- 轮询 ---------- */
async function tick() {
  try {
    const res = await fetch('/api/panel/overview', { headers: { Accept: 'application/json' } });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const ov = await res.json();
    render(ov);
  } catch (err) {
    const dot = $('fresh-dot'), text = $('fresh-text');
    dot.className = 'dot dot--crit';
    text.textContent = 'fetch failed (' + err.message + ') · retrying in 15s';
  }
}

tick();
setInterval(tick, POLL_MS);
