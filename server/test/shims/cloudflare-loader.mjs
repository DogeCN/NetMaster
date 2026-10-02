// cloudflare:* → 本地 shim。socket.js 的 connect 落到这里：
// 优先用 globalThis.connect（测试注入 mock），没有就抛"未 mock"——
// 宁可响亮地失败，也别让测试在没打通路的情况下静默通过。
const SHIM = `
export function connect(options) {
  if (typeof globalThis.connect !== "function") {
    throw new Error("connect() called in Node without a mock (set globalThis.connect)");
  }
  return globalThis.connect(options);
}
export default {};
`;

export async function resolve(specifier, context, next) {
  if (specifier.startsWith("cloudflare:")) {
    return {
      url: "data:text/javascript," + encodeURIComponent(SHIM),
      shortCircuit: true,
    };
  }
  return next(specifier, context);
}
