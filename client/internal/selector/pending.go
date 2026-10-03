package selector

import (
	"errors"
	"math/rand"
	"sync"
	"time"
)

// 断线重连与等待队列（PRD §6.7）：
//
//	退避      1s → 2s → 4s → 8s → 16s → 32s，每次 ±20% 抖动
//	节点判死  连续失败 nodeFailLimit 次剔除并从池中重随机，退避清零
//	等待队列  上限 pendingLimit 条、单条 pendingTimeout；重连后批量发起
//	不做会话恢复：在途流直接关闭，由上层应用自行重试
//
// 关键取舍：等待队列里的请求在"传输就绪"时收到信号后**自己**再发一次开流，
// 而不是由重连 goroutine 代发 —— 同一批里一条慢流（服务端出站最长 15s）不该
// 拖住整批，也让失败归属清晰（错误直接回到调用方）。

const (
	backoffBase    = 1 * time.Second
	backoffMax     = 32 * time.Second
	nodeFailLimit  = 5
	pendingLimit   = 128
	pendingTimeout = 10 * time.Second
)

var (
	errPendingFull    = errors.New("selector: pending queue full")
	errPendingTimeout = errors.New("selector: timed out waiting for tunnel")
	errNoUsableExit   = errors.New("selector: no usable exit")
	errStopped        = errors.New("selector: stopped")
)

// backoffFor 返回第 attempt 次失败后的退避时长（attempt 从 1 起），带 ±20% 抖动。
func backoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := backoffBase
	for i := 1; i < attempt && d < backoffMax; i++ {
		d *= 2
	}
	if d > backoffMax {
		d = backoffMax
	}
	jitter := 1 + (rand.Float64()*0.4 - 0.2)
	return time.Duration(float64(d) * jitter)
}

// pendingReq 是一条等待传输就绪的请求。
type pendingReq struct {
	target string
	ready  chan error // nil = 传输已就绪；非 nil = 失败原因（含超时）
	once   sync.Once
}

func (p *pendingReq) settle(err error) {
	p.once.Do(func() { p.ready <- err })
}

// dialPending 管理等待队列与重连退避。挂载在 Pool 上。
type dialPending struct {
	pool *Pool

	mu       sync.Mutex
	queue    []*pendingReq
	attempt  int  // 连续失败次数，决定退避时长
	retrying bool // 是否已有重连 goroutine 在跑
	stopped  bool
}

func newDialPending(p *Pool) *dialPending { return &dialPending{pool: p} }

// enqueue 把请求排进队列并触发重连。
func (d *dialPending) enqueue(target string) (*pendingReq, error) {
	req := &pendingReq{target: target, ready: make(chan error, 1)}
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return nil, errStopped
	}
	if len(d.queue) >= pendingLimit {
		d.mu.Unlock()
		return nil, errPendingFull
	}
	d.queue = append(d.queue, req)
	d.mu.Unlock()
	d.kick()
	return req, nil
}

// kick 确保只有一个重连 goroutine 在跑（退避期间后来的请求只排队等着）。
func (d *dialPending) kick() {
	d.mu.Lock()
	if d.retrying || d.stopped {
		d.mu.Unlock()
		return
	}
	d.retrying = true
	d.mu.Unlock()

	go func() {
		attempt := 0
		for {
			d.mu.Lock()
			if d.stopped || len(d.queue) == 0 {
				d.retrying = false
				d.mu.Unlock()
				return
			}
			attempt = d.attempt
			d.mu.Unlock()

			if attempt > 0 {
				time.Sleep(backoffFor(attempt))
			}
			// 先问"是不是已经有隧道了"，再问"要不要再拨一条"。
			//
			// 顺序反了会白等：请求排进队列是因为那一刻没有可用传输，而与它并发的
			// 另一个请求很可能已经建好了一条。此时若只盯着"拨号成功"才放行，
			// 队列会一直等到退避超时 —— 实测目标 2 条时，八个并发请求要等 10 秒，
			// 而隧道从头到尾都是活的，只是名额满了不需要再拨。
			ok := d.pool.liveCount() > 0 || d.pool.dialWithinBudget()
			if ok {
				d.mu.Lock()
				d.attempt = 0
				queued := d.queue
				d.queue = nil
				d.retrying = false
				d.mu.Unlock()
				for _, req := range queued {
					req.settle(nil)
				}
				// 排过队 = 需求确实超过了现有隧道扛得住的范围，这也是把池补回
				// muxTarget 的正确时机（空闲期刚回收过隧道时不会误触发）。
				d.pool.clearIdleHold()
				return
			}
			d.mu.Lock()
			d.attempt++
			d.mu.Unlock()
		}
	}()
}

// wake 在"传输已就绪"时立刻放行队列，不必等下一次轮询。
//
// 调用方是 Pool.attach —— 那是唯一确切的"一条传输刚刚可用"时刻。没有它的话，
// 排队的请求要等 kick 循环的下一轮（退避 1s→2s→4s…，最坏能等到 wait 的 10s 超时），
// 而这段时间里隧道明明已经好了。
func (d *dialPending) wake() {
	if d.pool.liveCount() == 0 {
		return
	}
	d.mu.Lock()
	if d.stopped || len(d.queue) == 0 {
		d.mu.Unlock()
		return
	}
	queued := d.queue
	d.queue = nil
	d.attempt = 0
	d.mu.Unlock()
	for _, req := range queued {
		req.settle(nil)
	}
	// 排过队 = 需求超过了现有隧道扛得住的范围，这也是把池补回 muxTarget 的时机。
	d.pool.clearIdleHold()
}

// stop 终止队列：所有等待者立即失败。
func (d *dialPending) stop() {
	d.mu.Lock()
	d.stopped = true
	queued := d.queue
	d.queue = nil
	d.mu.Unlock()
	for _, req := range queued {
		req.settle(errStopped)
	}
}

// wait 等到传输就绪（或超时）。
func (d *dialPending) wait(req *pendingReq) error {
	select {
	case err := <-req.ready:
		return err
	case <-time.After(pendingTimeout + 500*time.Millisecond):
		return errPendingTimeout
	}
}
