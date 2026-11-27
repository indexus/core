// Run inside a built quickstart image; stdout is a metadata inventory, not legal clearance.
import fs from 'node:fs';
import path from 'node:path';
const roots = ['/opt/indexus/sdk', '/opt/indexus/dashboard', '/usr/local/lib/node_modules'];
const seen = new Set(), packages = [];
function walk(dir) {
  let real;
  try { real = fs.realpathSync(dir); } catch { return; }
  if (seen.has(real)) return;
  seen.add(real);
  let entries;
  try { entries = fs.readdirSync(real, { withFileTypes:true }); } catch { return; }
  const file = path.join(real, 'package.json');
  if (fs.existsSync(file)) {
    try {
      const data = JSON.parse(fs.readFileSync(file,'utf8'));
      if (data.name && data.version) {
        const texts = entries.filter(e => e.isFile() && /^(licen[cs]e|copying|notice|copyright)([.\-_].*)?$/i.test(e.name)).map(e => e.name);
        packages.push({metadataType: /\/(?:test|tests|fixtures)\//.test(real) ? 'test-fixture' : 'package', name:data.name, version:data.version, declaredLicense:data.license ?? data.licenses ?? null, directory:real, noticeFiles:texts});
      }
    } catch {}
  }
  for (const entry of entries) {
    if (entry.isDirectory() || entry.isSymbolicLink()) walk(path.join(real,entry.name));
  }
}
for (const root of roots) walk(root);
packages.sort((a,b)=>a.name.localeCompare(b.name)||a.directory.localeCompare(b.directory));
const counts = {};
for (const p of packages) {
  const label=typeof p.declaredLicense==='string'?p.declaredLicense:JSON.stringify(p.declaredLicense);
  counts[label]=(counts[label]??0)+1;
}
const noticeRoot = '/opt/indexus/notices';
const noticeBundles = fs.existsSync(noticeRoot) ? fs.readdirSync(noticeRoot).filter(name => fs.existsSync(path.join(noticeRoot,name,'LICENSE'))) : [];
console.log(JSON.stringify({noticeBundles, scope:'Installed JavaScript package metadata in runtime SDK, gateway and global Node tools; not a complete image SBOM or legal clearance. Bundled browser assets, Go/Node binaries and OS notices need separate checks.', roots, metadataCount:packages.length, packageCount:packages.filter(p=>p.metadataType==='package').length, fixtureMetadataCount:packages.filter(p=>p.metadataType==='test-fixture').length, declaredLicenseCounts:counts, missingDeclaration:packages.filter(p=>!p.declaredLicense).map(p=>p.directory), noTopLevelNotice:packages.filter(p=>!p.noticeFiles.length).map(p=>({name:p.name,version:p.version,directory:p.directory})), packages},null,2));
