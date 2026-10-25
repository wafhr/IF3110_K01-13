(() => {
  const root=document.getElementById('database-workspace'); if(!root) return;
  const id=root.dataset.databaseId, $key=id=>document.getElementById(id), notice=$key('viewer-notice'), schemaSelect=$key('schema-select'), tableSelect=$key('table-select'), details=$key('table-details');
  const enc=value=>encodeURIComponent(value), base=`/databases/${enc(id)}/schemas`;
  const error=message=>Atlas.setNotice(notice,message,true), clearNotice=()=>Atlas.setNotice(notice,'');
  const option=(select,value,label)=>{const item=document.createElement('option');item.value=value;item.textContent=label;select.append(item)};
  const heading=(parent,tag,text)=>{const node=document.createElement(tag);node.textContent=text;parent.append(node);return node};
  function table(headers, rows){const wrap=document.createElement('div');wrap.className='table-wrap';const t=document.createElement('table'),thead=document.createElement('thead'),tr=document.createElement('tr');headers.forEach(h=>heading(tr,'th',h));thead.append(tr);const body=document.createElement('tbody');rows.forEach(cells=>{const row=document.createElement('tr');cells.forEach(value=>heading(row,'td',value==null?'NULL':String(value)));body.append(row)});t.append(thead,body);wrap.append(t);return wrap}
  async function loadDatabase(){
    const list=(await Atlas.request('/databases')).databases||[], db=list.find(item=>item.id===id);
    if(!db) throw new Error('Database access is unavailable for this account.');
    $key('database-title').textContent=db.name; $key('database-permission').textContent=`${db.database_name} · ${db.permission==='view_edit'?'View and edit':'View only'}`;
    const schemas=(await Atlas.request(base)).schemas||[]; schemaSelect.replaceChildren(); option(schemaSelect,'','Choose schema'); schemas.forEach(schema=>option(schemaSelect,schema.name,schema.name));
  }
  async function loadTables(){
    details.replaceChildren(); heading(details,'p','Choose a table to inspect its columns and rows.');
    tableSelect.disabled=true;tableSelect.replaceChildren();option(tableSelect,'','Loading tables…');
    if(!schemaSelect.value){tableSelect.replaceChildren();option(tableSelect,'','Select a schema first');return}
    const tables=(await Atlas.request(`${base}/${enc(schemaSelect.value)}/tables`)).tables||[];
    tableSelect.replaceChildren();option(tableSelect,'','Choose table');tables.forEach(item=>option(tableSelect,item.name,`${item.name} (${item.type})`));tableSelect.disabled=false;
  }
  async function loadTable(){
    if(!tableSelect.value)return;clearNotice();details.replaceChildren();details.setAttribute('aria-busy','true');
    try{
      const schema=schemaSelect.value, name=tableSelect.value, path=`${base}/${enc(schema)}/tables/${enc(name)}`;
      const result=await Atlas.request(path), info=result.table;
      details.replaceChildren();details.setAttribute('aria-busy','false');
      heading(details,'h3',`${info.schema}.${info.name} · ${info.type}`);heading(details,'h4','Columns');
      details.append(table(['Name','Type','Nullable','Default','Identity','Generated'],(info.columns||[]).map(c=>[c.name,c.data_type,c.nullable?'Yes':'No',c.default||'',c.identity||'',c.generated||''])));
      heading(details,'h4','Constraints');details.append(table(['Name','Type','Definition','Validated'],(info.constraints||[]).map(c=>[c.name,c.type,c.definition,c.validated?'Yes':'No'])));
      heading(details,'h4','Indexes');details.append(table(['Name','Definition','Unique','Primary','Valid'],(info.indexes||[]).map(i=>[i.name,i.definition,i.unique?'Yes':'No',i.primary?'Yes':'No',i.valid?'Yes':'No'])));
      const dataPanel=document.createElement('section');dataPanel.dataset.rowsPanel='true';details.append(dataPanel);
      await loadData(path,dataPanel);
    }catch(e){details.setAttribute('aria-busy','false');error(e.message)}
  }
  async function loadData(path,panel,offset=0){
    const result=await Atlas.request(`${path}/data?limit=50&offset=${offset}`), page=result.data;
    panel.replaceChildren();heading(panel,'h4','Rows');
    const rows=(page.rows||[]).map(row=>row.map(cell=>cell===null?'NULL':cell));
    panel.append(table((page.columns||[]).map(c=>`${c.name} (${c.data_type})`),rows));
    panel.append(Atlas.pagination({offset:page.offset,limit:page.limit,hasMore:page.has_more,onChange:next=>loadData(path,panel,next).catch(e=>error(e.message))}));
    const summary=document.createElement('p');summary.className='muted small-copy';summary.textContent=`Rows ${page.offset+1}–${page.offset+page.rows.length}${page.has_more?' (more available)':''}`;panel.append(summary);
  }
  schemaSelect.onchange=()=>loadTables().catch(e=>error(e.message));tableSelect.onchange=()=>loadTable();
  loadDatabase().catch(error);
})();
