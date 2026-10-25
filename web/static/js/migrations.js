(() => {
  const root=document.getElementById('migration-workspace');if(!root)return;
  const id=root.dataset.databaseId,version=root.dataset.version,$key=value=>document.getElementById(value),notice=$key('migration-notice'),tbody=$key('migration-list');
  const endpoint=`/databases/${encodeURIComponent(id)}/migrations`, frontendBase=`/workspace/databases/${encodeURIComponent(id)}/migrations`;
  const cell=(row,value)=>{const td=document.createElement('td');td.textContent=value==null?'—':String(value);row.append(td);return td};
  const showError=message=>Atlas.setNotice(notice,message,true);
  const form=$key('migration-form'),sqlInput=$key('migration-sql'),planPanel=$key('query-plan'),modeInput=$key('query-plan-mode');
  const diffButton=$key('compare-schema'),diffPanel=$key('schema-diff-results');
  let busy=false;
  const setBusy=value=>{busy=value;form.querySelectorAll('button').forEach(button=>{button.disabled=value})};
  const clearPlan=()=>{planPanel.hidden=true;planPanel.replaceChildren()};
  sqlInput.addEventListener('input',clearPlan);
  diffButton.addEventListener('click',async()=>{
    let desired;try{desired=JSON.parse($key('desired-schema').value)}catch(_){showError('Desired schema must be valid JSON.');return}
    diffButton.disabled=true;diffPanel.hidden=true;clearPlan();Atlas.setNotice(notice,'Comparing database schemas…');
    try{
      const result=await Atlas.request(`${endpoint}/schema-diff`,'POST',{desired_schema:desired}),diff=result.schema_diff;
      diffPanel.replaceChildren();diffPanel.hidden=false;
      const title=document.createElement('h3');title.textContent='Schema differences';diffPanel.append(title);
      (diff.changes||[]).forEach(change=>{const item=document.createElement('p');item.textContent=`${change.kind} · ${change.object}: ${change.detail}${change.destructive?' (review carefully)':''}`;diffPanel.append(item)});
      if(!diff.changes?.length){const item=document.createElement('p');item.textContent='Schemas match.';diffPanel.append(item)}
      (diff.notes||[]).forEach(note=>{const item=document.createElement('p');item.className='muted';item.textContent=note;diffPanel.append(item)});
      sqlInput.value=diff.sql||'';if(!$key('migration-name').value)$key('migration-name').value='Schema update';
      Atlas.setNotice(notice,'Comparison complete. Review and edit the generated SQL before running it.');
    }catch(e){showError(e.message)}finally{diffButton.disabled=false}
  });
  modeInput.addEventListener('change',()=>{clearPlan();$key('query-plan-warning').hidden=modeInput.value!=='explain_analyze'});
  function showPlan(result){
    planPanel.replaceChildren();planPanel.hidden=false;
    const analyzed=result.mode==='explain_analyze';
    const title=document.createElement('h2');title.textContent=`Execution plan · ${analyzed?'EXPLAIN ANALYZE':'EXPLAIN'}`;planPanel.append(title);
    const explanation=document.createElement('p');explanation.className='muted';explanation.textContent=analyzed?'SQL was executed and the transaction was rolled back. Actual times are milliseconds; row counts and node times are averages per loop. Sequence/auto-increment values and external effects may persist.':'Estimated rows and planner cost units, not actual execution time. Preview does not execute the migration.';planPanel.append(explanation);
    if(!Array.isArray(result.plan)){const native=document.createElement('pre');native.textContent=typeof result.plan==='string'?result.plan:JSON.stringify(result.plan,null,2);planPanel.append(native);return}
    (result.plan||[]).forEach(entry=>{
      if(entry['Execution Time']!=null){const timing=document.createElement('p');timing.textContent=`Planning: ${entry['Planning Time']??'—'} ms · Execution: ${entry['Execution Time']} ms`;planPanel.append(timing)}
    });
    const wrap=document.createElement('div');wrap.className='table-wrap';
    const table=document.createElement('table'),head=table.createTHead().insertRow(),body=table.createTBody();
    const columns=['Node / join type','Relation / index','Estimated rows','Startup cost','Total cost'];
    if(analyzed)columns.push('Actual rows / loop','Loops','Actual time ms / loop');
    columns.forEach(label=>{const th=document.createElement('th');th.textContent=label;head.append(th)});
    function addNode(node,depth){
      const row=body.insertRow(),type=`${node['Node Type']||'Unknown'}${node['Join Type']?` (${node['Join Type']})`:''}`;
      const label=cell(row,`${'  '.repeat(depth)}${depth?'↳ ':''}${type}`);label.style.whiteSpace='pre';
      cell(row,[node['Relation Name'],node['Index Name']].filter(Boolean).join(' / '));
      cell(row,node['Plan Rows']);cell(row,node['Startup Cost']);cell(row,node['Total Cost']);
      if(analyzed){cell(row,node['Actual Rows']);cell(row,node['Actual Loops']);cell(row,node['Actual Total Time'])}
      (node.Plans||[]).forEach(child=>addNode(child,depth+1));
    }
    (result.plan||[]).forEach(entry=>{if(entry.Plan)addNode(entry.Plan,0)});
    wrap.append(table);planPanel.append(wrap);
    const details=document.createElement('details'),summary=document.createElement('summary'),raw=document.createElement('pre');
    summary.textContent='Full plan JSON';raw.textContent=JSON.stringify(result.plan,null,2);details.append(summary,raw);planPanel.append(details);
  }
  $key('preview-query-plan').addEventListener('click',async()=>{
    if(busy||!sqlInput.reportValidity())return;
    const sql=sqlInput.value,analyze=modeInput.value==='explain_analyze';
    if(analyze&&!window.confirm('EXPLAIN ANALYZE executes SQL. Transactional changes will be rolled back, but sequences and external effects may persist. Continue?'))return;
    setBusy(true);planPanel.hidden=true;Atlas.setNotice(notice,analyze?'Executing query for analysis…':'Planning query…');
    try{
      const result=await Atlas.request(`${endpoint}/explain`,'POST',{sql,analyze});
      if(sqlInput.value!==sql||(modeInput.value==='explain_analyze')!==analyze){showError('SQL or mode changed while planning. Preview again to see the current plan.');return}
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
      $key('migration-editor').querySelector('.muted').textContent=db.dbms==='mysql'?'MySQL may implicitly commit DDL; only transaction-safe statements can be rolled back.':'SQL runs transactionally. Failed statements are rolled back and retained in history.';
      if(db.permission==='view_edit')$key('migration-editor').hidden=false;
      const records=(await Atlas.request(endpoint)).migrations||[];tbody.replaceChildren();
      if(!records.length){const row=document.createElement('tr');cell(row,'No migrations have been run yet.');tbody.append(row)}
      records.forEach(record=>{const row=document.createElement('tr'),link=document.createElement('a');link.href=`${frontendBase}/${record.version}`;link.textContent=String(record.version);const td=document.createElement('td');td.append(link);row.append(td);cell(row,record.name);cell(row,record.status);cell(row,record.started_at);cell(row,record.finished_at);tbody.append(row)});
      if(version){const detail=await Atlas.request(`${endpoint}/${encodeURIComponent(version)}`);showRecord(detail.migration)}
    }catch(e){showError(e.message)}
  }
  $key('migration-form').addEventListener('submit',async event=>{
    event.preventDefault();if(busy)return;setBusy(true);planPanel.hidden=true;Atlas.setNotice(notice,'Running migration…');
    try{const result=await Atlas.request(endpoint,'POST',{name:$key('migration-name').value,sql:$key('migration-sql').value});Atlas.setNotice(notice,`Migration ${result.migration.version} completed with status ${result.migration.status}.`);$key('migration-form').reset();$key('query-plan-warning').hidden=true;await load();showRecord(result.migration)}
    catch(e){if(e.data?.migration)showRecord(e.data.migration);showError(e.message);await load()}
    finally{setBusy(false)}
  });
  load();
})();
