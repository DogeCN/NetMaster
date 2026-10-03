// run-all.mjs —— npm test 的跨平台入口。
//
// 之前 npm test 用 `NODE_OPTIONS="..." node test/x.mjs` 的 POSIX 环境变量前缀，
// Windows 的 cmd 不认这种写法，直接 exit 1；且漏了 refresh-relays 套件。
// 用 spawnSync 显式传 --import，两个平台行为一致。
import { spawnSync } from "node:child_process";

// crypto/protocol 是纯逻辑；其余套件经 socket.js 摸平台模块，要挂 Node shim。
const suites = [
  ["crypto", false],
  ["protocol", false],
  ["integration", true],
  ["proxyip", true],
  ["race", true],
  ["router", true],
  ["refresh-relays", true],
];

for (const [name, shim] of suites) {
  const args = shim ? ["--import", "./test/shims/register.mjs", `test/${name}.mjs`] : [`test/${name}.mjs`];
  const r = spawnSync(process.execPath, args, { stdio: "inherit" });
  if (r.status !== 0) {
    console.error(`\nnpm test: suite "${name}" failed (exit ${r.status})`);
    process.exit(r.status ?? 1);
  }
}
console.log("\nnpm test: all 7 suites passed");
