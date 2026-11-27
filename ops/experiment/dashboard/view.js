/* Shared offline renderer: simulation and real campaigns use schema 1.
 * No dependencies, external requests or interpolation of telemetry into HTML. */
(function(global){
  'use strict';
  const fmt=(v,d=0)=>Number.isFinite(v)?v.toLocaleString('fr-FR',{maximumFractionDigits:d}):'—';
  const el=(tag,text,cls)=>{const n=document.createElement(tag);if(text!=null)n.textContent=text;if(cls)n.className=cls;return n;};
  const svg=(tag,attrs)=>{const n=document.createElementNS('http://www.w3.org/2000/svg',tag);for(const [k,v] of Object.entries(attrs))n.setAttribute(k,v);return n;};
  function growthMetrics(p){
    const fleet=p.fleet||{};
    const nodes=fleet.vnodes, machines=fleet.machines, simulated=fleet.source==='simulation';
    let ready=p.metrics_stale?null:p.ready_nodes??null;
    let observed=p.metrics_stale?null:p.nodes??null;
    if(simulated&&Array.isArray(nodes)){
      observed=nodes.length;
      ready=nodes.filter(n=>n.state==='ready'||(n.state==='receiving'&&n.estimated_s>0)).length;
    }
    const reporting=new Set((p.metrics_stale?[]:nodes||[]).filter(n=>n.state!=='stale').map(n=>n.machine));
    const counted=machines?.filter(h=>h.state!=='unknown'||reporting.has(h.id)).length;
    const hosts=counted>0||simulated?counted:null;
    const density=hosts>0&&ready!=null&&hosts===machines.length?ready/hosts:null;
    return {ready_count:ready,observed_count:observed,machine_count:hosts,ready_per_machine:density};
  }
  function render(root,points,index=points.length-1){
    root.classList.add('ix-dashboard');root.replaceChildren();
    const cols=Math.max(1,Math.floor((root.clientWidth+16)/436));
    const chartWidth=Math.max(160,Math.min(600,(root.clientWidth-16*(cols-1))/cols-26));
    points=points.map(p=>{
      const hs=p.fleet?.machines||[],out={...p};
      for(const k of ['cpu','ram','disk']){
        const vs=hs.map(h=>h[k]).filter(Number.isFinite);out[k+'_max']=vs.length?Math.max(...vs):null;
      }
      return Object.assign(out,growthMetrics(p));
    });
    if(!points.length){root.append(el('p','Aucune mesure disponible.'));return;}
    const p=points[index],f=p.fleet||{},sim=f.source==='simulation';
    root.append(el('p',sim?'Simulation de ressources : besoins de débit calculés ; ACK, lectures et latences non simulés.':'Mesures AWS : application au détenteur distincte des ACK client. Les tirets désignent une mesure inconnue.','note'));
    if(p.metrics_stale||p.load_stale)root.append(el('p','Télémétrie périmée : '+[p.metrics_stale?'nœuds':'',p.load_stale?'charge':''].filter(Boolean).join(', '),'alert'));
    const machines=f.machines||[],nodes=f.vnodes||[],transfers=f.transfers||[];
    const tiles=el('div',null,'tiles');
    const warm=machines.filter(h=>h.state==='warm').length,boot=machines.filter(h=>h.state==='booting').length;
    const bad=machines.filter(h=>[h.cpu,h.ram,h.disk,h.io].some(v=>Number.isFinite(v)&&v>60)).length;
    const unknown=machines.filter(h=>h.cpu==null||h.ram==null||h.disk==null).length;
    const hasResources=machines.some(h=>[h.cpu,h.ram,h.disk].some(Number.isFinite));
    const hasReadiness=sim||machines.some(h=>h.ready_offer!=null);
    const stats=[['Demande /s',p.load_stale?null:p.offered],['ACK client /s',p.load_stale?null:p.acked],['Vnodes prêts / observés',fmt(p.ready_count)+' / '+fmt(p.observed_count)],['Machines observées',p.machine_count],['Chaudes / démarrage',`${hasReadiness?warm:'?'} / ${sim?boot:'?'}`],['Réceptions admissibles',machines.some(h=>h.ready_offer!=null)?machines.filter(h=>h.ready_offer).length:null],['Machines observées > 60 %',hasResources?bad:null],['Transferts',p.metrics_stale?null:p.open??transfers.length],['p99 écriture, ms',p.load_stale?null:p.p99_ms]];
    for(const [label,value] of stats){const t=el('div',null,'tile');t.append(el('span',label),el('strong',typeof value==='string'?value:fmt(value,1)));tiles.append(t);}root.append(tiles);
    const charts=el('div',null,'charts'),unmeasured=[];
    for(const [title,series,stepped] of [
      ['Écritures /s',[['offered','demandées'],['acked','ACK client'],['estimated_s','besoin réparti (simulation)']]],
      ['Lectures client /s',[['reads_s','tentatives'],['read_failed_s','échecs']]],
      ['Latence écriture, ms',[['p50_ms','p50'],['p95_ms','p95'],['p99_ms','p99']]],
      ['Croissance du réseau',[['ready_count','vnodes prêts'],['machine_count','machines observées']],true],
      ['Densité du réseau',[['ready_per_machine','vnodes prêts par machine']],true],
      ['Transferts ouverts',[['open','ouverts'],['frozen','gelés']]],
      ['Ressources : maximum observé, %',[['cpu_max','CPU'],['ram_max','RAM'],['disk_max','disque']]],
      ['Pression client',[['shed','non envoyées /s'],['queued','en file'],['in_flight','en vol']]],
      ['Lectures et fraîcheur, p99 ms',[['read_p99_ms','lecture'],['fresh_p99_ms','total racine']]]
    ]){
      const past=points.slice(0,index+1);
      if(past.some(p=>series.some(([k])=>Number.isFinite(p[k]))))charts.append(chart(past,title,series,chartWidth,stepped));else unmeasured.push(title);
    }
    root.append(charts);
    if(unmeasured.length)root.append(el('p','Non mesuré : '+unmeasured.join(' · '),'note'));
    const diagnostics=el('details'),more=el('div',null,'charts');diagnostics.append(el('summary','Routage, trafic et remontée des totaux'));
    for(const [title,series] of [
      ['Écritures réussies avec relais, %',[['relayed_pct','avec relais']]],
      ['Trafic moyen par vnode, Kio/s',[['mesh_kb_s','mesh'],['gossip_kb_s','membership']]],
      ['Membership, Kio',[['membership_kb','taille maximale']]],
      ['Latence depuis l’arrivée prévue, ms',[['intended_p50_ms','p50'],['intended_p95_ms','p95'],['intended_p99_ms','p99']]],
      ['Générateur de charge, % CPU',[['loader_pct','CPU']]],
      ['Remontée des totaux',[['flush_s','temps le plus long (s)'],['pending_k','arêtes en attente (milliers)']]]
    ])more.append(chart(points.slice(0,index+1),title,series,chartWidth));diagnostics.append(more);root.append(diagnostics);
    root.append(el('h2','Machines et vnodes'));
    root.append(el('p',`${unknown} machine(s) avec ressources inconnues. Cliquer un vnode pour son détail. Les liens ci-dessous montrent les transferts observés, pas toutes les connexions P2P.`, 'note'));
    const detail=el('p','Sélectionner un vnode.','detail');detail.setAttribute('aria-live','polite');
    const grid=el('div',null,'fleet');
    for(const h of [...machines].sort((a,b)=>pressure(b)-pressure(a)||String(a.id).localeCompare(String(b.id)))){
      const card=el('section',null,'machine '+(pressure(h)>60?'hot':''));
      card.append(el('h3',h.label||h.id),el('p',`${h.state||'unknown'} · CPU : ${h.cpu_scope||'estimé'}`,'note'));
      for(const [key,label] of [['cpu','CPU'],['ram','RAM'],['disk','Disque'],['io','Débit disque']]){
        if(key==='io'&&h[key]==null)continue;
        const row=el('div',null,'resource'),track=el('div',null,'track'),fill=el('i');
        fill.style.width=Math.min(100,Math.max(0,h[key]||0))+'%';if(h[key]>60)fill.className='danger';
        track.append(fill);row.append(el('span',label),track,el('span',fmt(h[key],1)+' %'));card.append(row);
      }
      const strip=el('div',null,'vnodes');
      for(const n of nodes.filter(n=>n.machine===h.id)){
        const b=el('button',String(n.label).slice(0,10),n.state==='stale'?'stale':'');b.type='button';
        const rate=n.applied_s??n.estimated_s;
        b.title=`${n.label} · ${n.state} · ${fmt(rate,1)} ${sim?'requises':'applications'}/s`;
        b.onclick=()=>{detail.textContent=`${n.label} · machine ${h.label||h.id} · ${n.state} · ${fmt(rate,1)} ${sim?'écritures/s requises':'nouvelles applications/s'} · ${fmt(n.items)} items · ${n.monitor||'modèle de ressources'}`;};
        b.append(el('small',fmt(rate,1)+'/s'));strip.append(b);
      }
      card.append(strip);grid.append(card);
    }
    if(!machines.length)grid.append(el('p','Cette archive contient uniquement les agrégats globaux ; aucun inventaire machine n’a été enregistré.'));
    root.append(grid,detail,el('h2','Transferts et délégations'));
    const list=el('div',null,'transfers');
    if(!transfers.length)list.append(el('p',p.metrics_stale?'État des transferts inconnu.':'Aucun transfert observé à cet instant.'));
    for(const tr of transfers){
      const row=el('div',null,'transfer'+(tr.frozen?' hot':''));
      row.append(el('strong',`${tr.source_machine||'?'} → ${tr.target_machine||'destination inconnue'}`),
        el('span',`${tr.phase||'copie'}${tr.stale?' · état périmé':''} · âge ${fmt(tr.age_s,1)} s · ${tr.progress==null?'progression non mesurée':fmt(tr.progress*100,1)+' % copiés'}${tr.share==null?'':' · '+fmt(tr.share*100,1)+' % de charge déplacée'}${tr.frozen?' · gel observé':''}`));list.append(row);
    }
    root.append(list);
  }
  function pressure(h){return Math.max(...['cpu','ram','disk','io'].map(k=>Number.isFinite(h[k])?h[k]:-1));}
  function chart(points,title,series,width,stepped=false){
    const figure=el('figure'),caption=el('figcaption',title),legend=el('div',null,'legend');figure.append(caption,legend);
    const values=series.flatMap(([k])=>points.map(p=>p[k]).filter(Number.isFinite));
    if(!values.length){figure.append(el('p','Non mesuré.','note'));return figure;}
    const w=width,h=190,l=55,r=12,t=12,b=28,top=Math.max(...values,1)*1.08;
    const first=points[0].x??points[0].t??0,last=Math.max(first+1,points.at(-1).x??points.at(-1).t??0);
    const sx=p=>l+((p.x??p.t??0)-first)/(last-first)*(w-l-r),sy=v=>t+(1-v/top)*(h-t-b);
    const g=svg('svg',{viewBox:`0 0 ${w} ${h}`,role:'img','aria-label':title});
    for(const f of [0,.5,1]){g.append(svg('line',{x1:l,x2:w-r,y1:sy(top*f),y2:sy(top*f),class:'gridline'}));const label=svg('text',{x:l-5,y:sy(top*f)+4,'text-anchor':'end'});label.textContent=fmt(top*f,1);g.append(label);}
    for(const [i,[key,label]] of series.entries()){
      if(!points.some(p=>Number.isFinite(p[key])))continue;
      const color=`var(--series-${i+1})`;const leg=el('span',label);leg.style.color=color;legend.append(leg);
      // Null samples split paths; stale intervals never look like valid flat lines.
      let path='',previous=null;
      for(const p of points){
        if(!Number.isFinite(p[key])){path+='|';previous=null;continue;}
        if(stepped&&previous!=null)path+=`${sx(p)},${sy(previous)} `;
        path+=`${sx(p)},${sy(p[key])} `;previous=p[key];
      }
      for(const segment of path.split('|').filter(s=>s.trim()))g.append(svg('polyline',{points:segment,fill:'none',stroke:color,'stroke-width':2}));
    }
    for(const [x,label] of [[l,first],[w-r,last]]){const n=svg('text',{x,y:h-5,'text-anchor':x===l?'start':'end'});n.textContent=fmt(label)+' s';g.append(n);}figure.append(g);return figure;
  }
  global.IndexusDashboard={render,growthMetrics};
})(typeof window==='undefined'?globalThis:window);
