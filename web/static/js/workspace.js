(() => {
  const list = document.getElementById('workspace-list'), notice = document.getElementById('workspace-notice');
  const addText = (parent, tag, text, className) => { const node=document.createElement(tag); node.textContent=text; if(className) node.className=className; parent.append(node); return node; };
  Atlas.request('/databases').then(result => {
    list.replaceChildren(); list.setAttribute('aria-busy','false');
    const databases=result.databases||[];
    if(!databases.length){ const empty=document.createElement('div'); empty.className='empty-state'; addText(empty,'h3','No database workspaces yet'); addText(empty,'p','A Platform Admin must grant you database access through one of your groups.'); list.append(empty); return; }
    databases.forEach(database => {
      const card=document.createElement('a'); card.className='feature-card'; card.href=`/workspace/databases/${encodeURIComponent(database.id)}`;
      addText(card,'span','DATABASE','eyebrow'); addText(card,'h2',database.name); addText(card,'p',database.database_name,'muted');
      const actions=document.createElement('div'); actions.className='workspace-card-actions'; addText(actions,'span',database.permission==='view_edit'?'View and edit':'View only','small-tag'); addText(actions,'span','Open workspace →','card-link'); card.append(actions); list.append(card);
    });
  }).catch(error => { list.setAttribute('aria-busy','false'); list.replaceChildren(); Atlas.setNotice(notice,error.message,true); });
})();
