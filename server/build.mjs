/**
 * build.mjs — bundle src/*.js into a single _worker.js for Cloudflare Workers.
 * ESM concatenation: relative imports are stripped (top-level names share bundle
 * scope); platform imports (cloudflare:*) are hoisted to the top.
 *
 * ⚠️ Module order is **derived from the actual import graph**, not a hand-written
 * list. The hardcoded array this replaced was a silent landmine: a new src/*.js that
 * nobody remembered to add got its relative import stripped to an empty string, its
 * contents never entered the bundle, and the build still exited 0. The missing symbol
 * then surfaced as a runtime ReferenceError — in production, behind a green CI gate
 * (`node --check` only validates syntax, never a free identifier).
 *
 * Order still matters (concatenated `const`/`class` have TDZ), so the graph is walked
 * depth-first and emitted dependencies-first. A cycle is a hard error: it cannot be
 * ordered, and silently picking an order would hide a real design problem.
 */
import { readFileSync, writeFileSync, readdirSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const srcDir = join(here, 'src');

// entry.js must stay last: it is what becomes `export default __default`.
const LAST = 'index.js';

const files = readdirSync(srcDir).filter((f) => f.endsWith('.js')).sort();
const sources = new Map(files.map((f) => [f, readFileSync(join(srcDir, f), 'utf8')]));

// Relative-import graph: module -> modules it needs first.
const deps = new Map();
for (const [file, code] of sources) {
  const needs = [];
  const re = /^\s*import\s+([^;]*?)from\s+['"]\.\/([^'"]+)['"];?\s*$/gm;
  let m;
  while ((m = re.exec(code)) !== null) {
    const [, clause, raw] = m;
    // Renamed imports (`import { a as b }`) cannot survive concatenation: stripping the
    // statement removes the binding without ever defining `b`, so the call site becomes
    // a runtime ReferenceError behind a green build. Reject it at build time instead.
    if (/\bas\b/.test(clause)) {
      console.error(`build: ${file} uses a renamed import (${clause.trim()}). ` +
        `The bundler concatenates modules and cannot honour \`as\`; use the original name.`);
      process.exit(1);
    }
    const dep = raw.endsWith('.js') ? raw : `${raw}.js`;
    if (!sources.has(dep)) {
      console.error(`build: ${file} imports ./${dep}, which is not in src/`);
      process.exit(1);
    }
    needs.push(dep);
  }
  deps.set(file, needs);
}

// Depth-first post-order: a module is emitted only after everything it needs.
const order = [];
const state = new Map(); // file -> 'visiting' | 'done'
function visit(file, stack) {
  const s = state.get(file);
  if (s === 'done') return;
  if (s === 'visiting') {
    console.error(`build: import cycle: ${[...stack, file].join(' → ')}`);
    process.exit(1);
  }
  state.set(file, 'visiting');
  for (const dep of deps.get(file) || []) visit(dep, [...stack, file]);
  state.set(file, 'done');
  order.push(file);
}
for (const f of files) visit(f, []);

// index.js has to define __default last; everything else before it.
const lastIdx = order.indexOf(LAST);
if (lastIdx === -1) {
  console.error(`build: src/${LAST} is missing`);
  process.exit(1);
}
order.splice(lastIdx, 1);
order.push(LAST);

const platformImports = new Set();

let out = `/**
 * _worker.js — NetMaster protocol v2 (mux over WebSocket, Session DO).
 *
 * GENERATED from src/ by build.mjs — edit src/, not this file.
 * Modules (ordered from the import graph): ${order.join(', ')}
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
