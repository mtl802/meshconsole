/* MeshConsole read-only panel (SPEC-M1b-b §4 + SPEC-M1b-c §2.3 中台信息架构)
 * 纯原生 JS：15s 轮询 /api/panel/overview，就地 diff 更新（不重建带状态的
 * DOM，数字/状态点走过渡而非跳变）。所有动态文本经 textContent 写入，
 * 不用 innerHTML 拼数据 —— DB 内容视为不可信（DESIGN §7-8）。
 * 动效：单一 easing 签名（--ease）的延伸 —— 载入 stagger 在 CSS，数字 tween /
 * 刷新闪灯在这里且曲线与 CSS 完全同一条；prefers-reduced-motion 时全部直出。
 * 业务口径：异常服务数等由 /api/panel/overview 的 service_issues 直接输出
 * （300s 宽限在 meshview service 层统一，R19-#4），前端不做业务计算。
 * M1b-c：顶栏 = 在线终端/运行任务/服务异常/tailnet 在线；核心区 = 跨终端
 * 任务大卡流（活动计时本地每秒走动）；终端卡带 agent 清单 + last_activity。 */
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
  return '4 120 87';
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
function el(tag, cls) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  return e;
}

/* ---------- 格式化 ---------- */
function relTime(ts, now) {
  if (!ts) return '—';
  const d = Math.max(0, (now - ts) * 1000);
  if (d < 60000) return Math.floor(d / 1000) + 's';
  if (d < 3600000) return Math.floor(d / 60000) + 'm' + String(Math.floor((d % 60000) / 1000)).padStart(2, '0') + 's';
  return Math.floor(d / 3600000) + 'h' + String(Math.floor((d % 3600000) / 60000)).padStart(2, '0') + 'm';
}
// 任务活动计时（本地每秒走动）：s → h:mm:ss / mm:ss
function fmtDur(totalS) {
  const s = Math.max(0, Math.floor(totalS));
  const hh = Math.floor(s / 3600);
  const mm = Math.floor((s % 3600) / 60);
  const ss = s % 60;
  if (hh > 0) return hh + ':' + String(mm).padStart(2, '0') + ':' + String(ss).padStart(2, '0');
  return String(mm).padStart(2, '0') + ':' + String(ss).padStart(2, '0');
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

/* ---------- 列表就地 diff：按键复用行元素，顺序对齐；空态占位受管 ----------
 * data-empty 占位元素克隆缓存到 container._empty：有数据时移除、清零时还原
 * （「全部安静」空态本身是信息，SPEC-M1b-c §2.3，不能显示一次就丢）。 */
function syncList(container, items, keyFn, build, update) {
  if (!container._empty) {
    const proto = container.querySelector(':scope > [data-empty]');
    if (proto) {
      container._empty = proto.cloneNode(true);
      container._empty.removeAttribute('data-empty');
      proto.remove(); // 原型立即移除：受管的是克隆（有数据移除、清零回位），否则空态会同显两份
    }
  }
  if (container._empty && container._empty.parentNode) container._empty.remove();
  container.querySelectorAll(':scope > .empty').forEach((e) => e.remove());
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
  if (!items.length && container._empty) container.appendChild(container._empty);
}

/* ---------- 仪表条（SPEC-M1b-c §2.3 顶栏：在线终端/运行任务/服务异常/tailnet） ---------- */
function renderReadout(ov) {
  // 异常服务数由 overview 直接输出（R19-#4）：300s 宽限/stale 簿记态等口径
  // 全部在 meshview service 层统一，前端只消费名单长度，不做业务计算。
  const issues = (ov.service_issues || []).length;
  const online = ov.nodes.filter((n) => n.node.status === 'online').length;
  // 运行任务仪表只计未过期快照（R27-#3）：stale 快照已停走标注，不冒充运行中。
  const tasks = (ov.running_tasks || []).filter((t) => !t.stale).length;
  const tailnetUp = ov.tailnet ? ov.tailnet.online : 0;

  const gOn = $('g-online'), gTk = $('g-tasks'), gIss = $('g-issues'), gTn = $('g-tailnet');
  tweenNum(gOn, online);
  tweenNum(gTk, tasks);
  tweenNum(gIss, issues);
  tweenNum(gTn, tailnetUp);
  gOn.classList.toggle('is-zero', online === 0);
  gTk.classList.toggle('is-zero', tasks === 0);
  gTk.classList.toggle('is-live', tasks > 0); // 有任务在跑：数字亮品牌绿（「活着」的信息）
  gIss.classList.toggle('has-issue', issues > 0);
  gIss.classList.toggle('is-zero', issues === 0);
  gTn.classList.toggle('is-zero', tailnetUp === 0);
}

/* ---------- 运行中的 agent 任务（核心区大卡） ---------- */
// 活动计时本地走动的行集合：每秒重算 textContent（数据更新非动画，
// reduced-motion 下照常走，只是不做 tween/呼吸）。
const liveRows = new Set();
setInterval(() => {
  for (const row of liveRows) {
    if (!row.isConnected) { liveRows.delete(row); continue; }
    if (row._tick) row._tick();
  }
}, 1000);

function buildTaskCard() {
  const card = el('div', 'task-card glass-inset glass-inset-hover');
  const aura = el('span', 'aura');
  aura.setAttribute('aria-hidden', 'true');
  card.appendChild(aura);

  const top = el('div', 'task-top');
  const dot = el('span', 'dot dot--ok breathing--live');
  const node = el('span', 'task-node');
  const agent = el('span', 'task-agent');
  const cpu = el('span', 'task-cpu');
  const flag = el('span', 'task-flag');
  flag.style.display = 'none';
  top.appendChild(dot);
  top.appendChild(node);
  top.appendChild(agent);
  top.appendChild(cpu);
  top.appendChild(flag);
  card.appendChild(top);

  const cmd = el('div', 'task-cmd');
  card.appendChild(cmd);

  const erow = el('div', 'task-elapsed-row');
  const elapsed = el('span', 'task-elapsed');
  const since = el('span', 'task-since');
  erow.appendChild(elapsed);
  erow.appendChild(since);
  card.appendChild(erow);

  card._dot = dot;
  card._node = node;
  card._agent = agent;
  card._cpu = cpu;
  card._flag = flag;
  card._cmd = cmd;
  card._elapsed = elapsed;
  card._since = since;
  // 活动计时：_base 为最近一次上报的 elapsed_s，_anchor 为收到该值的本地时刻；
  // 每秒 tick = base + (now - anchor)，15s 轮询刷新锚点消除累积漂移。
  card._base = 0;
  card._anchor = Date.now();
  card._tick = () => {
    card._elapsed.textContent = fmtDur(card._base + (Date.now() - card._anchor) / 1000);
  };
  return card;
}

function updateTaskCard(card, t, now, truncNodes) {
  card._node.textContent = t.node;
  card._agent.textContent = t.agent_name;
  card._agent.title = t.node + ' / ' + t.agent_name + ' · pid ' + t.pid;
  card._cpu.textContent = 'cpu ' + fmtPct(t.cpu_pct) + '%';
  card._cpu.title = 'mem ' + fmtPct(t.mem_pct) + '%';
  const cmdText = t.cmd || '(no cmdline)';
  card._cmd.textContent = cmdText;
  card._cmd.title = cmdText;

  // 快照诚实性标注（R27-#3/#4）：stale（节点失联/快照过期）优先于截断标注，
  // 二者皆无则不占位。stale 时计时冻结在快照值、移出每秒走动集合、呼吸点停
  // 止并转 warn——过期快照不是「正在运行」，不冒充活性。
  const stale = !!t.stale;
  if (stale) {
    card._flag.textContent = 'snapshot stale';
    card._flag.title = 'node offline or task snapshot not refreshed — timer frozen, not live';
  } else if (truncNodes && truncNodes.has(t.node)) {
    card._flag.textContent = 'list truncated';
    card._flag.title = "node hit the per-beat task cap — this list is incomplete";
  } else {
    card._flag.textContent = '';
    card._flag.title = '';
  }
  card._flag.style.display = card._flag.textContent ? '' : 'none';
  card._dot.className = stale ? 'dot dot--warn' : 'dot dot--ok breathing--live';

  card._base = t.elapsed_s || 0;
  card._anchor = Date.now();
  if (stale) {
    card._elapsed.textContent = fmtDur(card._base);
    liveRows.delete(card);
  } else {
    card._tick();
    liveRows.add(card);
  }
  card._since.textContent = t.started_at
    ? 'since ' + new Date(t.started_at * 1000).toLocaleTimeString()
    : 'since unknown';
}

/* ---------- 终端卡（每终端一张：状态/角色/sparkline/disk/agent 清单） ---------- */
function buildNodeCard() {
  const card = el('section', 'node-card glass-inset');
  const head = el('div', 'node-head');
  const dot = el('span');
  const name = el('span', 'node-name');
  const role = el('span', 'node-chip');
  const hb = el('span', 'node-hb');
  head.appendChild(dot);
  head.appendChild(name);
  head.appendChild(role);
  head.appendChild(hb);
  card.appendChild(head);
  card._dot = dot;
  card._name = name;
  card._role = role;
  card._hb = hb;

  const sparks = el('div', 'spark-row');
  card._sparks = {};
  for (const key of ['cpu', 'mem']) {
    const block = el('div', 'spark-block glass-inset');
    const headRow = el('div', 'spark-head');
    const lbl = el('span', 'label');
    lbl.textContent = key === 'cpu' ? 'cpu' : 'memory';
    const val = el('span', 'spark-val');
    const valTxt = document.createTextNode('—');
    const valUnit = el('small');
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

  const disk = el('div', 'disk-meter');
  const diskLbl = el('span', 'label');
  diskLbl.textContent = 'disk';
  const diskVal = el('span', 'disk-val');
  const track = el('div', 'disk-track');
  const fill = el('i');
  track.appendChild(fill);
  disk.appendChild(diskLbl);
  disk.appendChild(diskVal);
  disk.appendChild(track);
  card.appendChild(disk);
  card._diskVal = diskVal;
  card._diskTrack = track;
  card._diskFill = fill;

  const agents = el('div', 'node-agents');
  const agLbl = el('span', 'label');
  agLbl.textContent = 'agents';
  const agList = el('div');
  agList.style.display = 'flex';
  agList.style.flexDirection = 'column';
  agList.style.gap = '5px';
  agents.appendChild(agLbl);
  agents.appendChild(agList);
  card.appendChild(agents);
  card._agentList = agList;
  return card;
}

function updateNodeCard(card, ncard, agentsOfNode, now) {
  const n = ncard.node;
  setDot(card._dot, statusKind('node', n.status), n.status === 'offline');
  card._name.textContent = n.name;
  card._role.textContent = n.role || 'node';
  card._role.style.display = n.role ? '' : 'none';
  card._hb.textContent = 'hb ' + relTime(n.last_seen, now);
  card._hb.title = n.last_seen ? 'last_seen ' + new Date(n.last_seen * 1000).toLocaleTimeString() : '';

  const cpu = ncard.latest ? ncard.latest.cpu_pct : null;
  const m = ncard.latest;
  const memPct = m && m.mem_used != null && m.mem_total ? (m.mem_used / m.mem_total) * 100 : null;
  tweenNum(card._sparks.cpu.valTxt, cpu, (v) => v.toFixed(1));
  tweenNum(card._sparks.mem.valTxt, memPct, (v) => v.toFixed(1));
  sparkPaint(card._sparks.cpu.sp, (ncard.spark || []).map((p) => ({ v: p.cpu_pct })));
  sparkPaint(card._sparks.mem.sp, (ncard.spark || []).map((p) => ({ v: p.mem_pct })));

  const du = ncard.latest ? ncard.latest.disk_used : null;
  const dt = ncard.latest ? ncard.latest.disk_total : null;
  const pct = du != null && dt ? (du / dt) * 100 : null;
  card._diskVal.textContent = pct == null ? '—' : fmtBytes(du) + ' / ' + fmtBytes(dt);
  card._diskFill.style.width = (pct == null ? 0 : Math.min(100, pct)).toFixed(1) + '%';
  card._diskTrack.classList.toggle('warn', pct != null && pct >= 85);

  syncList(card._agentList, agentsOfNode, (a) => a.name, buildAgentLine, (row, a) => updateAgentLine(row, a, now));
}

function buildAgentLine() {
  const row = el('div', 'agent-line glass-inset');
  const dot = el('span');
  const name = el('span', 'al-name');
  const sub = el('span', 'al-sub');
  const act = el('span', 'al-act');
  row.appendChild(dot);
  row.appendChild(name);
  row.appendChild(sub);
  row.appendChild(act);
  row._dot = dot;
  row._name = name;
  row._sub = sub;
  row._act = act;
  return row;
}
function updateAgentLine(row, a, now) {
  setDot(row._dot, statusKind('agent', a.status), false);
  row._name.textContent = a.name;
  row._sub.textContent = a.version || a.type;
  row._sub.title = a.path || '';
  // last_activity：会话目录最近 mtime（只 stat 不读内容）；文件数为辅证。
  if (a.last_activity) {
    row._act.textContent = 'act ' + relTime(a.last_activity, now);
    row._act.title = a.session_files != null ? a.session_files + ' session files (stat only)' : '';
  } else {
    row._act.textContent = 'no sessions';
    row._act.title = '';
  }
}

/* ---------- 服务 / tailnet 行：结构只在 build 建一次，update 原地改值 ---------- */
function buildServiceRow() {
  const row = el('div', 'row row-services glass-inset glass-inset-hover');
  const dot = el('span');
  const name = el('span', 'row-name');
  const sub = el('span', 'row-sub hide-sm');
  const chip = el('span', 'type-chip');
  const st = el('span');
  const detail = el('span', 'detail-line');
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

function buildTailnetRow() {
  const row = el('div', 'row row-tailnet glass-inset glass-inset-hover');
  const dot = el('span');
  const name = el('span', 'row-name');
  const ips = el('span', 'row-sub hide-sm');
  const seen = el('span', 'row-sub');
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
function render(ov) {
  const now = ov.generated_at;
  renderReadout(ov);

  // 核心区：跨终端聚合任务卡流（节点名稳定序；前端只渲染，业务口径在服务端）。
  const tasks = (ov.running_tasks || []).slice().sort((a, b) =>
    a.node === b.node
      ? (a.agent_name === b.agent_name ? a.pid - b.pid : a.agent_name.localeCompare(b.agent_name))
      : a.node.localeCompare(b.node));
  // 清单截断标注（R27-#4）：按节点名取 tasks_truncated，命中节点的任务卡与
  // meta 行标注「list truncated」（清单不完整可见）。
  const truncNodes = new Set((ov.nodes || []).filter((n) => n.node.tasks_truncated).map((n) => n.node.name));
  syncList($('tasks-grid'), tasks, (t) => t.node + '/' + t.pid + '/' + t.agent_name,
    buildTaskCard, (row, t) => updateTaskCard(row, t, now, truncNodes));
  const staleCount = tasks.filter((t) => t.stale).length;
  $('tasks-meta').textContent = tasks.length
    ? (tasks.length - staleCount) + ' running'
      + (staleCount ? ' · ' + staleCount + ' stale' : '')
      + (truncNodes.size ? ' · list truncated' : '')
    : 'all quiet';

  // 终端区：每终端一张卡；agent 清单按节点过滤自带 last_activity。
  const agentsByNode = {};
  for (const a of ov.agents || []) {
    (agentsByNode[a.node] || (agentsByNode[a.node] = [])).push(a);
  }
  const nodes = (ov.nodes || []).slice().sort((a, b) => {
    const oa = a.node.status === 'online' ? 0 : 1, ob = b.node.status === 'online' ? 0 : 1;
    if (oa !== ob) return oa - ob;
    return a.node.name.localeCompare(b.node.name);
  });
  syncList($('nodes-grid'), nodes, (n) => n.node.name, buildNodeCard,
    (row, n) => updateNodeCard(row, n, agentsByNode[n.node.name] || [], now));
  $('nodes-meta').textContent = nodes.length + ' nodes';

  syncList($('services-list'), ov.services || [], (s) => s.node + '/' + s.name, buildServiceRow, (row, s) => updateServiceRow(row, s));
  $('services-meta').textContent = (ov.services || []).length + ' tracked';

  const tn = ov.tailnet;
  if (tn && tn.nodes && tn.nodes.length) {
    syncList($('tailnet-list'), tn.nodes, (t) => t.id, buildTailnetRow, (row, t) => updateTailnetRow(row, t, now));
    $('tailnet-meta').textContent = tn.online + '/' + tn.tracked + ' online';
  } else {
    syncList($('tailnet-list'), [], (t) => t.id, buildTailnetRow, () => {});
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
// 最近一次成功 overview 与到达时刻（R29-#2）：拉取持续失败、数据超过新鲜度
// 期限后，旧任务卡不得继续冒充运行中。期限取服务端 taskStaleAfter 同窗口
// 90s（R27-#3 口径，约 6 拍轮询）——窗口内保留旧数据原样，过线即冻结。
const FETCH_STALE_MS = 90 * 1000;
let lastGoodOv = null;
let lastGoodAt = 0;

// renderStaleSnapshot 把上一次成功 overview 以「全部任务 stale」的形态重渲：
// 冻结完全复用 render/updateTaskCard 既有语义（计时移出 liveRows 停走、呼吸
// 点停转、snapshot stale 标注、运行仪表与 meta 只计非 stale），不另起炉灶；
// 拉取恢复后下一拍正常 render 即自动解冻。
function renderStaleSnapshot() {
  if (!lastGoodOv) return;
  render(Object.assign({}, lastGoodOv, {
    running_tasks: (lastGoodOv.running_tasks || []).map((t) => Object.assign({}, t, { stale: true })),
  }));
}

async function tick() {
  try {
    const res = await fetch('/api/panel/overview', { headers: { Accept: 'application/json' } });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const ov = await res.json();
    lastGoodOv = ov;
    lastGoodAt = Date.now();
    render(ov);
  } catch (err) {
    // 刚失败一两拍时旧数据仍在新鲜度窗口内，保持原样；超线才冻结。
    if (lastGoodOv && Date.now() - lastGoodAt > FETCH_STALE_MS) renderStaleSnapshot();
    const dot = $('fresh-dot'), text = $('fresh-text');
    dot.className = 'dot dot--crit';
    text.textContent = 'fetch failed (' + err.message + ') · retrying in 15s';
  }
}

tick();
setInterval(tick, POLL_MS);
