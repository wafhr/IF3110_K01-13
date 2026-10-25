(() => {
  const root=document.getElementById('migration-workspace');if(!root)return;
  const id=root.dataset.databaseId,version=root.dataset.version,$key=value=>document.getElementById(value),notice=$key('migration-notice'),tbody=$key('migration-list');
  const endpoint=`/databases/${encodeURIComponent(id)}/migrations`, frontendBase=`/workspace/databases/${encodeURIComponent(id)}/migrations`;
  const cell=(row,value)=>{const td=document.createElement('td');td.textContent=value==null?'—':String(value);row.append(td);return td};
  const showError=message=>Atlas.setNotice(notice,message,true);
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
    event.preventDefault();const button=event.submitter;button.disabled=true;Atlas.setNotice(notice,'Running migration…');
    try{const result=await Atlas.request(endpoint,'POST',{name:$key('migration-name').value,sql:$key('migration-sql').value});Atlas.setNotice(notice,`Migration ${result.migration.version} completed with status ${result.migration.status}.`);$key('migration-form').reset();await load();showRecord(result.migration)}
    catch(e){if(e.data?.migration)showRecord(e.data.migration);showError(e.message);await load()}
    finally{button.disabled=false}
  });
  load();
})();
