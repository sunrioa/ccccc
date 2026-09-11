'use strict';
const $=s=>document.querySelector(s);
const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
let state={devices:[],snapshots:[],jobs:[]},tab='snapshots',busy=false,logged=false;
const names={codex:'Codex',claude:'Claude Code'};
const kinds={export:'创建快照',preview:'恢复预览',restore:'恢复会话',rollback:'撤销恢复',scan:'扫描项目'};
const statuses={queued:'等待设备',running:'执行中',done:'已完成',failed:'失败',cancelled:'已取消'};
const actions={import:'新增',update:'追加更新',conflict:'冲突','skip-identical':'内容相同','skip-ahead':'本机较新','skip-non-session':'跳过'};
const bytes=n=>{if(!n)return '0 B';const u=['B','KiB','MiB','GiB'];const k=Math.min(3,Math.floor(Math.log(n)/Math.log(1024)));return (n/1024**k).toFixed(k?1:0)+' '+u[k]};
const date=s=>s&&new Date(s).getFullYear()>2000?new Date(s).toLocaleString('zh-CN',{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit'}):'尚未连接';
const online=d=>Date.now()-new Date(d.last_seen).getTime()<50000;
const shortPath=p=>String(p||'').split(/[\\/]/).filter(Boolean).pop()||'项目';
const currentClient=d=>{const m=/^(\d+)\.(\d+)/.exec(d?.version||'');return !!m&&(Number(m[1])>0||Number(m[2])>=3)};
const deviceName=id=>state.devices.find(d=>d.id===id)?.name||'手动导入';
async function api(path,options={}){
 const r=await fetch('/api'+path,{...options,headers:options.body instanceof File?{}:{'Content-Type':'application/json',...options.headers}});
 const b=await r.json();if(!r.ok){if(r.status===401)showLogin();throw Error(b.error||'请求失败')};return b;
}
function toast(s){$('#toast').textContent=s;$('#toast').hidden=false;setTimeout(()=>$('#toast').hidden=true,5000)}
function showLogin(){logged=false;$('#login').hidden=false;$('#app').hidden=true}
function modal(html){$('#modal-body').innerHTML=html;$('#modal').showModal()}
function closeModal(){$('#modal').close()}
async function guarded(fn){if(busy)return;busy=true;try{await fn()}catch(e){toast(e.message)}finally{busy=false}}
function setTab(t){tab=t;for(const b of document.querySelectorAll('[data-tab]'))b.classList.toggle('active',b.dataset.tab===t);for(const el of document.querySelectorAll('.tab'))el.hidden=el.id!=='tab-'+t;$('#page-title').textContent={snapshots:'快照库',jobs:'传输与恢复',devices:'我的设备'}[t];$('#page-kicker').textContent={snapshots:'SNAPSHOT LIBRARY',jobs:'TRANSFER HISTORY',devices:'CONNECTED DEVICES'}[t]}
async function refresh(){try{state=await api('/state');logged=true;$('#login').hidden=true;$('#app').hidden=false;render();$('#connection').textContent='中转站已连接';$('#updated-at').textContent='更新于 '+new Date().toLocaleTimeString('zh-CN');$('#banner').hidden=true}catch(e){if(logged){$('#banner').hidden=false;$('#banner').textContent='连接暂时中断：'+e.message}}}
function render(){
 $('#online-count').innerHTML=state.devices.filter(online).length+' <small>/ '+state.devices.length+'</small>';
 $('#snapshot-count').textContent=state.snapshots.length;$('#nav-count').textContent=state.snapshots.length;
 $('#stored-size').textContent=bytes(state.snapshots.reduce((s,b)=>s+b.size,0));
 $('#running-count').textContent=state.jobs.filter(j=>['queued','running'].includes(j.status)).length;
 renderSnapshots();renderDevices();renderJobs();
}
function renderSnapshots(){
 const filter=$('#filter').value,search=$('#search').value.toLowerCase();
 const list=state.snapshots.filter(b=>(filter==='all'||b.provider===filter)&&`${b.project} ${b.source_path} ${deviceName(b.device_id)}`.toLowerCase().includes(search));
 if(!list.length){$('#snapshot-list').innerHTML=`<div class="empty"><div class="empty-symbol">▤</div><h3>${state.snapshots.length?'没有匹配的快照':'还没有快照'}</h3><p>${state.devices.length?'选择已连接的电脑，关闭对应 Agent，然后创建第一份压缩快照。':'先连接 Mac 或 Windows，再把会话上传到这里。'}</p><button class="primary" data-action="${state.devices.length?'export':'device'}">${state.devices.length?'创建快照':'添加第一台设备'}</button></div>`;return}
 $('#snapshot-list').innerHTML=`<table class="snapshot-table"><thead><tr><th>项目 / 工具</th><th>来源设备</th><th>会话与体积</th><th>创建时间</th><th>操作</th></tr></thead><tbody>${list.map(b=>`<tr><td><span class="badge ${esc(b.provider)}">${names[b.provider]}</span><p class="project-name">${esc(shortPath(b.source_path))}</p><div class="subtext" title="${esc(b.source_path)}">${esc(b.source_path)}</div></td><td>${esc(deviceName(b.device_id))}<div class="subtext">${esc(b.source_os)}</div></td><td>${b.sessions} 个记录 · ${bytes(b.size)}<div class="subtext">原始 ${bytes(b.raw_size)}${b.extras?' · '+b.extras+' 个关联文件':''}</div></td><td>${date(b.created)}<div class="subtext">${b.temporary?"临时中转 · "+date(b.expires)+" 到期":"长期保留"}</div></td><td><div class="row-actions"><button class="text-button" data-action="preview" data-id="${b.id}">恢复到设备</button><a href="/api/snapshots/${b.id}">下载</a><button class="text-button danger" data-action="delete-snapshot" data-id="${b.id}">删除</button></div></td></tr>`).join('')}</tbody></table>`;
}
function renderDevices(){
 $('#device-list').innerHTML=state.devices.length?state.devices.map(d=>`<article class="device-card"><header><div><h3>${esc(d.name)}</h3><p class="muted">${esc(d.os||'等待连接')} · 客户端 ${esc(d.version||'未知')}</p></div><span class="badge ${online(d)?'ok':''}">${online(d)?'在线':'离线'}</span></header>${!/^0\.([4-9]|[1-9][0-9]+)\./.test(d.version||'')?'<div class="notice">0.4 客户端支持自动 SSH 连接；升级时保留 client.json 与数据目录，并复制新版启动和设置脚本。</div>':''}<p class="muted">最后连接 ${date(d.last_seen)}</p>${d.import_root?`<p class="muted">新项目导入位置：<code>${esc(d.import_root)}</code></p><button class="text-button" data-action="scan" data-id="${d.id}">立即刷新项目与工具状态</button>`:''}<ul>${(d.projects||[]).map(p=>{const items=(d.inventory||[]).filter(i=>i.project===p.key&&i.count>0);return `<li><strong>${esc(shortPath(p.path))}</strong> ${p.missing?'<span class="badge warn">代码目录已不存在</span>':''}<br><code>${esc(p.path)}</code>${items.map(i=>`<div class="project-transfer"><span>${names[i.provider]} · ${i.count} 个记录${i.bytes?' · '+bytes(i.bytes):''}${i.largest?' · 最大单文件 '+bytes(i.largest):''}</span><button class="text-button" data-action="export-project" data-device="${d.id}" data-project="${esc(p.key)}" data-provider="${i.provider}">创建快照 →</button>${i.agent_status==='running'?`<small class="error">${names[i.provider]} 正在运行，导出前请完全退出。</small>`:''}${i.error?`<small class="error">${esc(i.error)}</small>`:''}</div>`).join('')}${!items.length?'<p class="muted">暂未发现会话</p>':''}</li>`}).join('')||'<li>连接后自动扫描项目，无需手填路径。</li>'}</ul>${(d.inventory||[]).filter(i=>!i.project&&i.error).map(i=>`<p class="error">${esc(i.error)}</p>`).join('')}</article>`).join(''):`<div class="empty"><h3>先连接两台电脑</h3><p>每台电脑使用各自的 client.json；完成一次 SSH 设置后，启动客户端即可自动连接。</p><button class="primary" data-action="device">添加设备</button></div>`;
}
function progressHTML(j){
 const p=j.progress;if(j.status!=='running'||!p)return '';
 const transfer=['upload','download'].includes(p.stage);
 return `<div class="job-progress"><p>${esc(p.message)}${p.total>0?' · '+Math.floor(p.done/p.total*100)+'%':''}</p>${p.total>0?`<progress max="${p.total}" value="${p.done}"></progress><small>${transfer?bytes(p.done)+' / '+bytes(p.total):p.done+' / '+p.total+' 个文件'}</small>`:'<p class="muted">正在处理，请保持客户端与连接开启。</p>'}</div>`;
}
function renderJobs(){
 $('#job-list').innerHTML=state.jobs.length?state.jobs.map(j=>{
 const result=j.result||{};
 const proj=state.devices.find(d=>d.id===j.device_id)?.projects?.find(p=>p.key===j.project);
 const sid=result.snapshot_id, canContinue=j.kind==='export'&&j.status==='done'&&state.snapshots.some(b=>b.id===sid);
 const buttons=[];
 if(canContinue)buttons.push(`<button class="primary" data-action="preview" data-id="${sid}">下一步：恢复到另一台电脑 →</button>`);
 if(j.kind==='preview'&&j.status==='done')buttons.push(`<button class="primary" data-action="plan" data-id="${j.id}">查看预览并恢复</button>`);
 if(j.status==='failed'&&['export','preview','scan'].includes(j.kind))buttons.push(`<button class="secondary" data-action="retry" data-id="${j.id}">重试，无需重新选择</button>`);
 if(j.kind==='restore'&&['done','failed'].includes(j.status))buttons.push(`<button class="text-button" data-action="rollback" data-id="${j.id}">撤销本次恢复</button>`);
 if(j.status==='queued')buttons.push(`<button class="text-button danger" data-action="cancel" data-id="${j.id}">取消排队</button>`);
 buttons.push(`<button class="text-button" data-action="detail" data-id="${j.id}">详情</button>`);
 return `<article class="job-card"><div class="job-head"><h3>${kinds[j.kind]} <span class="muted">/ ${esc(proj?shortPath(proj.path):j.project||'设备')}</span></h3><span class="badge ${j.status==='failed'?'error':j.status==='done'?'ok':'warn'}">${statuses[j.status]}</span></div><div class="job-meta">${esc(deviceName(j.device_id))} · ${names[j.provider]||''} · ${date(j.created)}</div>${progressHTML(j)}${j.error?`<p class="error">${esc(j.error)}</p>`:''}${result.message?`<p class="muted">${esc(result.message)}</p>`:''}${result.discovery_warning?`<p class="error">${esc(result.discovery_warning)}</p>`:''}${j.kind==='preview'&&j.status==='done'?`<p class="muted">${result.changes?.filter(c=>['import','update'].includes(c.action)).length||0} 个文件待写入 · ${result.can_apply?'预览通过，等待你确认':'存在冲突，未写入会话'}</p>`:''}<div class="row-actions">${buttons.join('')}</div></article>`;
 }).join(''):`<div class="empty"><h3>还没有任务</h3><p>① 来源电脑创建快照 → ② 上传后选择目标 → ③ 预览并确认恢复</p></div>`;
}
function deviceOptions(){return state.devices.map(d=>`<option value="${d.id}">${esc(d.name)} · ${online(d)?'在线':'离线，任务将排队'}</option>`).join('')}
function fillProjects(select,preferred){const d=state.devices.find(x=>x.id===select.value);$('#job-project').innerHTML=(d?.projects||[]).map(p=>`<option value="${esc(p.key)}">${esc(shortPath(p.path))} — ${esc(p.path)}</option>`).join('');if(preferred&&d?.projects.some(p=>p.key===preferred))$('#job-project').value=preferred}
function exportModal(preset={}){
 if(!state.devices.length){deviceModal();return}
 modal(`<h2>创建压缩快照</h2><p class="muted">按项目导出原生会话，保留图片与工具记录。</p><form id="export-form"><label>来源设备<select id="job-device">${deviceOptions()}</select></label><label>项目<select id="job-project" required></select></label><label>工具<select id="job-provider"><option value="codex">Codex</option><option value="claude">Claude Code</option></select></label><label>服务器保存方式<select id="keep-snapshot"><option value="false">临时中转（默认，成功预览后清理，24 小时到期）</option><option value="true">长期快照（手动删除）</option></select></label><div id="export-advice" class="notice"></div><p class="muted">传输上限：单会话 2 GiB，压缩包 2 GiB，展开总量 8 GiB。项目代码不包含在快照内。</p><div class="dialog-actions"><button class="primary">压缩并上传</button></div></form>`);
 if(preset.device)$('#job-device').value=preset.device;
 fillProjects($('#job-device'),preset.project);if(preset.provider)$('#job-provider').value=preset.provider;
 const advice=()=>{const d=state.devices.find(d=>d.id===$('#job-device').value);const i=(d?.inventory||[]).find(i=>i.project===$('#job-project').value&&i.provider===$('#job-provider').value);$('#export-advice').textContent=(!currentClient(d)?'该设备仍是旧版，请先升级客户端。 ':'')+(i?i.count+' 个记录，约 '+bytes(i.bytes)+'，最大单文件 '+bytes(i.largest)+'。 ':'')+(i?.agent_status==='running'?'对应 Agent 正在运行，请完全退出桌面应用与 CLI 后再上传。':'导出前请关闭对应 Agent。')+'保留 relay、SSH 隧道和普通浏览器。';};
 $('#job-device').onchange=()=>{fillProjects($('#job-device'));advice()};$('#job-project').onchange=advice;$('#job-provider').onchange=advice;advice();
 $('#export-form').onsubmit=ev=>{ev.preventDefault();guarded(async()=>{await api('/jobs',{method:'POST',body:JSON.stringify({kind:'export',device_id:$('#job-device').value,project:$('#job-project').value,provider:$('#job-provider').value,keep_snapshot:$('#keep-snapshot').value==='true'})});closeModal();setTab('jobs');await refresh();toast('任务已排队')})};
}
function previewModal(sid){
 const b=state.snapshots.find(x=>x.id===sid);if(!state.devices.length){deviceModal();return}
 const base=b.source_path.split(/[\\/]/).filter(Boolean).pop()||'project';
 const folder=Array.from(base.replace(/[<>:"/\\|?*]/g,'-').replace(/[ .]+$/,'')).slice(0,24).join('')+'-'+b.project.slice(-6);
 modal(`<h2>恢复 ${esc(base)}</h2><p class="muted">${names[b.provider]} · ${b.sessions} 个记录 · ${date(b.created)}</p><form id="preview-form"><label>目标设备<select id="job-device">${deviceOptions()}</select></label><label>导入位置<select id="job-project" required></select></label><label id="new-folder-label" hidden>新项目文件夹名称<input id="new-folder" value="${esc(folder)}" maxlength="100"></label><p class="muted" id="import-location"></p><div class="notice">目标电脑没有此项目或会话也可以导入。新建时先创建空目录和项目映射，会话仍需预览后确认写入。项目代码和依赖请另行准备。请关闭目标电脑上的 ${names[b.provider]}。</div><div class="dialog-actions"><button class="primary">生成恢复预览</button></div></form>`);
 const updateLocation=()=>{const d=state.devices.find(x=>x.id===$('#job-device').value);const isNew=$('#job-project').value==='__new';$('#new-folder-label').hidden=!isNew;$('#new-folder').required=isNew;$('#import-location').textContent=isNew?'将导入到 '+d.import_root+(d.os?.startsWith('windows')?'\\':'/')+$('#new-folder').value:'将映射到所选项目的本机路径。'};
 const update=()=>{const d=state.devices.find(x=>x.id===$('#job-device').value);const ps=(d?.projects||[]).filter(p=>!p.missing);$('#job-project').innerHTML=(d?.import_root?'<option value="__new">＋ 在本机新建项目（无需已有会话）</option>':'')+ps.map(p=>`<option value="${esc(p.key)}">${esc(p.path)}</option>`).join('');if(ps.some(p=>p.key===b.project))$('#job-project').value=b.project;updateLocation()};
 const other=state.devices.find(d=>d.id!==b.device_id&&online(d))||state.devices.find(d=>d.id!==b.device_id);if(other)$('#job-device').value=other.id;
 update();$('#job-device').onchange=update;$('#job-project').onchange=updateLocation;$('#new-folder').oninput=updateLocation;
 $('#preview-form').onsubmit=ev=>{ev.preventDefault();guarded(async()=>{const isNew=$('#job-project').value==='__new';if(b.min_client_version&&!currentClient(state.devices.find(d=>d.id===$('#job-device').value)))throw Error('目标客户端需要升级到 0.3 或更新版本；请保留配置，只替换程序后重试');await api('/jobs',{method:'POST',body:JSON.stringify({kind:'preview',device_id:$('#job-device').value,project:isNew?'':$('#job-project').value,snapshot_id:sid,new_project:isNew,folder:isNew?$('#new-folder').value:''})});closeModal();setTab('jobs');await refresh();toast('设备将生成预览，完成后点击「查看预览」')})};
}
function planModal(j){
 const p=j.result;modal(`<h2>恢复预览</h2><p class="muted">目标：${esc(deviceName(j.device_id))} · ${esc(p.target)}</p>${p.expanded_bytes?`<p class="muted">展开约 ${bytes(p.expanded_bytes)}，建议预留至少 ${bytes(p.expanded_bytes*3)} 的暂存和备份空间。</p>`:''}${p.warnings?.length?`<div class="notice">${p.warnings.map(esc).join('<br>')}</div>`:''}<div class="plan-list">${(p.changes||[]).map(c=>`<div class="plan-line ${esc(c.action)}"><b>${esc(actions[c.action]||c.action)}</b>${esc(c.path)}</div>`).join('')}</div>${p.can_apply?'<p class="notice">预览已保存在目标电脑；临时服务器包可能已清理，不影响确认恢复。写入前再次核对文件并备份，若本机有新变化会拒绝覆盖。</p>':'<p class="error">存在冲突或不支持的记录，本机原文件未变。临时包会保留到到期，请处理冲突后重新预览。</p>'}<div class="dialog-actions"><button class="secondary" id="plan-close">关闭</button>${p.can_apply?'<button class="primary" id="apply-plan">确认恢复</button>':''}</div>`);
 $('#plan-close').onclick=closeModal;if(p.can_apply)$('#apply-plan').onclick=()=>guarded(async()=>{await api('/jobs',{method:'POST',body:JSON.stringify({kind:'restore',device_id:j.device_id,preview_id:j.id})});closeModal();await refresh();toast('恢复任务已排队')});
}
function deviceModal(){
 modal(`<h2>添加设备</h2><p class="muted">客户端会自动扫描 Codex 与 Claude Code 的会话目录，发现项目，无需手填项目路径。</p><form id="device-form"><label>设备名称<input id="device-name" placeholder="例如：MacBook / Windows 工作站" required maxlength="80"></label><label>服务器地址<input id="server-url" value="${esc(state.public_url||location.origin)}" required></label><details><summary>SSH 自动连接与高级设置</summary><label>SSH 服务器（使用隧道时填写）<input id="ssh-host" placeholder="服务器 IP 或主机名"></label><label>SSH 用户<input id="ssh-user" value="root"></label><p class="muted">SSH 私钥不上传。下载配置后运行一次 setup-ssh，核对指纹并验证本机密钥。以后 start-client 自动维护隧道。</p><label>新项目导入根目录<input id="import-root" placeholder="留空：用户目录 / SessionRelayProjects"></label></details><div class="notice">启动时扫描，之后约每分钟刷新。自动扫描只读取本机会话并上报项目列表，创建快照后才上传会话正文。</div><div class="dialog-actions"><button class="primary">生成配置</button></div></form>`);
 $('#device-form').onsubmit=ev=>{ev.preventDefault();guarded(async()=>{const root=$('#import-root').value.trim();const c=await api('/devices',{method:'POST',body:JSON.stringify({name:$('#device-name').value,server:$('#server-url').value})});c.projects=[];const host=$('#ssh-host').value.trim();if(host)c.ssh={host,user:$('#ssh-user').value.trim(),identity_file:'~/.ssh/id_ed25519',known_hosts_file:'~/.ssh/known_hosts',port:22,remote_port:8787};if(root)c.import_root=root;const text=JSON.stringify(c,null,2);$('#modal-body').innerHTML=`<h2>配置已生成</h2><p class="muted">保存到客户端程序旁。使用 SSH 时先运行一次 setup-ssh.cmd / setup-ssh.command，再运行 start-client；以后无需单独开启隧道。</p><pre>Mac: ./relay client --config client.json<br>Windows: .&#92;relay.exe client --config client.json</pre><div class="notice">配置包含设备密钥，请保存在自己的电脑。</div><div class="dialog-actions"><button class="primary" id="download-config">下载 client.json</button></div>`;$('#download-config').onclick=()=>downloadText('client.json',text);await refresh()})};
}
function downloadText(name,text){const url=URL.createObjectURL(new Blob([text],{type:'application/json'}));const a=document.createElement('a');a.href=url;a.download=name;a.click();setTimeout(()=>URL.revokeObjectURL(url),10000)}
document.addEventListener('click',e=>{
 const b=e.target.closest('[data-action]');if(!b)return;const j=state.jobs.find(j=>j.id===b.dataset.id);
 switch(b.dataset.action){
 case'export-project':exportModal({device:b.dataset.device,project:b.dataset.project,provider:b.dataset.provider});break;
 case'retry':guarded(async()=>{await api('/jobs',{method:'POST',body:JSON.stringify({kind:j.kind,device_id:j.device_id,provider:j.provider,project:j.project,snapshot_id:j.snapshot_id,new_project:j.new_project,folder:j.folder,keep_snapshot:j.keep_snapshot})});await refresh();toast('已重新排队')});break;
 case'scan':guarded(async()=>{await api('/jobs',{method:'POST',body:JSON.stringify({kind:'scan',device_id:b.dataset.id})});setTab('jobs');await refresh();toast('已请求设备重新扫描')});break;
 case'device':deviceModal();break;case'export':exportModal();break;case'preview':previewModal(b.dataset.id);break;case'plan':planModal(j);break;
 case'delete-snapshot':modal(`<h2>删除服务器上的快照？</h2><p class="muted">只删除中转站保存的这一份压缩包。电脑上的对话、本地导出和恢复备份不受影响。</p><div class="dialog-actions"><button class="secondary" id="delete-close">取消</button><button class="primary" id="delete-confirm">确认删除</button></div>`);$('#delete-close').onclick=closeModal;$('#delete-confirm').onclick=()=>guarded(async()=>{await api('/snapshots/'+b.dataset.id,{method:'DELETE'});closeModal();await refresh();toast('快照已删除')});break;
case'detail':modal(`<h2>任务详情</h2><pre>${esc(JSON.stringify(j,null,2))}</pre>`);break;
 case'cancel':guarded(async()=>{await api('/jobs/'+j.id,{method:'DELETE'});await refresh()});break;
 case'rollback':modal(`<h2>撤销这次恢复？</h2><p class="muted">恢复前的文件会从本机备份取回。本次新增的会话文件会移除；如果恢复后又有新对话，程序会拒绝覆盖。</p><div class="dialog-actions"><button class="secondary" id="rollback-close">取消</button><button class="primary" id="rollback-confirm">确认撤销</button></div>`);$('#rollback-close').onclick=closeModal;$('#rollback-confirm').onclick=()=>guarded(async()=>{await api('/jobs',{method:'POST',body:JSON.stringify({kind:'rollback',device_id:j.device_id,restore_id:j.id})});closeModal();await refresh()});break;
 }
});
$('#login-form').onsubmit=async e=>{e.preventDefault();$('#login-error').textContent='';try{await api('/login',{method:'POST',body:JSON.stringify({token:$('#token').value})});$('#token').value='';await refresh()}catch(e){$('#login-error').textContent=e.message}};
$('#logout').onclick=async()=>{await api('/logout',{method:'POST'});showLogin()};$('#refresh').onclick=refresh;$('#new-export').onclick=exportModal;$('#add-device').onclick=deviceModal;$('#search').oninput=renderSnapshots;$('#filter').onchange=renderSnapshots;document.querySelectorAll('[data-tab]').forEach(b=>b.onclick=()=>setTab(b.dataset.tab));
let uploadKeep=false;
$('#upload-button').onclick=()=>{modal(`<h2>导入压缩包</h2><label>服务器保存方式<select id="upload-retention"><option value="false">临时中转（24 小时到期，成功预览后清理）</option><option value="true">长期快照（手动删除）</option></select></label><div class="dialog-actions"><button class="primary" id="choose-upload">选择压缩包</button></div>`);$('#choose-upload').onclick=()=>{uploadKeep=$('#upload-retention').value==='true';closeModal();$('#upload-file').click()}};
$('#upload-file').onchange=()=>guarded(async()=>{const f=$('#upload-file').files[0];if(!f)return;if(f.size>2*1024**3)throw Error('压缩包不能超过 2 GiB');toast('正在上传并校验压缩包…');await api('/snapshots/upload'+(uploadKeep?'?keep=true':''),{method:'POST',body:f});$('#upload-file').value='';await refresh();toast(uploadKeep?'已导入长期快照':'已导入临时中转，24 小时后清理')});
refresh();setInterval(()=>{if(logged)refresh()},5000);

// Optional browser tool surface. Authentication and preview checks use the same API.
if(document.modelContext?.registerTool){
 const lifecycle=new AbortController();
 addEventListener('pagehide',()=>lifecycle.abort(),{once:true});
 for(const tool of [
  {name:'read_session_relay_state',title:'查看会话中转状态',description:'Read registered devices, retained snapshots and recent transfer jobs.',inputSchema:{type:'object',properties:{},additionalProperties:false},annotations:{readOnlyHint:true,untrustedContentHint:true},execute:async input=>{if(!input||Object.keys(input).length)throw Error('不接受参数');const s=await api('/state');state=s;render();return s}},
  {name:'start_session_restore_preview',title:'打开恢复预览',description:'Open the restore configuration dialog for a snapshot; this does not write any conversation files.',inputSchema:{type:'object',properties:{snapshot_id:{type:'string'}},required:['snapshot_id'],additionalProperties:false},annotations:{readOnlyHint:false,untrustedContentHint:true},execute:async input=>{if(!input||typeof input.snapshot_id!=='string'||Object.keys(input).length!==1)throw Error('需要 snapshot_id');state=await api('/state');render();if(!state.snapshots.some(b=>b.id===input.snapshot_id))throw Error('快照不存在');setTab('snapshots');previewModal(input.snapshot_id);return {status:'configuration_open'}}}
 ]){try{Promise.resolve(document.modelContext.registerTool(tool,{signal:lifecycle.signal})).catch(()=>{})}catch{}}
}
