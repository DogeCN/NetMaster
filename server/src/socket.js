// socket.js — connect() 的唯一出口。
//
// Workers 里 connect() 不是全局，必须从 cloudflare:sockets import（静态 import
// 由 build.mjs 提升到 bundle 顶部）。Node 单测没有这个平台模块，直接 import 会在
// 加载期炸掉测试 —— 用 test/shims 里的 loader 把 cloudflare:* 映射到本地 shim
// （见 package.json 的 test 脚本与 test/shims/register.mjs）。
import { connect } from "cloudflare:sockets";

export { connect };
