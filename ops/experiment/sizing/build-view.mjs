// The interactive view embeds the same source as the CLI sweep, not a second model.
import {readFile,writeFile} from 'node:fs/promises';
import {resolve} from 'node:path';
const source=await readFile(new URL('./model.mjs',import.meta.url),'utf8');
const view=await readFile(new URL('./view.html',import.meta.url),'utf8');
if(source.includes('</script'))throw Error('Unsafe script terminator in model source');
const output=resolve(process.argv[2]||'sizing.html');
const js=await readFile(new URL('../dashboard/view.js',import.meta.url),'utf8');
const css=(await Promise.all(['palette.css','view.css'].map(name=>readFile(new URL('../dashboard/'+name,import.meta.url),'utf8')))).join('\n');
const fragment=view.replace('/*__SIZING_MODEL__*/',source.replace(/^export /gm,'')).replace('/*__DASHBOARD_JS__*/',js).replace('/*__DASHBOARD_CSS__*/',css);
const page=process.argv.includes('--page')?await readFile(new URL('./page.html',import.meta.url),'utf8'):null;
await writeFile(output,page?page.replace('<!--__SIZING_VIEW__-->',fragment):fragment);
console.log(output);
