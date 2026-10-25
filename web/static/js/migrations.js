(() => {
  const root=document.getElementById('migration-workspace');if(!root)return;
  const id=root.dataset.databaseId,version=root.dataset.version,$key=value=>document.getElementById(value),notice=$key('migration-notice'),tbody=$key('migration-list');
  const endpoint=`/databases/${encodeURIComponent(id)}/migrations`, frontendBase=`/workspace/databases/${encodeURIComponent(id)}/migrations`;
  const cell=(row,value)=>{const td=document.createElement('td');td.textContent=value==null?'—':String(value);row.append(td);return td};
  const showError=message=>Atlas.setNotice(notice,message,true);
  const form=$key('migration-form'),sqlInput=$key('migration-sql'),planPanel=$key('query-plan');
  let busy=false;
  const setBusy=value=>{busy=value;form.querySelectorAll('button').forEach(button=>{button.disabled=value})};
  sqlInput.addEventListener('input',()=>{planPanel.hidden=true;planPanel.replaceChildren()});
  function showPlan(result){
    planPanel.replaceChildren();planPanel.hidden=false;
    const title=document.createElement('h2');title.textContent='Execution plan · EXPLAIN';planPanel.append(title);
    const explanation=document.createElement('p');explanation.className='muted';explanation.textContent='Estimated rows and planner cost units, not actual execution time. Preview does not run the migration or add a history entry.';planPanel.append(explanation);
    const wrap=document.createElement('div');wrap.className='table-wrap';
    const table=document.createElement('table'),head=table.createTHead().insertRow(),body=table.createTBody();
    ['Node / join type','Relation / index','Estimated rows','Startup cost','Total cost'].forEach(label=>{const th=document.createElement('th');th.textContent=label;head.append(th)});
    function addNode(node,depth){
      const row=body.insertRow(),type=`${node['Node Type']||'Unknown'}${node['Join Type']?` (${node['Join Type']})`:''}`;
      const label=cell(row,`${'  '.repeat(depth)}${depth?'↳ ':''}${type}`);label.style.whiteSpace='pre';
      cell(row,[node['Relation Name'],node['Index Name']].filter(Boolean).join(' / '));
      cell(row,node['Plan Rows']);cell(row,node['Startup Cost']);cell(row,node['Total Cost']);
      (node.Plans||[]).forEach(child=>addNode(child,depth+1));
    }
    (result.plan||[]).forEach(entry=>{if(entry.Plan)addNode(entry.Plan,0)});
    wrap.append(table);planPanel.append(wrap);
    const details=document.createElement('details'),summary=document.createElement('summary'),raw=document.createElement('pre');
    summary.textContent='Full plan JSON';raw.textContent=JSON.stringify(result.plan,null,2);details.append(summary,raw);planPanel.append(details);
  }
  $key('preview-query-plan').addEventListener('click',async()=>{
    if(busy||!sqlInput.reportValidity())return;
    const sql=sqlInput.value;setBusy(true);planPanel.hidden=true;Atlas.setNotice(notice,'Planning query…');
    try{
      const result=await Atlas.request(`${endpoint}/explain`,'POST',{sql});
      if(sqlInput.value!==sql){showError('SQL changed while planning. Preview again to see the current plan.');return}
      showPlan(result.query_plan);Atlas.setNotice(notice,'Execution plan ready.');
    }catch(e){showError(e.message)}finally{setBusy(false)}
  });
  function showRecord(record){const panel=$key('migration-detail');panel.hidden=false;panel.replaceChildren();const title=document.createElement('h2');title.textContent=`Migration ${record.version}: ${record.name}`;panel.append(title);const status=document.createElement('p');status.textContent=`Status: ${record.status} · Started: ${record.started_at}${record.finished_at?` · Finished: ${record.finished_at}`:''}`;panel.append(status);const sqlTitle=document.createElement('h3');sqlTitle.textContent='SQL';panel.append(sqlTitle);const pre=document.createElement('pre');pre.textContent=record.sql;panel.append(pre);const back=document.createElement('a');back.href=frontendBase;back.textContent='← Back to migration history';panel.append(back);}
  async function load(){
    try{
      const accessList=(await Atlas.request('/databases')).databases||[],db=accessList.find(item=>item.id===id);
      if(!db)throw new Error('Database access is unavailable for this account.');
      $key('migration-database-title').textContent=`Migrations · ${db.name}`;
      $key('migration-permission').textContent=db.permission==='view_edit'?'View and edit access':'View only access';
      if(db.permission==='view_edit')$key('migration-editor').hidden=false;
      const records=(await Atlas.request(endpoint)).migrations||[];tbody.replaceChildren();
      if(!records.length){const row=document.createElement('tr');cell(row,'No migrations have been run yet.');tbody.append(row)}
      records.forEach(record=>{const row=document.createElement('tr'),link=document.createElement('a');link.href=`${frontendBase}/${record.version}`;link.textContent=String(record.version);const td=document.createElement('td');td.append(link);row.append(td);cell(row,record.name);cell(row,record.status);cell(row,record.started_at);cell(row,record.finished_at);tbody.append(row)});
      if(version){const detail=await Atlas.request(`${endpoint}/${encodeURIComponent(version)}`);showRecord(detail.migration)}
    }catch(e){showError(e.message)}
  }
  $key('migration-form').addEventListener('submit',async event=>{
    event.preventDefault();if(busy)return;setBusy(true);planPanel.hidden=true;Atlas.setNotice(notice,'Running migration…');
    try{const result=await Atlas.request(endpoint,'POST',{name:$key('migration-name').value,sql:$key('migration-sql').value});Atlas.setNotice(notice,`Migration ${result.migration.version} completed with status ${result.migration.status}.`);$key('migration-form').reset();await load();showRecord(result.migration)}
    catch(e){if(e.data?.migration)showRecord(e.data.migration);showError(e.message);await load()}
    finally{setBusy(false)}
  });
  load();
})();
