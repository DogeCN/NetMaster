// Package instlock 保证 serve 单实例（PRD §6.5 step 1）。
//
// 没有它，双击两次 exe 就是两个 serve 同时抢系统代理：后启动的把前一个的
// 还原状态覆盖掉，退出顺序一乱系统代理就悬空指向一个已经不存在的端口。
//
// 平台实现（Acquire）：
//   - unix（instlock_unix.go）：flock。锁属于打开的文件描述，进程一死内核
//     就放锁，不存在陈旧锁。
//   - windows（instlock_windows.go）：O_EXCL 建锁文件 + 持有者 PID 活性
//     探测，死进程留下的锁视为陈旧，清掉重拿。
package instlock

import "errors"

// ErrLocked 表示已有存活实例持有锁。
var ErrLocked = errors.New("netmaster is already running")
