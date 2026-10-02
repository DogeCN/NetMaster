// Node 测试专用：把 cloudflare:* 平台模块映射到本地 shim，让 src/ 的静态
// import 在 Node 里可加载。用法见 package.json 的 test 脚本（NODE_OPTIONS=--import）。
import { register } from "node:module";
register("./cloudflare-loader.mjs", import.meta.url);
