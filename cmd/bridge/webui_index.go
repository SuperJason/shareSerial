package main

// indexHTML is the single-page Web UI served at /. Dark theme, auto-refresh
// status, config editing and com0com pair management.
const indexHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Serial Bridge Hub</title>
<style>
:root{--bg:#0f1420;--card:#1a2130;--line:#2a3348;--fg:#dce4f0;--dim:#8b97ad;--ok:#3ddc84;--bad:#ff5c5c;--acc:#4c9aff}
*{box-sizing:border-box;margin:0;padding:0}
body{background:var(--bg);color:var(--fg);font:14px/1.6 "Segoe UI",system-ui,sans-serif;padding:20px}
h1{font-size:20px;margin-bottom:4px}
.sub{color:var(--dim);font-size:12px;margin-bottom:20px}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:16px;margin-bottom:16px}
.card h2{font-size:14px;margin-bottom:12px;color:var(--acc)}
.badge{display:inline-block;padding:2px 10px;border-radius:20px;font-size:12px;font-weight:600}
.badge.run{background:rgba(61,220,132,.15);color:var(--ok)}
.badge.stop{background:rgba(255,92,92,.15);color:var(--bad)}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(170px,1fr));gap:10px}
.kv{background:#141b29;border:1px solid var(--line);border-radius:8px;padding:8px 12px}
.kv .k{color:var(--dim);font-size:11px}
.kv .v{font-size:14px;font-family:Consolas,monospace}
.btns{display:flex;gap:10px;margin-top:12px}
button{background:var(--acc);border:none;color:#fff;padding:8px 18px;border-radius:8px;font-size:13px;cursor:pointer}
button.sec{background:#2a3348}
button.danger{background:#b34545}
button:disabled{opacity:.4;cursor:not-allowed}
label{display:block;font-size:12px;color:var(--dim);margin:8px 0 4px}
input,select{width:100%;background:#141b29;border:1px solid var(--line);color:var(--fg);border-radius:8px;padding:7px 10px;font-size:13px;font-family:Consolas,monospace}
.row{display:grid;grid-template-columns:repeat(auto-fill,minmax(220px,1fr));gap:0 14px}
table{width:100%;border-collapse:collapse;font-size:13px}
th,td{text-align:left;padding:7px 10px;border-bottom:1px solid var(--line)}
th{color:var(--dim);font-weight:500;font-size:12px}
pre{background:#0d1220;border:1px solid var(--line);border-radius:8px;padding:10px;font-size:12px;max-height:280px;overflow:auto;white-space:pre-wrap}
.msg{font-size:12px;margin-top:8px;min-height:18px}
.msg.ok{color:var(--ok)}.msg.bad{color:var(--bad)}
</style>
</head>
<body>
<h1>🔌 Serial Bridge Hub</h1>
<div class="sub">COM22 → hub4com → com0com → TCP · 远程 AI + 本地监视 · 托管模式</div>

<div class="card">
  <h2>运行状态 <span id="badge" class="badge stop">…</span></h2>
  <div class="grid" id="statusGrid"></div>
  <div class="btns">
    <button id="btnStart" onclick="act('start')">启动</button>
    <button id="btnStop" class="danger" onclick="act('stop')">停止</button>
    <button id="btnRestart" class="sec" onclick="act('restart')">重启</button>
    <button class="sec" onclick="refresh(true)">刷新</button>
  </div>
  <div id="actMsg" class="msg"></div>
</div>

<div class="card">
  <h2>配置 (bridge / hub4com)</h2>
  <div class="row">
    <div><label>串口 (bridge, com0com 对端)</label><input id="cfg_com_port"></div>
    <div><label>波特率</label><input id="cfg_baud_rate" type="number"></div>
    <div><label>TCP 监听端口</label><input id="cfg_target_port" type="number"></div>
    <div><label>TCP 目标 IP (留空=服务端)</label><input id="cfg_target_ip"></div>
    <div><label>物理串口 (hub4com)</label><input id="cfg_hub_physical_port"></div>
    <div><label>hub4com slave 口</label><input id="cfg_hub_slave_port"></div>
    <div><label>额外 slave 口 (逗号分隔)</label><input id="cfg_hub_extra_slaves"></div>
    <div><label>hub4com.exe 路径</label><input id="cfg_hub4com_path"></div>
    <div><label>日志文件</label><input id="cfg_log_file"></div>
    <div><label>数据日志</label><input id="cfg_data_log_file"></div>
    <div><label>hub4com 日志</label><input id="cfg_hub4com_log_file"></div>
    <div><label>Web UI 端口 (重启后生效)</label><input id="cfg_web_port" type="number"></div>
    <div><label>FTDI VBUS 控制 (1=开, 0=关)</label><input id="cfg_ftdi_enabled" type="number" min="0" max="1"></div>
    <div><label>CBUS 引脚 (0-3)</label><input id="cfg_ftdi_cbus_pin" type="number" min="0" max="3"></div>
    <div><label>VBUS 低电平有效 (1=是)</label><input id="cfg_ftdi_active_low" type="number" min="0" max="1"></div>
    <div><label>FTDI 序列号 (留空=自动检测)</label><input id="cfg_ftdi_serial"></div>
  </div>
  <div class="btns">
    <button onclick="saveCfg(true)">保存并重启</button>
    <button class="sec" onclick="saveCfg(false)">仅保存</button>
  </div>
  <div id="cfgMsg" class="msg"></div>
</div>

<div class="card">
  <h2>com0com 口对管理 <span class="sub" style="font-weight:400">(改名/建对/删除需 UAC 确认)</span></h2>
  <table><thead><tr><th>Pair</th><th>Port</th><th>Peer</th></tr></thead>
  <tbody id="com0comBody"><tr><td colspan="3" style="color:var(--dim)">加载中…</td></tr></tbody></table>
  <div class="row" style="margin-top:14px">
    <div><label>改名: Pair 名 (如 CNCA0)</label><input id="c0c_pair" placeholder="CNCA0"></div>
    <div><label>新端口号</label><input id="c0c_port" placeholder="COM161"></div>
  </div>
  <div class="btns"><button class="sec" onclick="c0cRename()">改名</button></div>
  <div class="row" style="margin-top:14px">
    <div><label>新建口对: 端口 A</label><input id="c0c_a" placeholder="COM170"></div>
    <div><label>端口 B</label><input id="c0c_b" placeholder="COM171"></div>
  </div>
  <div class="btns"><button class="sec" onclick="c0cCreate()">新建口对</button></div>
  <div id="c0cMsg" class="msg"></div>
</div>

<div class="card" id="vbusCard">
  <h2>FTDI CBUS / DUT VBUS 控制 <span class="sub" style="font-weight:400">(模拟 DUT 的 USB 插拔; 每块板卡独立控制)</span></h2>
  <table><thead><tr><th>板卡 (COM)</th><th>序列号</th><th>引脚</th><th>VBUS 状态</th><th style="width:210px">操作</th></tr></thead>
  <tbody id="vbusBody"><tr><td colspan="5" style="color:var(--dim)">加载中…</td></tr></tbody></table>
  <div id="vbusMsg" class="msg"></div>
</div>

<div class="card">
  <h2>日志 <button class="sec" style="padding:2px 10px;font-size:12px" onclick="loadLogs()">刷新</button></h2>
  <pre id="logBox">加载中…</pre>
</div>

<script>
let S=null;
async function j(url,opt){const r=await fetch(url,opt);return r.json()}
async function refresh(force){
  try{
    const d=await j('/api/status');
    if(!d.ok)throw new Error(d.error||'bad');
    S=d.data;
    const b=document.getElementById('badge');
    b.textContent=S.running?'运行中':'已停止';
    b.className='badge '+(S.running?'run':'stop');
    document.getElementById('btnStart').disabled=S.running;
    document.getElementById('btnStop').disabled=!S.running;
    const g=document.getElementById('statusGrid');
    g.innerHTML='';
    const items=[
      ['串口',S.config.com_port||'—'],['波特率',S.config.baud_rate||'—'],
      ['TCP 端口',S.config.target_port||'—'],['客户端',S.clients],
      ['物理口',S.config.hub_physical_port||'—'],
      ['slave',[S.config.hub_slave_port,...(S.config.hub_extra_slaves||[])].join(', ')||'—'],
      ['Web UI',location.host],['启动时间',S.startedAt||'—'],
      ['LAN IP',(S.lanIPs||[]).join(', ')||'—'],
      ['DUT VBUS',vbusSummary(S.ftdi)]
    ];
    for(const[k,v]of items){const d=document.createElement('div');d.className='kv';
      d.innerHTML='<div class="k">'+k+'</div><div class="v">'+v+'</div>';g.appendChild(d)}
    vbusCard(S.ftdi);
    if(S.config&&!force===false){fillCfg(S.config)}
  }catch(e){document.getElementById('badge').textContent='状态获取失败';document.getElementById('badge').className='badge stop'}
  loadLogs();
}
function fillCfg(c){
  const map={cfg_com_port:c.com_port,cfg_baud_rate:c.baud_rate,cfg_target_port:c.target_port,
    cfg_target_ip:c.target_ip||'',cfg_hub_physical_port:c.hub_physical_port||'',
    cfg_hub_slave_port:c.hub_slave_port||'',cfg_hub_extra_slaves:(c.hub_extra_slaves||[]).join(','),
    cfg_hub4com_path:c.hub4com_path||'',cfg_log_file:c.log_file||'',cfg_data_log_file:c.data_log_file||'',
    cfg_hub4com_log_file:c.hub4com_log_file||'',cfg_web_port:c.web_port||18080,
    cfg_ftdi_enabled:c.ftdi_enabled===false?0:1,cfg_ftdi_cbus_pin:c.ftdi_cbus_pin||0,
    cfg_ftdi_active_low:c.ftdi_active_low?1:0,cfg_ftdi_serial:c.ftdi_serial||''};
  for(const[id,v]of Object.entries(map)){const el=document.getElementById(id);if(el&&el.value==='')el.value=v}
}
function collectCfg(){
  return {com_port:document.getElementById('cfg_com_port').value.trim(),
    baud_rate:+document.getElementById('cfg_baud_rate').value,
    target_port:+document.getElementById('cfg_target_port').value,
    target_ip:document.getElementById('cfg_target_ip').value.trim(),
    hub_physical_port:document.getElementById('cfg_hub_physical_port').value.trim(),
    hub_slave_port:document.getElementById('cfg_hub_slave_port').value.trim(),
    hub_extra_slaves:document.getElementById('cfg_hub_extra_slaves').value.split(',').map(s=>s.trim()).filter(Boolean),
    hub4com_path:document.getElementById('cfg_hub4com_path').value.trim(),
    log_file:document.getElementById('cfg_log_file').value.trim(),
    data_log_file:document.getElementById('cfg_data_log_file').value.trim(),
    hub4com_log_file:document.getElementById('cfg_hub4com_log_file').value.trim(),
    web_port:+document.getElementById('cfg_web_port').value||18080,
    ftdi_enabled:document.getElementById('cfg_ftdi_enabled').value!=='0',
    ftdi_cbus_pin:+document.getElementById('cfg_ftdi_cbus_pin').value||0,
    ftdi_active_low:document.getElementById('cfg_ftdi_active_low').value==='1',
    ftdi_serial:document.getElementById('cfg_ftdi_serial').value.trim()};
}
async function act(a){
  const m=document.getElementById('actMsg');
  try{const d=await j('/api/'+a,{method:'POST'});m.className='msg '+(d.ok?'ok':'bad');
    m.textContent=d.ok?(a==='start'?'已启动':a==='stop'?'已停止':'已重启'):(d.error||'操作失败')}
  catch(e){m.className='msg bad';m.textContent='请求失败: '+e}
  setTimeout(()=>refresh(true),800);
}
async function saveCfg(restart){
  const m=document.getElementById('cfgMsg');
  try{
    let d=await j('/api/config',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(collectCfg())});
    if(!d.ok){m.className='msg bad';m.textContent='保存失败: '+(d.error||'');return}
    m.className='msg ok';m.textContent='配置已保存'+(restart?', 正在重启…':'');
    if(restart){d=await j('/api/restart',{method:'POST'});
      m.textContent=d.ok?'配置已保存并重启':'保存成功但重启失败: '+(d.error||'')}
  }catch(e){m.className='msg bad';m.textContent='请求失败: '+e}
  setTimeout(()=>refresh(true),1000);
}
async function c0c(){
  try{const d=await j('/api/com0com');const t=document.getElementById('com0comBody');
    if(!d.ok){t.innerHTML='<tr><td colspan="3" style="color:var(--bad)">'+d.error+'</td></tr>';return}
    const pairs=d.data.pairs||[];
    t.innerHTML=pairs.length?pairs.map(p=>'<tr><td>'+p.name+'</td><td>'+p.port+'</td><td>'+(p.peer||'—')+'</td></tr>').join('')
      :'<tr><td colspan="3" style="color:var(--dim)">未检测到 com0com 口对</td></tr>';
  }catch(e){document.getElementById('com0comBody').innerHTML='<tr><td colspan="3" style="color:var(--bad)">加载失败</td></tr>'}
}
async function c0cRename(){
  const m=document.getElementById('c0cMsg');
  try{const d=await j('/api/com0com/rename',{method:'POST',headers:{'Content-Type':'application/json'},
    body:JSON.stringify({pair:document.getElementById('c0c_pair').value.trim(),port:document.getElementById('c0c_port').value.trim()})});
    m.className='msg '+(d.ok?'ok':'bad');m.textContent=d.ok?(d.message||'已提交'):(d.error||'失败');if(d.ok)setTimeout(c0c,2500)}
  catch(e){m.className='msg bad';m.textContent='请求失败: '+e}
}
async function c0cCreate(){
  const m=document.getElementById('c0cMsg');
  try{const d=await j('/api/com0com/create',{method:'POST',headers:{'Content-Type':'application/json'},
    body:JSON.stringify({portA:document.getElementById('c0c_a').value.trim(),portB:document.getElementById('c0c_b').value.trim()})});
    m.className='msg '+(d.ok?'ok':'bad');m.textContent=d.ok?(d.message||'已提交'):(d.error||'失败');if(d.ok)setTimeout(c0c,2500)}
  catch(e){m.className='msg bad';m.textContent='请求失败: '+e}
}
function vbusSummary(f){
  if(!f)return '—';
  const boards=f.boards;
  if(!Array.isArray(boards)||boards.length===0)return '未检测到板卡';
  const on=boards.filter(b=>b.vbusKnown&&b.vbusOn).length;
  const off=boards.filter(b=>b.vbusKnown&&!b.vbusOn).length;
  return boards.length+' 板卡 · '+on+' ON / '+off+' OFF / '+(boards.length-on-off)+' 未知';
}
function vbusCard(f){
  const t=document.getElementById('vbusBody');
  if(!t)return;
  const m=document.getElementById('vbusMsg');
  const boards=f&&f.boards;
  if(!f){
    t.innerHTML='<tr><td colspan="5" style="color:var(--dim)">未启用: 在配置中设置 ftdi_enabled=true 后重启</td></tr>';
    m.className='msg';m.textContent='';
    return;
  }
  if(!Array.isArray(boards)||boards.length===0){
    t.innerHTML='<tr><td colspan="5" style="color:var(--bad)">未检测到板卡: '+(f.detectError||'请检查 USB 连接')+'</td></tr>';
    return;
  }
  t.innerHTML=boards.map((b,i)=>{
    const st=b.vbusKnown?(b.vbusOn?'<span style="color:var(--ok)">已插入</span>':'<span style="color:var(--bad)">已拔出</span>'):'未知';
    const onD=b.vbusKnown&&b.vbusOn?' disabled':'';
    const offD=b.vbusKnown&&!b.vbusOn?' disabled':'';
    return '<tr><td>'+(b.comPort||('板卡'+(i+1)))+'</td><td>'+b.serial+'</td><td>CBUS'+b.pin+(b.activeLow?' (低有效)':'')+'</td><td>'+st+'</td>'
      +'<td><button style="padding:4px 12px" onclick="vbus(true,\''+b.serial+'\')"'+onD+'>插入</button> '
      +'<button class="sec" style="padding:4px 12px" onclick="vbus(false,\''+b.serial+'\')"'+offD+'>拔出</button></td></tr>';
  }).join('');
}
async function vbus(on,serial){
  const m=document.getElementById('vbusMsg');
  try{const d=await j('/api/vbus',{method:'POST',headers:{'Content-Type':'application/json'},
    body:JSON.stringify({on:on,serial:serial||''})});
    m.className='msg '+(d.ok?'ok':'bad');
    m.textContent=d.ok?((serial||'板卡')+' 已'+(on?'插入':'拔出')):(d.error||'操作失败')}
  catch(e){m.className='msg bad';m.textContent='请求失败: '+e}
  setTimeout(()=>refresh(true),400);
}
async function loadLogs(){
  try{const d=await j('/api/logs?lines=150');document.getElementById('logBox').textContent=d.data||'(空)'}
  catch(e){document.getElementById('logBox').textContent='日志加载失败'}
}
refresh(true);c0c();setInterval(()=>{if(!document.hidden)refresh(false)},3000);
</script>
</body>
</html>`
