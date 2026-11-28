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
  const prepared=new WeakMap();
  const scopes=new WeakMap();
  function selectedPoint(p,ids){
    const fresh=(p.fleet?.vnodes||[]).filter(n=>ids.has(n.label)&&n.state!=='stale');
    const complete=!p.metrics_stale&&fresh.length===ids.size&&new Set(fresh.map(n=>n.label)).size===ids.size;
    const sum=get=>complete&&fresh.every(n=>Number.isFinite(get(n)))?fresh.reduce((s,n)=>s+get(n),0):null;
    const incident=(p.fleet?.transfers||[]).filter(tr=>!tr.stale&&fresh.some(n=>touchesNode(tr,n)));
    const hosts=new Set(fresh.map(n=>n.machine));
    const machines=(p.fleet?.machines||[]).filter(h=>hosts.has(h.id));
    const peak=key=>complete&&machines.length===hosts.size&&machines.every(h=>Number.isFinite(h[key]))?
      Math.max(...machines.map(h=>h[key])):null;
    return {ready:p.metrics_stale?null:fresh.filter(n=>n.state==='ready').length,
      reporting:p.metrics_stale?null:fresh.length,cpu:peak('cpu'),ram:peak('ram'),disk:peak('disk'),
      peakRate:complete&&fresh.every(n=>Number.isFinite(n.applied_s??n.estimated_s))?
        Math.max(...fresh.map(n=>n.applied_s??n.estimated_s)):null,
      x:p.x,t:p.t,rate:sum(n=>n.applied_s??n.estimated_s),stock:sum(n=>n.items_counted===false?null:n.items),
      open:complete?incident.length:null,frozen:complete?incident.filter(tr=>tr.frozen).length:null};
  }
  function bindPick(root,element,labels,text){
    const scope=scopes.get(root);
    element.dataset.tip=text;
    element.setAttribute('aria-label',text);
    if(!labels.length)return;
    element.setAttribute('role','button');element.setAttribute('tabindex','0');
    const picked=labels.every(id=>scope.ids.has(id));
    element.setAttribute('aria-pressed',String(picked));
    if(scope.ids.size&&!picked)element.classList.add('outside-scope');
    element.onclick=e=>{
      for(const id of labels){if(picked)scope.ids.delete(id);else scope.ids.add(id);}
      scope.refresh();
      const replacement=[...root.querySelectorAll('.network-pane [data-tip]')].find(item=>item.dataset.tip===text);
      if(replacement&&e?.detail===0)replacement.focus({preventScroll:true});
      if(replacement&&e?.clientX)replacement.dispatchEvent(new PointerEvent('pointermove',{bubbles:true,clientX:e.clientX,clientY:e.clientY}));
    };
    element.onkeydown=e=>{if(e.key==='Enter'||e.key===' '){e.preventDefault();element.onclick(e);}};
  }
  function transferTip(tr){
    return [tr.source_machine+' → '+(tr.target_machine||'?'),tr.phase,'âge '+fmt(tr.age_s,1)+' s',
      tr.frozen?'Bascule en cours par lots de zones':null,tr.attempts!=null?'Tentative '+tr.attempts:null,
      tr.installed_zones!=null?fmt(tr.installed_zones)+' installations':null,
      tr.last_install_s!=null?'Dernier lot il y a '+fmt(tr.last_install_s,1)+' s':null,
      tr.phase_age_s!=null?'Dans cette phase depuis '+fmt(tr.phase_age_s,1)+' s':null].filter(Boolean).join(' · ');
  }
  function installTips(plot){
    const tip=el('div',null,'graph-tip');tip.hidden=true;tip.setAttribute('role','tooltip');plot.append(tip);
    function show(e){
      const hit=e.target.closest('[data-tip]');
      if(!hit){tip.hidden=true;return;}
      const r=hit.getBoundingClientRect();
      tip.textContent=hit.dataset.tip;tip.hidden=false;
      tip.style.left=Math.max(4,Math.min((e.clientX??r.x)+12,innerWidth-tip.offsetWidth-8))+'px';
      tip.style.top=Math.max(4,Math.min((e.clientY??r.y)+14,innerHeight-tip.offsetHeight-8))+'px';
    }
    plot.onpointermove=show;plot.onfocusin=show;plot.onpointerdown=show;
    plot.onpointerleave=plot.onfocusout=()=>{tip.hidden=true;};
  }
  function render(root,points,index=points.length-1){
    const previous=new Map();
    if(!global.matchMedia('(prefers-reduced-motion: reduce)').matches){
      for(const g of root.querySelectorAll('.topology [data-glyph]')){
        previous.set(g.dataset.glyph,[Number(g.dataset.x),Number(g.dataset.y)]);
      }
    }
    const frame=document.createDocumentFragment();
    const sections=['.transfers','.topology','details','.charts','.tiles'];
    const anchor=sections.find(sel=>{
      const r=root.querySelector(sel)?.getBoundingClientRect();
      return r&&r.top<=0&&r.bottom>0;
    });
    const offset=anchor&&root.querySelector(anchor).getBoundingClientRect().top;
    root.classList.add('ix-dashboard');
    const raw=points;
    const scope=scopes.get(root)||{ids:new Set()};
    scopes.set(root,scope);scope.refresh=()=>render(root,raw,index);
    const filtered=scope.ids.size>0;
    points=prepared.get(raw)||raw.map(p=>{
      const hs=p.fleet?.machines||[],out={...p};
      for(const k of ['cpu','ram','disk']){
        const vs=hs.map(h=>h[k]).filter(Number.isFinite);out[k+'_max']=vs.length?Math.max(...vs):null;
      }
      const stock=itemSum(p.fleet?.vnodes||[],p.metrics_stale);
      out.stock=stock.value??(!p.metrics_stale&&!p.fleet?.vnodes?.length?p.items:null);
      out.coverage=stock;
      return Object.assign(out,growthMetrics(p),hostSpread(p));
    });
    prepared.set(raw,points);
    if(!points.length){root.replaceChildren(el('p','Aucune mesure disponible.'));return;}
    const p=points[index],f=p.fleet||{},sim=f.source==='simulation';
    const machines=f.machines||[],nodes=f.vnodes||[],transfers=f.transfers||[];
    const tiles=el('div',null,'tiles');
    const warm=machines.filter(h=>h.state==='warm').length,boot=machines.filter(h=>h.state==='booting').length;
    const bad=machines.filter(h=>[h.cpu,h.ram,h.disk,h.io].some(v=>Number.isFinite(v)&&v>60)).length;

    const hasResources=machines.some(h=>[h.cpu,h.ram,h.disk].some(Number.isFinite));
    const hasReadiness=sim||machines.some(h=>h.ready_offer!=null);
    const stats=[['Demande /s',p.load_stale?null:p.offered],['ACK client /s',p.load_stale?null:p.acked],['Items observés',p.stock],['Vnodes comptés / inventoriés',p.coverage.total?fmt(p.coverage.count)+' / '+fmt(p.coverage.total):'—'],['Vnodes prêts / observés',fmt(p.ready_count)+' / '+fmt(p.observed_count)],['Machines observées',p.machine_count],['Chaudes / démarrage',`${hasReadiness?warm:'?'} / ${sim?boot:'?'}`],['Réceptions admissibles',machines.some(h=>h.ready_offer!=null)?machines.filter(h=>h.ready_offer).length:null],['Machines observées > 60 %',hasResources?bad:null],['Transferts',p.metrics_stale?null:p.open??transfers.length],['p99 écriture, ms',p.load_stale?null:p.p99_ms]];
    const scoped=filtered?points.slice(0,index+1).map(pt=>selectedPoint(pt,scope.ids)):null;
    const last=scoped?.at(-1);
    const headline=filtered?[['Vnodes sélectionnés',scope.ids.size],['Applications /s',last.rate],['Items',last.stock],['Transferts observés',last.open]]:
      stats.filter((_,i)=>[0,1,2,5,9,10].includes(i));
    for(const [label,value] of headline){const t=el('div',null,'tile');t.append(el('span',label),el('strong',typeof value==='string'?value:fmt(value,1)));tiles.append(t);}frame.append(tiles);
    const charts=el('div',null,'charts overview');
    const diagnostics=el('section',null,'diagnostics'),more=el('div',null,'charts');
    diagnostics.append(el('h2','Autres mesures du réseau'));
    const catalog=filtered?[[sim?'Demande estimée /s':'Écritures appliquées /s',[['rate','sélection']]],['Items détenus',[['stock','sélection']]],['Transferts observés',[['open','ouverts'],['frozen','en bascule']]],
        ['Vnodes de la sélection',[['ready','prêts'],['reporting','observés']]],
        ['Débit maximal par vnode /s',[['peakRate','maximum']]],
        ['Ressources des hôtes, max. %',[['cpu','CPU'],['ram','RAM'],['disk','disque']]]]:[
      ['Écritures /s',[['offered','demandées'],['acked','ACK client'],['estimated_s','besoin réparti (simulation)']]],
      ['Lectures client /s',[['reads_s','tentatives'],['read_failed_s','échecs']]],
      ['Latence écriture, ms',[['p50_ms','p50'],['p95_ms','p95'],['p99_ms','p99']]],
      ['Items observés dans le réseau',[['stock','items comptés']]],
      ['Croissance du réseau',[['ready_count','vnodes prêts'],['machine_count','machines']],true],
      ['Écritures/s par machine active',[['hostRateLow','minimum'],['hostRateMedian','médiane'],['hostRateHigh','maximum']]],
      ['Items par machine active',[['hostItemsLow','minimum'],['hostItemsMedian','médiane'],['hostItemsHigh','maximum']]],
      ['Densité du réseau',[['ready_per_machine','vnodes prêts par machine']],true],
      ['Transferts ouverts',[['open','ouverts'],['frozen','en bascule']]],
      ['Ressources : maximum observé, %',[['cpu_max','CPU'],['ram_max','RAM'],['disk_max','disque']]],
      ['Pression client',[['shed','non envoyées /s'],['queued','en file'],['in_flight','en vol']]],
      ['Lectures et fraîcheur, p99 ms',[['read_p99_ms','lecture'],['fresh_p99_ms','total racine']]],
      ['Écritures réussies avec relais, %',[['relayed_pct','avec relais']]],
      ['Trafic moyen par vnode, Kio/s',[['mesh_kb_s','mesh'],['gossip_kb_s','membership']]],
      ['Membership, Kio',[['membership_kb','taille maximale']]],
      ['Latence depuis l’arrivée prévue, ms',[['intended_p50_ms','p50'],['intended_p95_ms','p95'],['intended_p99_ms','p99']]],
      ['Générateur de charge, % CPU',[['loader_pct','CPU']]],
      ['Remontée des totaux',[['flush_s','temps le plus long (s)'],['pending_k','arêtes en attente (milliers)']]]
    ];
    const past=filtered?scoped:points.slice(0,index+1);
    const available=catalog.map(([,series])=>past.some(p=>series.some(([key])=>Number.isFinite(p[key]))));
    const layoutKey=filtered?'vnodeCharts':'networkCharts';
    const defaults=filtered?[0,1,2,3,4,5]:[0,2,3,4,8,9];
    if(!scope[layoutKey]){
      let saved;
      try{saved=JSON.parse(sessionStorage.getItem(location.pathname+':'+layoutKey));}catch{}
      scope[layoutKey]=Array.isArray(saved)&&saved.length===6&&new Set(saved).size===6&&
        saved.every(id=>Number.isInteger(id)&&id>=0&&id<catalog.length)?saved:defaults.slice();
    }
    const layout=scope[layoutKey];
    function panel(slot){
      const id=layout[slot],figure=chart(past,...catalog[id]);
      const select=el('select');select.setAttribute('aria-label','Graphique du panneau '+(slot+1));
      catalog.forEach(([title],i)=>{
        const option=el('option',title+(available[i]?'':' — non mesuré'));option.value=i;
        select.append(option);
      });
      select.value=id;
      figure.querySelector('figcaption').firstChild.replaceWith(select);
      select.onchange=()=>{
        const next=Number(select.value),other=layout.indexOf(next);
        if(other>=0)layout[other]=id;
        layout[slot]=next;
        try{sessionStorage.setItem(location.pathname+':'+layoutKey,JSON.stringify(layout));}catch{}
        charts.children[slot].replaceWith(panel(slot));
        if(other>=0&&other!==slot)charts.children[other].replaceWith(panel(other));
        drawRemaining();
        charts.children[slot].querySelector('select').focus({preventScroll:true});
      };
      return figure;
    }
    layout.forEach((_,slot)=>charts.append(panel(slot)));
    function drawRemaining(){
      if(!filtered)more.replaceChildren(...catalog.flatMap((spec,id)=>layout.includes(id)?[]:[chart(past,...spec)]));
    }
    const choices=el('div',null,'scene-options'), plot=el('div',null,'network-pane');
    const active=p.metrics_stale?[]:transfers.filter(tr=>!tr.stale);
    const unknownLinks=transfers.length-active.length;

    const waiting=active.filter(tr=>tr.phase==='copy-slot').length;
    const modes=[['hosts','Par machine',''],['radial','Arbre radial','#vnodes'],['space','Réseau 3D','#reseau-3d'],['shares','Plages d’adresses','#plages']];
    const picker=el('select',null,'scene-mode');picker.setAttribute('aria-label','Vue du réseau');
    for(const [key,title] of modes){
      const option=el('option',title);option.value=key;picker.append(option);
    }
    picker.value=root.dataset.mode||modes.find(([, ,hash])=>hash===global.location.hash)?.[0]||'hosts';
    picker.onchange=()=>{
      root.dataset.mode=picker.value;
      if(modes.some(([, ,hash])=>hash===global.location.hash)){
        const url=new URL(global.location.href);url.hash=modes.find(([key])=>key===picker.value)[2];
        global.history.replaceState(global.history.state,'',url);
      }
      drawTopology();
    };
    if(filtered){
      const clear=el('button','Vue globale · effacer la sélection ('+scope.ids.size+')');
      clear.type='button';clear.title=[...scope.ids].join(', ');
      clear.onclick=()=>{scope.ids.clear();scope.refresh();};choices.append(clear);
    }
    if(waiting){
      const label=el('label'),box=el('input');box.type='checkbox';box.checked=root.dataset.showWaiting==='true';
      box.onchange=()=>{root.dataset.showWaiting=String(box.checked);drawTopology();};
      label.append(box,document.createTextNode(' Attentes de créneau ('+fmt(waiting)+')'));choices.append(label);
    }
    function drawTopology(){
      const mode=picker.value;
      const shown=root.dataset.showWaiting==='true'?active:active.filter(tr=>tr.phase!=='copy-slot');
      const visible=filtered?shown.filter(tr=>nodes.some(n=>scope.ids.has(n.label)&&touchesNode(tr,n))):shown;
      const scene=topology(machines,nodes,visible,sim,p.metrics_stale,mode,root);
      const viewport=scene.querySelector('.network-frame')||scene;
      viewport.prepend(picker);
      if(choices.childElementCount)viewport.append(choices);
      plot.replaceChildren(scene);installTips(plot);
    }
    drawTopology();
    const workspace=el('div',null,'network-workspace');
    workspace.append(plot,charts);
    frame.append(workspace);
    if(!filtered){
      const extra=el('div',null,'tiles');
      for(const [label,value] of stats.filter((_,i)=>![0,1,2,5,9,10].includes(i))){
        const tile=el('div',null,'tile');tile.append(el('span',label),el('strong',String(value??'—')));extra.append(tile);
      }
      drawRemaining();
      diagnostics.append(extra,more);
      if(unknownLinks)diagnostics.append(el('p',fmt(unknownLinks)+' observations de transfert périmées.'));
      frame.append(diagnostics);
    }
    root.replaceChildren(frame);
    if(anchor){
      const target=root.querySelector(anchor);
      if(target)global.scrollBy({top:target.getBoundingClientRect().top-offset,behavior:'instant'});
    }
    if(previous.size){
      for(const g of root.querySelectorAll('.topology [data-glyph]')){
        const pos=previous.get(g.dataset.glyph);
        if(!pos){g.animate([{opacity:0},{opacity:1}],450);continue;}
        const dx=pos[0]-Number(g.dataset.x),dy=pos[1]-Number(g.dataset.y);
        if(dx||dy)g.animate([{transform:`translate(${dx}px,${dy}px)`},{transform:'translate(0,0)'}],{duration:600,easing:'ease-out'});
      }
    }
  }
  function departedTransfers(before,now){
    if(!before?.fleet||!now.fleet||before.metrics_stale||now.metrics_stale)return null;
    const reported=new Set((now.fleet.vnodes||[]).filter(n=>n.state!=='stale').map(n=>n.id));
    const present=new Set((now.fleet.transfers||[]).map(tr=>tr.id));
    return (before.fleet.transfers||[]).filter(tr=>!tr.stale&&!present.has(tr.id)&&
      (now.fleet.source==='simulation'||reported.has(tr.source))).length;
  }
  function hostSpread(p){
    const sums=new Map(),result={};
    if(p.metrics_stale)return result;
    for(const n of p.fleet?.vnodes||[]){
      if(!sums.has(n.machine))sums.set(n.machine,{hostItems:0,hostRate:0});
      const h=sums.get(n.machine);
      for(const [key,val] of [['hostItems',n.items_counted===false?null:n.items],['hostRate',n.applied_s??n.estimated_s]]){
        if(n.state==='stale'||!Number.isFinite(val))h[key]=null;
        else if(h[key]!=null)h[key]+=val;
      }
    }
    for(const key of ['hostRate','hostItems']){
      const v=[...sums.values()].map(h=>h[key]).filter(Number.isFinite).sort((a,b)=>a-b);
      if(v.length){
        result[key+'Low']=v[0];result[key+'High']=v.at(-1);
        result[key+'Median']=(v[Math.floor((v.length-1)/2)]+v[Math.floor(v.length/2)])/2;
      }
    }
    return result;
  }
  function itemSum(nodes,stale){
    const counted=stale?[]:nodes.filter(n=>n.state!=='stale'&&n.items_counted!==false&&Number.isFinite(n.items));
    return {value:counted.length?counted.reduce((sum,n)=>sum+n.items,0):null,count:counted.length,total:nodes.length};
  }
  function topology(machines,nodes,transfers,sim,stale,mode,root){
    const wrap=el('div',null,'topology');
    if(!machines.length){wrap.append(el('p','Aucun inventaire machine enregistré.'));return wrap;}
    if(mode==='space')return spaceView(root,nodes,transfers,stale,wrap);
    if(mode==='shares')return shareView(nodes,stale,wrap,root);
    const colors={ready:'var(--ix-green)',receiving:'var(--ix-blue)',joining:'var(--ix-blue)',unknown:'var(--ix-line)'};
    const canvas=svg('svg',{'aria-label':'Machines, vnodes et sens des transferts',role:'group'});
    const defs=svg('defs',{});
    for(const [id,color] of [['flow','var(--ix-blue)'],['fence','var(--ix-orange)']]){
      const marker=svg('marker',{id,viewBox:'0 0 10 10',refX:9,refY:5,markerWidth:6,markerHeight:6,orient:'auto'});
      marker.append(svg('path',{d:'M0 0 L10 5 L0 10 Z',fill:color}));defs.append(marker);
    }
    const links=svg('g',{}),glyphs=svg('g',{});
    canvas.append(defs,glyphs,links);
    const hosts=new Map(),ends=new Map(),ordered=[...machines].sort((a,b)=>String(a.id).localeCompare(String(b.id),undefined,{numeric:true}));
    const members=new Map(ordered.map(h=>[h.id,[]]));
    for(const n of nodes)members.get(n.machine)?.push(n);
    const widest=Math.max(1,...Array.from(members.values(),ns=>Math.ceil(Math.sqrt(ns.length))));
    const radius=Math.max(38,widest*11),cell=radius*2+62,step=cell+36,cols=Math.max(1,Math.min(ordered.length,Math.ceil(Math.sqrt(ordered.length * step/cell))));
    canvas.setAttribute('viewBox',`0 0 ${cols*cell} ${Math.ceil(ordered.length/cols)*step}`);
    function label(group,x,y,value){
      const t=svg('text',{x,y,'text-anchor':'middle'});t.textContent=value;group.append(t);
    }
    function select(group,description,labels,extra){
      bindPick(root,group,labels,description);
      if(extra)group.onpointerenter=()=>{group.dataset.tip=description+extra();};
    }
    if(mode==='radial'){
      canvas.setAttribute('class','radial');
      radialNodes(nodes,glyphs,ends,canvas,colors,select,sim,stale);
    }else ordered.forEach((h,i)=>{
      const ns=members.get(h.id),x=(i%cols+.5)*cell,y=Math.floor(i/cols)*step+radius+12;
      hosts.set(h.id,{x,y,r:radius});
      const g=svg('g',{'data-glyph':'host:'+h.id,'data-x':x,'data-y':y}),sum=itemSum(ns,stale);
      g.append(svg('circle',{cx:x,cy:y,r:radius,fill:'var(--ix-tint)',stroke:pressure(h)>60?'var(--ix-orange)':'var(--ix-line)','stroke-width':2}));
      const observed=fmt(sum.value)+' items';
      const admission=(Number.isFinite(h.reserve_cores)?' · réserve '+fmt(h.reserve_cores,2)+' vCPU':'')+
        (h.refusal?' · réception refusée : '+h.refusal:(h.ready_offer===true?' · réception disponible':''));
      const resources=[['cpu','CPU'],['ram','RAM'],['disk','Disque']];
      label(g,x,y+radius+17,h.label||h.id);label(g,x,y+radius+34,observed);
      for(const [j,[k]] of resources.entries()){
        const left=x-34+24*j,top=y+radius+43;
        g.append(svg('rect',{x:left,y:top,width:19,height:4,fill:'var(--ix-rule)'}));
        if(Number.isFinite(h[k]))g.append(svg('rect',{x:left,y:top,width:19*Math.min(100,Math.max(0,h[k]))/100,height:4,fill:h[k]>60?'var(--ix-orange)':'var(--ix-blue)'}));
      }
      select(g,`${h.label||h.id} · ${observed} · ${sum.count}/${ns.length} vnodes comptés${admission} · ${resources.map(([k,l])=>l+' '+fmt(h[k],1)+' %').join(' · ')}`,ns.map(n=>n.label));
      glyphs.append(g);
      const side=Math.ceil(Math.sqrt(ns.length));
      ns.forEach((n,j)=>{
        const nx=x+((j%side)-(Math.min(side,ns.length-Math.floor(j/side)*side)-1)/2)*16,ny=y+(Math.floor(j/side)-(Math.ceil(ns.length/side)-1)/2)*16;
        const point={x:nx,y:ny,r:6};ends.set(n.id,point);if(n.monitor&&(n.state!=='stale'||!ends.has(n.monitor)))ends.set(n.monitor,point);
        const dot=svg('g',{'data-glyph':'grid:'+n.id,'data-x':nx,'data-y':ny});dot.append(svg('circle',{cx:nx,cy:ny,r:6,fill:stale?colors.unknown:colors[n.state]||colors.unknown}));
        select(dot,`${n.label||n.id} · ${h.label||h.id} · ${n.state} · ${fmt(itemSum([n],stale).value)} items · ${fmt(stale?null:n.applied_s??n.estimated_s,1)} ${sim?'requises':'applications'}/s`,[n.label]);glyphs.append(dot);
      });
    });
    let unresolved=0;
    for(const tr of transfers){
      const a=ends.get(tr.source)||hosts.get(tr.source_machine),b=ends.get(tr.target)||hosts.get(tr.target_machine);
      if(!a||!b){unresolved++;continue;}
      const dx=b.x-a.x,dy=b.y-a.y,d=Math.hypot(dx,dy),color=tr.frozen?'var(--ix-orange)':'var(--ix-blue)';
      const path=d>a.r+b.r?`M${a.x+dx*a.r/d} ${a.y+dy*a.r/d} L${b.x-dx*(b.r+2)/d} ${b.y-dy*(b.r+2)/d}`:
        `M${a.x} ${a.y-a.r} Q${a.x+55} ${a.y-55} ${b.x+b.r} ${b.y}`;
      const arrow=svg('path',{d:path,fill:'none',stroke:color,opacity:(tr.stale||stale) ? 0.25 : 0.72,'stroke-dasharray':tr.stale||stale?'4 4':'none','marker-end':'url(#'+(tr.frozen?'fence':'flow')+')','pointer-events':'none'});
      const bundle=svg('g',{});bundle.append(arrow);
      if(root.dataset.playing==='true'&&tr.phase!=='copy-slot'&&!global.matchMedia('(prefers-reduced-motion: reduce)').matches){
        const pulse=svg('circle',{r:2.4,fill:color,'pointer-events':'none'});
        pulse.append(svg('animateMotion',{path,dur:'2s',repeatCount:'indefinite'}));bundle.append(pulse);
      }
      const hit=svg('path',{d:path,fill:'none',stroke:'transparent','stroke-width':8,'pointer-events':'stroke',tabindex:0});
      hit.dataset.tip=transferTip(tr);hit.setAttribute('aria-label',hit.dataset.tip);bundle.prepend(hit);links.append(bundle);
    }
    wrap.append(viewport(root,mode,canvas));
    if(unresolved)wrap.append(el('p',fmt(unresolved)+' transfert(s) avec extrémité inconnue.','note'));
    return wrap;
  }
  // Names encode 96 bits using Indexus's alphabet (internal/tree/codec.go).
  function addressBits(label){
    if(!/^[0-9A-Za-z_-]{16}$/.test(label))return '';
    const alphabet='0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_';
    return [...label].map(c=>alphabet.indexOf(c).toString(2).padStart(6,'0')).join('');
  }
  function touchesNode(tr,n){
    return tr.source===n.id||tr.target===n.id||(n.monitor&&tr.target===n.monitor);
  }
  function commonBits(a,b){
    let i=0;while(i<a.length&&i<b.length&&a[i]===b[i])i++;return i;
  }
  function radialNodes(nodes,layer,ends,canvas,colors,select,sim,stale){
    const leaves=nodes.map(n=>({n,bits:addressBits(n.label)})).sort((a,b)=>
      (a.bits<b.bits?-1:a.bits>b.bits?1:0)||String(a.n.id).localeCompare(String(b.n.id)));
    if(!leaves.length)return;
    const radius=Math.max(150,leaves.length*2.6),size=2*radius+250,mid=size/2;
    canvas.setAttribute('viewBox',`0 0 ${size} ${size}`);
    const branches=svg('g',{fill:'none',stroke:'var(--ix-line)','pointer-events':'none'});
    layer.append(branches);
    const angle=i=>2*Math.PI*i/leaves.length-Math.PI/2;
    const point=(i,r)=>({x:mid+r*Math.cos(angle(i)),y:mid+r*Math.sin(angle(i))});
    function branch(start,end,depth,parent){
      const leaf=end-start===1;
      const here=point((start+end-1)/2,leaf?radius:radius*depth/(depth+2));
      branches.append(svg('line',{x1:parent.x,y1:parent.y,x2:here.x,y2:here.y}));
      if(leaf)return;
      const bit=commonBits(leaves[start].bits,leaves[end-1].bits);
      if(bit===96){
        for(let i=start;i<end;i++)branch(i,i+1,depth+1,here);
        return;
      }
      let split=start+1;
      while(split<end&&leaves[split].bits[bit]===leaves[start].bits[bit])split++;
      branch(start,split,depth+1,here);branch(split,end,depth+1,here);
    }
    if(leaves.every(l=>l.bits))branch(0,leaves.length,0,{x:mid,y:mid});
    for(const [i,{n,bits}] of leaves.entries()){
      const {x,y}=point(i,radius),r=5,g=svg('g',{'data-glyph':'radial:'+n.id,'data-x':x,'data-y':y});
      const pos={x,y,r};ends.set(n.id,pos);if(n.monitor&&(n.state!=='stale'||!ends.has(n.monitor)))ends.set(n.monitor,pos);
      g.append(svg('circle',{cx:x,cy:y,r,fill:stale?colors.unknown:colors[n.state]||colors.unknown}));
      const deg=angle(i)*180/Math.PI,left=Math.cos(angle(i))<0;
      const name=svg('text',{transform:`translate(${x} ${y}) rotate(${left?deg+180:deg})`,
        x:left?-9:9,y:4,'text-anchor':left?'end':'start','font-family':'monospace'});
      name.textContent=n.label;g.append(name);
      const description=`${n.label} · ${n.machine} · ${n.state} · ${fmt(itemSum([n],stale).value)} items · ${fmt(stale?null:n.applied_s??n.estimated_s,1)} ${sim?'requises':'applications'}/s`;
      select(g,description,[n.label],()=>{
        if(!bits||stale)return '';
        const buckets=new Map();
        for(const l of leaves)if(l.bits&&l.bits!==bits&&l.n.state!=='stale'){
          const k=commonBits(bits,l.bits);buckets.set(k,(buckets.get(k)||0)+1);
        }
        return ' · Buckets observables (bits communs × pairs) : '+[...buckets].sort(([a],[b])=>a-b).map(([k,count])=>k+' × '+count).join(', ');
      });
      layer.append(g);
    }
  }
  // Every branching bit of the XOR trie halves the cell's measure,
  // including when the preceding bits form a long common prefix.
  function xorShares(addresses){
    const keys=[...new Set(addresses.filter(a=>addressBits(a)))].map(a=>[addressBits(a),a]).sort();
    const shares=new Map();
    function visit(lo,hi,weight){
      if(hi-lo===1){shares.set(keys[lo][1],weight);return;}
      const bit=commonBits(keys[lo][0],keys[hi-1][0]);
      let split=lo+1;
      while(keys[split][0][bit]==='0')split++;
      visit(lo,split,weight/2);visit(split,hi,weight/2);
    }
    if(keys.length)visit(0,keys.length,1);
    return shares;
  }
  function shareView(nodes,stale,wrap,root){
    const fresh=nodes.filter(n=>n.state!=='stale');
    if(stale||!fresh.length){wrap.append(el('p','Plages indisponibles à cet instant.'));return wrap;}
    const measured=fresh.some(n=>Number.isFinite(n.native_share));
    const projected=xorShares(fresh.map(n=>n.label));
    const entries=fresh.map(n=>({n,share:measured?n.native_share:projected.get(n.label)}))
      .sort((a,b)=>a.n.label.localeCompare(b.n.label));
    if(!measured&&projected.size!==fresh.length){wrap.append(el('p','Adresses distinctes indisponibles pour calculer le placement XOR.'));return wrap;}
    const explanation=measured?'Droits régionaux installés, relevés sur chaque vnode. Les activations ponctuelles en cours de transfert sont exclues.':'Placement XOR théorique entre les vnodes observés. Les droits installés ne figurent pas dans cette capture.';
    const graph=svg('svg',{viewBox:'0 0 240 240',role:'group','aria-label':'Part de l’espace total des adresses'});

    graph.append(svg('circle',{cx:120,cy:120,r:88,stroke:'var(--ix-rule)','stroke-width':35,fill:'none'}));
    const total=entries.reduce((sum,e)=>sum+(e.share||0),0);
    const list=el('div',null,'address-list');

    let offset=0;
    const swatches=['var(--ix-accent)','var(--ix-blue)','var(--ix-green)','var(--ix-orange)','var(--ix-logo)'];
    for(const {n,share} of entries){
      const color=swatches[[...n.label].reduce((h,c)=>(h*31+c.charCodeAt(0))>>>0,0)%swatches.length];
      const text=n.label+' · '+(Number.isFinite(share)?fmt(100*share,4)+' %':'inconnu');
      const button=el('button',text);
      button.style.borderInlineStart='5px solid '+color;
      const hint=text+' · machine '+n.machine+' · '+fmt(n.items)+' items · '+explanation;
      bindPick(root,button,[n.label],hint);list.append(button);
      if(!(share>0)||total>1+1e-12)continue;
      const stroke={fill:'none',stroke:color,'stroke-width':35};
      const at=f=>[120+88*Math.sin(2*Math.PI*f),120-88*Math.cos(2*Math.PI*f)].join(' ');
      const arc=share===1?svg('circle',{...stroke,cx:120,cy:120,r:88}):
        svg('path',{...stroke,d:`M${at(offset)} A88 88 0 ${share>.5?1:0} 1 ${at(offset+share)}`});
      const title=svg('title',{});title.textContent=text;arc.append(title);bindPick(root,arc,[n.label],hint);graph.append(arc);
      offset+=share;
    }
    if(total>1+1e-12)wrap.append(el('p','Les relevés locaux dépassent ensemble 100 %. Les parts individuelles figurent ci-dessous.'));
    else {
      const center=svg('text',{x:120,y:120,'text-anchor':'middle'});center.textContent=measured?'Droits installés':'Placement XOR';
      center.dataset.tip=explanation;graph.append(center);wrap.append(viewport(root,'shares',graph));
    }
    if(total>1+1e-12||entries.some(e=>!(e.share>0))) {const details=el('details');details.append(el('summary','Adresses individuelles'),list);wrap.append(details);}
    return wrap;
  }
  // Camera state belongs to the view, so changing the replay time keeps it.
  const cameras=new WeakMap();
  function viewport(root,mode,graph,orbit){
    const frame=el('div',null,'network-frame'),bar=el('div',null,'camera-bar');
    frame.append(graph,bar);
    const base=graph.getAttribute('viewBox').split(' ').map(Number);
    const [, ,w,h]=base;
    graph.setAttribute('tabindex','0');
    graph.setAttribute('aria-description','Zoom avec + et − ou Ctrl + molette. Flèches pour naviguer.');
    let modes=cameras.get(root);
    if(!modes){modes=new Map();cameras.set(root,modes);}
    const initial={zoom:1,x:.5,y:.5,yaw:0,pitch:0};
    const cam=modes.get(mode)||{...initial};modes.set(mode,cam);
    const level=el('output');
    function paint(){
      const width=w/cam.zoom,height=h/cam.zoom;
      graph.setAttribute('viewBox',[cam.x*w-width/2,cam.y*h-height/2,width,height].join(' '));
      level.textContent=Math.round(cam.zoom*100)+' %';
      if(orbit)orbit(cam);
    }
    function zoom(f,event){
      const prior=cam.zoom;
      cam.zoom=Math.max(1,Math.min(12,prior*f));
      if(event){
        const matrix=graph.getScreenCTM();
        if(matrix){
          const p=new DOMPoint(event.clientX,event.clientY).matrixTransform(matrix.inverse());
          cam.x+=(p.x/w-cam.x)*(1-prior/cam.zoom);cam.y+=(p.y/h-cam.y)*(1-prior/cam.zoom);
        }
      }
      if(cam.zoom===1){cam.x=.5;cam.y=.5;}
      paint();
    }
    for(const [name,label,act] of [['−','Réduire',()=>zoom(1/1.4)],['+','Agrandir',()=>zoom(1.4)],['Recentrer','Rétablir la vue',()=>{Object.assign(cam,initial);paint();}]]){
      const b=el('button',name);b.type='button';b.setAttribute('aria-label',label);b.onclick=act;bar.append(b);
    }
    bar.append(level);
    graph.addEventListener('keydown',e=>{
      if(e.target!==graph)return;
      if(e.key==='+'||e.key==='=')zoom(1.4);
      else if(e.key==='-')zoom(1/1.4);
      else if(e.key==='Home'){Object.assign(cam,initial);paint();}
      else if(['ArrowLeft','ArrowRight','ArrowUp','ArrowDown'].includes(e.key)){
        const dx=Number(e.key==='ArrowRight')-Number(e.key==='ArrowLeft');
        const dy=Number(e.key==='ArrowDown')-Number(e.key==='ArrowUp');
        if(orbit){cam.yaw+=dx*.12;cam.pitch=Math.max(-1.55,Math.min(1.55,cam.pitch+dy*.12));}
        else{cam.x+=dx*.08/cam.zoom;cam.y+=dy*.08/cam.zoom;}
        paint();
      }else return;
      e.preventDefault();
    });
    graph.addEventListener('wheel',e=>{
      if(!e.ctrlKey&&!e.metaKey)return;
      e.preventDefault();zoom(Math.exp(-Math.sign(e.deltaY)*.15),e);
    },{passive:false});
    let drag=null,moved=false;
    graph.addEventListener('pointerdown',e=>{
      if(e.button!==0)return;
      moved=false;drag={x:e.clientX,y:e.clientY,id:e.pointerId};
    });
    graph.addEventListener('pointermove',e=>{
      if(!drag||drag.id!==e.pointerId)return;
      const dx=e.clientX-drag.x,dy=e.clientY-drag.y;
      if(!moved&&Math.hypot(dx,dy)<4)return;
      moved=true;graph.setPointerCapture(e.pointerId);drag.x=e.clientX;drag.y=e.clientY;
      if(orbit&&!e.shiftKey){
        cam.yaw+=dx*.009;cam.pitch=Math.max(-1.55,Math.min(1.55,cam.pitch+dy*.009));
      }else{
        const scale=graph.getScreenCTM()?.a||1;
        cam.x-=dx/(scale*w);cam.y-=dy/(scale*h);
      }
      paint();
    });
    for(const ev of ['pointerup','pointercancel','pointerleave'])graph.addEventListener(ev,e=>{
      if(ev!=='pointerleave'||!graph.hasPointerCapture(e.pointerId))drag=null;
    });
    graph.addEventListener('click',e=>{if(moved){e.stopPropagation();moved=false;}},true);
    paint();return frame;
  }

  function spaceView(root,nodes,transfers,stale,wrap){
    const graph=svg('svg',{viewBox:'0 0 480 480','aria-label':'Réseau de vnodes en trois dimensions'});
    const links=svg('g',{'pointer-events':'none'}),dots=svg('g',{});
    graph.append(links,dots);
    const members=[...nodes].sort((a,b)=>String(a.label).localeCompare(String(b.label)));
    const edges=[],byID=new Map(),byMonitor=new Map();
    const locations=members.map(n=>{
      // Interleave the first 30 address bits into latitude and longitude.
      let bits=addressBits(n.label);
      if(!bits){
        let hash=2166136261;for(const c of String(n.id))hash=Math.imul(hash^c.charCodeAt(0),16777619);
        bits=(hash>>>0).toString(2).padStart(32,'0');
      }
      let u=0,v=0;
      for(let i=0;i<30;i+=2){u=2*u+Number(bits[i]);v=2*v+Number(bits[i+1]);}
      const lat=(u+.5)/16384-1,lon=(v+.5)*Math.PI/16384,r=Math.sqrt(1-lat*lat);
      const dot=svg('g',{role:'button',tabindex:0,'aria-label':n.label});
      const circle=svg('circle',{fill:stale||n.state==='stale'?'var(--ix-line)':n.state==='ready'?'var(--ix-green)':'var(--ix-blue)'});
      const title=svg('title',{});title.textContent=n.label+' · '+n.machine;dot.append(circle,title);
      const loc={n,bits,x:r*Math.cos(lon),y:lat,z:r*Math.sin(lon),dot,circle};
      byID.set(n.id,loc);if(n.monitor&&(!byMonitor.has(n.monitor)||n.state!=='stale'))byMonitor.set(n.monitor,loc);
      bindPick(root,dot,[n.label],n.label+' · '+n.machine+' · '+n.state+' · '+fmt(n.items)+' items · '+fmt(n.applied_s??n.estimated_s,1)+'/s');
      return loc;
    });
    function edge(a,b,color,transfer){
      if(!a||!b||a===b)return;
      const line=svg('line',{stroke:color,'stroke-width':transfer?1.9:1});
      if(transfer){line.setAttribute('marker-end','url(#space-tip)');line.setAttribute('pointer-events','stroke');line.setAttribute('tabindex',0);line.dataset.tip=transferTip(transfer);}
      edges.push({a,b,line,transfer});links.append(line);
    }
    const defs=svg('defs',{}),tip=svg('marker',{id:'space-tip',viewBox:'0 0 10 10',refX:10,refY:5,markerWidth:5,markerHeight:5,orient:'auto'});
    tip.append(svg('path',{d:'M0 0 L10 5 L0 10Z',fill:'context-stroke'}));defs.append(tip);graph.prepend(defs);
    // One inferred link per trie bifurcation; never draw it as measured traffic.
    const sorted=locations.filter(p=>addressBits(p.n.label)).sort((a,b)=>a.bits.localeCompare(b.bits));
    function connect(lo,hi){
      if(hi-lo<2)return;
      const k=commonBits(sorted[lo].bits,sorted[hi-1].bits);if(k===96)return;
      let m=lo+1;while(sorted[m].bits[k]==='0')m++;
      edge(sorted[lo],sorted[m],'var(--ix-line)',false);connect(lo,m);connect(m,hi);
    }
    connect(0,sorted.length);
    for(const tr of transfers)edge(byID.get(tr.source),byID.get(tr.target)||byMonitor.get(tr.target),tr.frozen?'var(--ix-orange)':'var(--ix-blue)',tr);
    function paint(cam){
      const cy=Math.cos(cam.yaw),sy=Math.sin(cam.yaw),cp=Math.cos(cam.pitch),sp=Math.sin(cam.pitch);
      for(const p of locations){
        const x=p.x*cy+p.z*sy,z=p.z*cy-p.x*sy;
        p.depth=z*cp+p.y*sp;
        const scale=182/(1-p.depth*.21);
        p.px=240+x*scale;p.py=240+(p.y*cp-z*sp)*scale;
        const selected=scopes.get(root).ids.has(p.n.label);
        p.dot.setAttribute('transform',`translate(${p.px} ${p.py})`);
        p.circle.setAttribute('r',selected?7:4+p.depth*1.4);
        p.circle.setAttribute('stroke',selected?'var(--ix-accent)':'none');
        p.dot.setAttribute('opacity',selected?1:.72+.27*p.depth);

      }
      for(const {a,b,line,transfer} of edges){
        const dx=b.px-a.px,dy=b.py-a.py;
        const inset=transfer?Math.min(.45,7/Math.max(1,Math.hypot(dx,dy))):0;
        const attrs={x1:a.px,y1:a.py,x2:b.px-inset*dx,y2:b.py-inset*dy,opacity:transfer ? .9 : .25};
        for(const [k,v] of Object.entries(attrs))line.setAttribute(k,v);
      }
      dots.append(...[...locations].sort((a,b)=>a.depth-b.depth).map(p=>p.dot));
    }
    graph.dataset.tip='Glisser pour tourner · Gris : voisinage de préfixes · Flèches : transferts observés';
    wrap.append(viewport(root,'space',graph,paint));
    return wrap;
  }
  function pressure(h){return Math.max(...['cpu','ram','disk','io'].map(k=>Number.isFinite(h[k])?h[k]:-1));}
  function exploreChart(points,title,series,stepped){
    const modal=el('dialog',null,'ix-dashboard chart-dialog');
    modal.setAttribute('aria-label',title);
    const header=el('div',null,'camera-bar'),body=el('div'),level=el('output');
    let start=0,count=points.length;
    function update(){
      count=Math.max(Math.min(2,points.length),Math.min(points.length,count));
      start=Math.max(0,Math.min(points.length-count,start));
      body.replaceChildren(chart(points.slice(start,start+count),title,series,stepped,true));
      level.textContent=fmt(points[start].x??points[start].t)+' – '+fmt(points[start+count-1].x??points[start+count-1].t)+' s';
    }
    for(const [text,label,action] of [
      ['−','Élargir la période',()=>{const end=start+count;count=Math.min(points.length,count*2);start=end-count;}],
      ['+','Resserrer la période',()=>{const end=start+count;count=Math.max(2,Math.ceil(count/2));start=end-count;}],
      ['←','Période précédente',()=>{start-=Math.ceil(count/2);}],
      ['→','Période suivante',()=>{start+=Math.ceil(count/2);}],
      ['Fermer','Fermer',()=>modal.close()]]){
      const b=el('button',text);b.type='button';b.setAttribute('aria-label',label);b.onclick=()=>{action();if(modal.open)update();};header.append(b);
    }
    header.append(level);modal.append(header,body);document.body.append(modal);
    modal.onclose=()=>modal.remove();modal.showModal();update();
  }
  function chart(points,title,series,stepped=false,expanded=false){
    const figure=el('figure'),caption=el('figcaption',title),legend=el('div',null,'legend');figure.append(caption,legend);
    const values=series.flatMap(([k])=>points.map(p=>p[k]).filter(Number.isFinite));
    if(!values.length){figure.append(el('p','Non mesuré.','note'));return figure;}
    if(!expanded){
      const expand=el('button','⤢','expand-chart');expand.type='button';expand.setAttribute('aria-label','Explorer '+title);
      expand.onclick=()=>exploreChart(points,title,series,stepped);
      caption.append(expand);
    }
    const top=Math.max(...values,1)*1.08;
    // Scale the data surface to its actual box; the ticks remain native-size text.
    const field=el('div',null,'chart-field'),surface=el('div',null,'chart-surface');
    field.append(surface);figure.append(field);surface.append(legend);
    const first=points[0].x??points[0].t??0,last=Math.max(first+1,points.at(-1).x??points.at(-1).t??0);
    const sx=p=>((p.x??p.t??0)-first)/(last-first)*100,sy=v=>(1-v/top)*100;
    const g=svg('svg',{viewBox:'0 0 100 100',preserveAspectRatio:'none',role:'img','aria-label':title});
    surface.append(g);
    for(const f of [0,.5,1]){
      g.append(svg('line',{x1:0,x2:100,y1:sy(top*f),y2:sy(top*f),class:'gridline','vector-effect':'non-scaling-stroke'}));
      const label=el('span',fmt(top*f,1),'y-tick');label.style.top=sy(top*f)+'%';surface.append(label);
    }
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
      for(const segment of path.split('|').filter(s=>s.trim()))g.append(svg('polyline',{points:segment,fill:'none',stroke:color,'stroke-width':2.2,'vector-effect':'non-scaling-stroke','stroke-linejoin':'round'}));
    }
    for(const [side,value] of [['left',first],['right',last]]){
      const tick=el('span',fmt(value)+' s','x-tick');tick.style[side]='0';surface.append(tick);
    }
    g.addEventListener('pointermove',e=>{
      const m=g.getScreenCTM();if(!m)return;
      const pos=new DOMPoint(e.clientX,e.clientY).matrixTransform(m.inverse());
      const time=first+pos.x/100*(last-first);
      let nearest=points[0];
      for(const p of points)if(Math.abs((p.x??p.t)-time)<Math.abs((nearest.x??nearest.t)-time))nearest=p;
      g.dataset.tip=[fmt(nearest.x??nearest.t)+' s',...series.filter(([key])=>Number.isFinite(nearest[key])).map(([key,name])=>name+' '+fmt(nearest[key],2))].join(' · ');
    });
    installTips(figure);return figure;
  }
  global.IndexusDashboard={render,selectedPoint,xorShares,growthMetrics,itemSum,addressBits,commonBits,hostSpread,departedTransfers};
})(typeof window==='undefined'?globalThis:window);
