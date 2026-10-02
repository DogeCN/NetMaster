// socket.js — connect() 的双端解析。
//
// Workers 里 connect() 不是全局，要从 cloudflare:sockets import；Node（单测）
// 没有这个模块，静态 import 会在加载期直接炸掉测试。动态 import + 回退：
// Workers 拿到真实现，Node 落到 globalThis.connect（测试用 mock 注入那里）。
// 全部出口模块都从这里拿 connect，避免各自的声明在 bundle 里撞名。
let connect;
try {
	({ connect } = await import("cloudflare:sockets"));
} catch {
	connect = globalThis.connect;
}

export { connect };
