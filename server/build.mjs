/**
 * build.mjs — bundle src/*.js into a single _worker.js for Cloudflare Workers.
 * Simple ESM concatenation: relative imports are stripped (top-level names
 * share bundle scope); platform imports (cloudflare:*) are hoisted to the top.
 */
import { readFileSync, writeFileSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const srcDir = join(here, 'src');

const order = ['socket.js', 'crypto.js', 'protocol.js', 'exits.js', 'proxyip.js', 'race.js', 'router.js', 'session.js', 'index.js'];

const platformImports = new Set();

let out = `/**
 * _worker.js — NetMaster protocol v2 (mux over WebSocket, Session DO).
 *
 * GENERATED from src/ by build.mjs — edit src/, not this file.
 * Modules: ${order.join(', ')}
 */
`;

for (const file of order) {
  let code = readFileSync(join(srcDir, file), 'utf8');
  // hoist platform imports, strip relative ones
  code = code.replace(/^import\s+([^;]*?)from\s+['"]([^'"]+)['"];?\s*$/gm, (m, names, from) => {
    if (from.startsWith('cloudflare:')) {
      platformImports.add(m.trim());
      return '';
    }
    return ''; // relative import: concatenated scope shares all top-level names
  });
  // strip export default { ... }  ->  const __default = { ... }
  code = code.replace(/^export\s+default\s+/gm, 'const __default = ');
  // strip leading export on declarations (class SessionDO stays bundle-internal)
  code = code.replace(/^export\s+(async\s+function|function|const|class|let)/gm, '$1');
  out += `\n// ===== src/${file} =====\n${code}\n`;
}

out += `\n// ===== entry =====\n`;
for (const imp of platformImports) out += imp + '\n';
out += `export default __default;\n`;
// Durable Object classes must be exported from the worker module.
out += `export { SessionDO, RouterDO };\n`;

writeFileSync(join(here, '_worker.js'), out, 'utf8');
console.log(`built _worker.js (${out.length} bytes) from ${order.length} modules`);
