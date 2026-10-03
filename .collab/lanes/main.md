# 车道：主会话

## 2026-10-03 本轮做完的

- **HEAD 修好了**：之前 `d9be0da`（我）和 `840a26d`（协助者）各自引用了只在别人未提交
  工作区里的符号，两次都把 main 弄成编译不过。已补齐并在独立 worktree 复核 EXIT=0。
- **CI 门**：客户端单测原先只在打 tag 时跑。加了 `ci.yml` 的 `client` job（vet / gofmt /
  单测 / -race）。第一版挂在 `acceptance.yml` 上、且没 `npm ci`，恒红——已挪走。
- **C1**：`tunnels` 进 config.json（1–8，默认 4），`-tunnels` flag。
- **对抗评审 24 条**的修复（详见 history.md §12.1）：Close 非终态、空闲回收被自己的回调
  撤销、回收/attach 被拒被记成节点失败、attach 覆盖留下孤儿隧道、请求路径不计数、
  重试链无上限、兜底路径不截断 + 日志自相矛盾。
- **C2 + 实测**：启动探测现在验 WS 升级、走 ECH、先 TCP 预筛、复用已有连接。
  升级阶段 3.5s → **1.0~1.6s**。

## 留给协助者

- `client/internal/proxy/` 的分片实现仍未入库（`tlsfrag.go`、`replay.go` 改动、四个测试文件）。
  现在 `scripts/test-all.sh` 是绿的（我给 `tlsfrag_p1_test.go` 跑过一次 `gofmt -w`，只改格式）。
- `TestWinningFragmentedConnIsReleased` 仍然 flaky：独立跑必红，整套跑有时能过。
- `internal/tlsfrag/` 已入库，但 `tlsfrag` 包自己的测试是后来才补的（B2）。

## 我接下来要做的

1. ECH 的处置（等用户拍板，见 README）。
2. 客户端日志加逐请求记录：现在用户报障时除了启动那几行什么都看不到。
