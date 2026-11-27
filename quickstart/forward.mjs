// Same-origin streaming HTTP proxy plus a read-only local demo inventory.
// Monitoring stays on loopback. The inventory is advisory, never routing authority.
import http from 'node:http';
import { readdir } from 'node:fs/promises';
const [listen='0.0.0.0:5174',target='127.0.0.1:3847']=process.argv.slice(2);
const split=value=>[value.slice(0,value.lastIndexOf(':')),Number(value.slice(value.lastIndexOf(':')+1))];
const [host,port]=split(listen),[targetHost,targetPort]=split(target);
let snapshot,pending,expires=0;
async function inventory(){
 if(snapshot&&Date.now()<expires)return snapshot;
 return pending??=(async()=>{
  const dirs=await readdir(process.env.INDEXUS_DATA || '/data',{withFileTypes:true});
  const slots=dirs.filter(d=>d.isDirectory() && /^node[1-9][0-9]*$/.test(d.name))
    .map(d=>Number(d.name.slice(4))).sort((a,b)=>a-b);
  const nodes=await Promise.all(slots.map(async slot=>{
   const mon=19030+slot;
   try{
    const read=async path=>{const response=await fetch(`http://127.0.0.1:${mon}/${path}`,{signal:AbortSignal.timeout(4000)});if(!response.ok)throw Error('unavailable');return response.json();};
    const [status,zones]=await Promise.all([read('status'),read('ownership')]);
    return {label:`N${slot}`,name:status.name,available:true,collections:Object.fromEntries(Object.entries(zones).map(([collection,locations])=>[collection,Object.keys(locations)]))};
   }catch{return {label:`N${slot}`,available:false,collections:{}};}
  }));
  // A transient monitoring failure must not hide a recovered node for a minute.
  snapshot={observedAt:new Date().toISOString(),scope:'Local processes on one Docker host; stored-zone inventory, not a failover guarantee',nodes};expires=Date.now()+(nodes.every(node=>node.available)?60000:5000);return snapshot;
 })().finally(()=>{pending=null;});
}
const server=http.createServer(async(req,res)=>{
 if(req.url.split('?')[0]==='/api/demo/mesh'){
  if(req.method!=='GET'){res.writeHead(405,{'Allow':'GET'});res.end();return;}
  try{const body=JSON.stringify(await inventory());res.writeHead(200,{'Content-Type':'application/json','Cache-Control':'no-store'});res.end(body);}catch{res.writeHead(503);res.end();}
  return;
 }
 const upstream=http.request({hostname:targetHost,port:targetPort,path:req.url,method:req.method,headers:req.headers},response=>{
  res.writeHead(response.statusCode,response.headers);response.pipe(res);
 });
 upstream.on('error',()=>{if(!res.headersSent)res.writeHead(502);res.end();});
 req.on('aborted',()=>upstream.destroy());res.on('close',()=>{if(!res.writableFinished)upstream.destroy();});
 req.pipe(upstream);
});
server.listen(port,host,()=>console.log(`forwarding ${listen} -> ${target}`));
for(const signal of ['SIGTERM','SIGINT'])process.on(signal,()=>server.close(()=>process.exit(0)));
