/**
 * build.mjs — bundle src/*.js into a single _worker.js for Cloudflare Workers.
 * Simple ESM concatenation: strip import/export statements, rename nothing
 * (module top-level names are unique across our files).
 */
import { readFileSync, writeFileSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const srcDir = join(here, 'src');

const order = ['util.js', 'crypto.js', 'protocol.js', 'relayprobe.js', 'forward.js', 'index.js'];

let out = `/**
 * _worker.js — NetMaster 内部协议（mux over WebSocket）转发器。
 *
 * GENERATED from src/ by build.mjs — edit src/, not this file.
 * Modules: ${order.join(', ')}
 */
`;

for (const file of order) {
	let code = readFileSync(join(srcDir, file), 'utf8');
	// strip import ... from '...';
	code = code.replace(/^import\s+[^;]*?from\s+['"][^'"]+['"];?\s*$/gm, '');
	// strip export default { ... }  ->  const __default = { ... }
	code = code.replace(/^export\s+default\s+/gm, 'const __default = ');
	// strip leading export on declarations
	code = code.replace(/^export\s+(async\s+function|function|const|class|let)/gm, '$1');
	out += `\n// ===== src/${file} =====\n${code}\n`;
}

// append default export at the end
out += `\n// ===== entry =====\nexport default __default;\n`;

writeFileSync(join(here, '_worker.js'), out, 'utf8');
console.log(`built _worker.js (${out.length} bytes) from ${order.length} modules`);
