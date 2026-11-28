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
      const stock=itemSum(p.fleet?.vnodes||[],p.metrics_stale);
      out.stock=stock.value??(!p.metrics_stale&&!p.fleet?.vnodes?.length?p.items:null);
      out.coverage=stock;
      return Object.assign(out,growthMetrics(p),hostSpread(p));
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
    const stats=[['Demande /s',p.load_stale?null:p.offered],['ACK client /s',p.load_stale?null:p.acked],['Items observés',p.stock],['Vnodes comptés / inventoriés',p.coverage.total?fmt(p.coverage.count)+' / '+fmt(p.coverage.total):'—'],['Vnodes prêts / observés',fmt(p.ready_count)+' / '+fmt(p.observed_count)],['Machines observées',p.machine_count],['Chaudes / démarrage',`${hasReadiness?warm:'?'} / ${sim?boot:'?'}`],['Réceptions admissibles',machines.some(h=>h.ready_offer!=null)?machines.filter(h=>h.ready_offer).length:null],['Machines observées > 60 %',hasResources?bad:null],['Transferts',p.metrics_stale?null:p.open??transfers.length],['p99 écriture, ms',p.load_stale?null:p.p99_ms]];
    for(const [label,value] of stats){const t=el('div',null,'tile');t.append(el('span',label),el('strong',typeof value==='string'?value:fmt(value,1)));tiles.append(t);}root.append(tiles);
    const charts=el('div',null,'charts'),unmeasured=[];
    for(const [title,series,stepped] of [
      ['Écritures /s',[['offered','demandées'],['acked','ACK client'],['estimated_s','besoin réparti (simulation)']]],
      ['Lectures client /s',[['reads_s','tentatives'],['read_failed_s','échecs']]],
      ['Latence écriture, ms',[['p50_ms','p50'],['p95_ms','p95'],['p99_ms','p99']]],
      ['Items observés dans le réseau',[['stock','somme des compteurs disponibles']]],
      ['Croissance du réseau',[['ready_count','vnodes prêts'],['machine_count','machines observées']],true],
      ['Écritures/s par machine active',[['hostRateLow','minimum'],['hostRateMedian','médiane'],['hostRateHigh','maximum']]],
      ['Items par machine active',[['hostItemsLow','minimum'],['hostItemsMedian','médiane'],['hostItemsHigh','maximum']]],
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
    root.append(el('h2','Réseau au temps '+fmt(p.x??p.t)+' s'));
    root.append(el('p',`${unknown} machine(s) avec ressources inconnues. Les flèches représentent les transferts signalés à cet instant, du donneur vers le receveur. Sélectionner un rond pour ses mesures. La somme des compteurs disponibles peut être partielle et n’est pas un audit des clés uniques.`, 'note'));
    const detail=el('p','Sélectionner un vnode.','detail');detail.setAttribute('aria-live','polite');
    const choices=el('div',null,'legend'), plot=el('div');
    const active=p.metrics_stale?[]:transfers.filter(tr=>!tr.stale);
    const unknownLinks=transfers.length-active.length;
    const departures=departedTransfers(points[index-1],p);
    choices.setAttribute('aria-label','Vue du réseau');
    for(const [key,title] of [['hosts','Par machine'],['radial','Vnodes indépendants']]){
      const b=el('button',title);b.type='button';b.dataset.mode=key;
      b.onclick=()=>{
        root.dataset.mode=key;
        if(global.location.hash===''||global.location.hash==='#vnodes'){
          const url=new URL(global.location.href);url.hash=key==='radial'?'vnodes':'';
          global.history.replaceState(global.history.state,'',url);
        }
        drawTopology();
      };choices.append(b);
    }
    function drawTopology(){
      const mode=root.dataset.mode||(global.location.hash==='#vnodes'?'radial':'hosts');
      for(const b of choices.children)b.setAttribute('aria-pressed',String(b.dataset.mode===mode));
      detail.textContent='Sélectionner un vnode.';
      plot.replaceChildren(topology(machines,nodes,active,detail,sim,p.metrics_stale,root.clientWidth,mode));
    }
    drawTopology();
    root.append(choices,detail,plot,el('h2','Transferts et délégations'));
    if(!p.metrics_stale)root.append(el('p',fmt(active.length)+' opération(s) signalée(s)'+(departures==null?'':' · '+fmt(departures)+' sortie(s) de la liste depuis le relevé précédent')+'.','note'));
    if(unknownLinks)root.append(el('p',fmt(unknownLinks)+' dernière(s) observation(s) périmée(s), masquée(s) sur le graphe. Clôture inconnue.','note'));
    const list=el('div',null,'transfers');
    if(!transfers.length)list.append(el('p',p.metrics_stale?'État des transferts inconnu.':'Aucun transfert observé à cet instant.'));
    for(const tr of transfers){
      const row=el('div',null,'transfer'+(tr.frozen?' hot':''));
      row.append(el('strong',`${tr.source_machine||'?'} → ${tr.target_machine||'destination inconnue'}`),
        el('span',`${tr.phase||'copie'}${tr.stale?' · état périmé':''} · âge ${fmt(tr.age_s,1)} s · ${tr.progress==null?'progression non mesurée':fmt(tr.progress*100,1)+' % copiés'}${tr.share==null?'':' · '+fmt(tr.share*100,1)+' % de charge déplacée'}${tr.frozen?' · fence franchie':''}`));list.append(row);
    }
    root.append(list);
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
  function topology(machines,nodes,transfers,detail,sim,stale,width,mode){
    const wrap=el('div',null,'topology');
    if(!machines.length){wrap.append(el('p','Aucun inventaire machine enregistré.'));return wrap;}
    const colors={ready:'#16856a',receiving:'#347cca',joining:'#347cca',unknown:'#888'};
    wrap.append(el('p','Vnode vert = prêt · bleu = en cours de join · gris = inconnu ou périmé. Contour orange = ressource au-dessus de 60 %. Flèche orange = opération ayant franchi une fence. Le gel d’une écriture peut déjà être terminé.','note'));
    const canvas=svg('svg',{'aria-label':'Machines, vnodes et sens des transferts',role:'group'});
    const defs=svg('defs',{});
    for(const [id,color] of [['flow','#588fc2'],['fence','#ce663e']]){
      const marker=svg('marker',{id,viewBox:'0 0 10 10',refX:9,refY:5,markerWidth:6,markerHeight:6,orient:'auto'});
      marker.append(svg('path',{d:'M0 0 L10 5 L0 10 Z',fill:color}));defs.append(marker);
    }
    const links=svg('g',{}),glyphs=svg('g',{});
    canvas.append(defs,glyphs,links);
    const hosts=new Map(),ends=new Map(),ordered=[...machines].sort((a,b)=>String(a.id).localeCompare(String(b.id),undefined,{numeric:true}));
    const members=new Map(ordered.map(h=>[h.id,[]]));
    for(const n of nodes)members.get(n.machine)?.push(n);
    const widest=Math.max(1,...Array.from(members.values(),ns=>Math.ceil(Math.sqrt(ns.length))));
    const radius=Math.max(38,widest*11),cell=radius*2+62,step=cell+36,cols=Math.max(1,Math.min(ordered.length,Math.floor(width/cell)));
    canvas.setAttribute('viewBox',`0 0 ${cols*cell} ${Math.ceil(ordered.length/cols)*step}`);
    function label(group,x,y,value){
      const t=svg('text',{x,y,'text-anchor':'middle'});t.textContent=value;group.append(t);
    }
    const edges=[];
    const all=el('button','Afficher tous les transferts');
    all.type='button';all.hidden=true;
    const reset=()=>{for(const edge of edges)edge.path.style.display='';all.hidden=true;};
    all.onclick=()=>{reset();detail.textContent='Sélectionner un vnode.';};
    wrap.append(all);
    function select(group,description,matches,extra){
      group.setAttribute('tabindex',0);group.setAttribute('role','button');group.setAttribute('aria-label',description);
      const title=svg('title',{});title.textContent=description;group.append(title);
      group.onclick=()=>{
        const related=edges.filter(edge=>matches(edge.tr));
        for(const edge of edges)edge.path.style.display=matches(edge.tr)?'':'none';
        all.hidden=edges.length===0;
        detail.textContent=description+(extra?extra():'')+' · '+fmt(related.length)+' lien(s) de transfert visible(s)';
      };
      group.onkeydown=e=>{if(e.key==='Enter'||e.key===' '){e.preventDefault();group.onclick();}};
    }
    if(mode==='radial'){
      canvas.setAttribute('class','radial');
      radialNodes(nodes,glyphs,ends,canvas,colors,select,sim,stale);
    }else ordered.forEach((h,i)=>{
      const ns=members.get(h.id),x=(i%cols+.5)*cell,y=Math.floor(i/cols)*step+radius+12;
      hosts.set(h.id,{x,y,r:radius});
      const g=svg('g',{}),sum=itemSum(ns,stale);
      g.append(svg('circle',{cx:x,cy:y,r:radius,fill:'#8881',stroke:pressure(h)>60?'#ce663e':'#888','stroke-width':2}));
      const observed=fmt(sum.value)+' items';
      const resources=[['cpu','CPU'],['ram','RAM'],['disk','Disque']];
      label(g,x,y+radius+17,h.label||h.id);label(g,x,y+radius+34,observed);
      for(const [j,[k]] of resources.entries()){
        const left=x-34+24*j,top=y+radius+43;
        g.append(svg('rect',{x:left,y:top,width:19,height:4,fill:'#8884'}));
        if(Number.isFinite(h[k]))g.append(svg('rect',{x:left,y:top,width:19*Math.min(100,Math.max(0,h[k]))/100,height:4,fill:h[k]>60?'#ce663e':'#347cca'}));
      }
      select(g,`${h.label||h.id} · ${observed} · ${sum.count}/${ns.length} vnodes comptés · ${resources.map(([k,l])=>l+' '+fmt(h[k],1)+' %').join(' · ')}`,tr=>tr.source_machine===h.id||tr.target_machine===h.id);
      glyphs.append(g);
      const side=Math.ceil(Math.sqrt(ns.length));
      ns.forEach((n,j)=>{
        const nx=x+((j%side)-(Math.min(side,ns.length-Math.floor(j/side)*side)-1)/2)*16,ny=y+(Math.floor(j/side)-(Math.ceil(ns.length/side)-1)/2)*16;
        const point={x:nx,y:ny,r:6};ends.set(n.id,point);if(n.monitor&&(n.state!=='stale'||!ends.has(n.monitor)))ends.set(n.monitor,point);
        const dot=svg('g',{});dot.append(svg('circle',{cx:nx,cy:ny,r:6,fill:stale?colors.unknown:colors[n.state]||colors.unknown}));
        select(dot,`${n.label||n.id} · ${h.label||h.id} · ${n.state} · ${fmt(itemSum([n],stale).value)} items · ${fmt(stale?null:n.applied_s??n.estimated_s,1)} ${sim?'requises':'applications'}/s`,tr=>touchesNode(tr,n));glyphs.append(dot);
      });
    });
    let unresolved=0;
    for(const tr of transfers){
      const a=ends.get(tr.source)||hosts.get(tr.source_machine),b=ends.get(tr.target)||hosts.get(tr.target_machine);
      if(!a||!b){unresolved++;continue;}
      const dx=b.x-a.x,dy=b.y-a.y,d=Math.hypot(dx,dy),color=tr.frozen?'#ce663e':'#588fc2';
      const path=d>a.r+b.r?`M${a.x+dx*a.r/d} ${a.y+dy*a.r/d} L${b.x-dx*(b.r+2)/d} ${b.y-dy*(b.r+2)/d}`:
        `M${a.x} ${a.y-a.r} Q${a.x+55} ${a.y-55} ${b.x+b.r} ${b.y}`;
      const arrow=svg('path',{d:path,fill:'none',stroke:color,opacity:(tr.stale||stale) ? 0.25 : 0.72,'stroke-dasharray':tr.stale||stale?'4 4':'none','marker-end':'url(#'+(tr.frozen?'fence':'flow')+')','pointer-events':'none'});
      edges.push({tr,path:arrow});links.append(arrow);
    }
    if(mode==='radial')wrap.append(el('p',sim?'Disposition circulaire des vnodes simulés.':'Branches grises : préfixes binaires communs aux adresses. Sélectionner un vnode pour lire ses buckets parmi les adresses observées.','note'));
    wrap.append(canvas);
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
    const radius=Math.max(150,leaves.length*2.6),size=2*radius+28,mid=size/2;
    canvas.setAttribute('viewBox',`0 0 ${size} ${size}`);
    const branches=svg('g',{fill:'none',stroke:'#8886','pointer-events':'none'});
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
      const {x,y}=point(i,radius),r=5,g=svg('g',{});
      const pos={x,y,r};ends.set(n.id,pos);if(n.monitor&&(n.state!=='stale'||!ends.has(n.monitor)))ends.set(n.monitor,pos);
      g.append(svg('circle',{cx:x,cy:y,r,fill:stale?colors.unknown:colors[n.state]||colors.unknown}));
      const description=`${n.label} · ${n.machine} · ${n.state} · ${fmt(itemSum([n],stale).value)} items · ${fmt(stale?null:n.applied_s??n.estimated_s,1)} ${sim?'requises':'applications'}/s`;
      select(g,description,tr=>touchesNode(tr,n),()=>{
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
  global.IndexusDashboard={render,growthMetrics,itemSum,addressBits,commonBits,hostSpread,departedTransfers};
})(typeof window==='undefined'?globalThis:window);
