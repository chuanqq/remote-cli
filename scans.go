package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// 分离式只读扫描（remote_search_content / remote_aggregate）
//
// MCP 客户端会按自己的请求超时（TypeScript SDK 默认 60s）掐断工具调用，
// 而一次大目录的内容搜索或聚合往往超过这个预算。ScanManager 把这类只读
// 扫描从请求上彻底剥离：Start 立即返回，扫描在后台 goroutine 里运行，
// 只有它自身的 timeout、Scan.Cancel / CancelAll（服务关停）能停止它；
// 请求断开不影响扫描。调用方在本次预算内 Await 一段，等不到就拿
// scan_id 之后通过 remote_scan_result 续取。
//
// 与 executions.go 的分离式命令同构，但刻意不耦合 shell 执行：run 是一
// 个纯函数（内存中的搜索/聚合），不产生进程，也不需要进程组管理。run
// 必须尊重传入的 ctx，在超时/取消时尽量返回已收集的部分结果 —— 部分结
// 果会随 timed_out / cancelled 状态一起原样保留。
//
// 已结束的扫描按完成时间保留：最近 maxRetainedScans 个、结束后
// scanRetention 内可查，超期后 Lookup 不到。清理是惰性的（Start /
// Lookup / List / CancelAll 时顺手 prune），不为每个任务挂常驻定时器；
// 扫描自身的 timeout 由 context.WithTimeout 承担，run 返回后立刻
// cancel 释放其计时器资源。结果一经写入不可变：内部从不修改已发布的
// result，也没有 Execution 那种输出丢弃 —— 扫描/聚合的输出本身就有界。
// ---------------------------------------------------------------------------

const (
	// defaultScanTimeout：调用方未指定（<=0）时整个扫描自身的默认时限。
	defaultScanTimeout = 300 * time.Second
	// maxScanTimeout：单个扫描允许的最大时限，超出会被压到该值。
	maxScanTimeout = time.Hour
	// scanRetention：已结束扫描的可查时长，超期后 Lookup 不到。
	scanRetention = 30 * time.Minute
	// maxActiveScans：同时运行的扫描上限，占满时 Start 直接拒绝。
	maxActiveScans = 4
	// maxRetainedScans：保留的可查（已结束）扫描个数上限，按完成时间
	// 从新到旧修剪。
	maxRetainedScans = 16
)

// 扫描状态（报告给 MCP 客户端）。
const (
	ScanRunning   = "running"   // 仍在运行：凭 scan_id 之后续取
	ScanCompleted = "completed" // 扫描正常结束；覆盖范围见结果中的完整性标记
	ScanTimedOut  = "timed_out" // 到达自身 timeout；部分结果（若有）仍保留
	ScanCancelled = "cancelled" // 被 Scan.Cancel / CancelAll（服务关停）终止
	ScanFailed    = "failed"    // run 返回错误或 panic
)

// Start 的哨兵错误：管理器已关闭（CancelAll 之后）或活跃扫描占满。
var (
	ErrScanManagerClosed = errors.New("scan manager is closed")
	ErrTooManyScans      = errors.New("too many active scans")
)

// Scan 是一次由 ScanManager.Start 启动的分离式只读扫描。导出字段是
// Start 时写入的一次性元数据，之后视为只读；状态与结果经 Snapshot
// 读取。Scan 不与任何请求绑定：只有自身 timeout、Cancel / CancelAll
// 能让它停下。
type Scan struct {
	ID        string        // scan_id：续取与取消的凭据
	Tool      string        // 发起扫描的工具名（remote_search_content / remote_aggregate）
	Path      string        // 扫描的根路径（列表展示用；不进 ScanInfo，避免与结果 JSON 的 path 冲突）
	StartedAt time.Time     // 启动时间
	Timeout   time.Duration // 整个扫描自身的时限，与单次调用预算无关

	done chan struct{} // 终态写入之后才 close，等待者醒来即可读到最终结果

	mu      sync.Mutex
	state   string             // ScanRunning，之后一次性变为某个终态
	result  any                // run 的返回值（可为部分结果）；发布后不可变
	errText string             // 失败/超时/取消原因
	endedAt time.Time          // 终态写入时间，保留与修剪按它计算
	cancel  context.CancelFunc // Scan.Cancel / CancelAll 的抓手；Start 时设置后不再变
}

// ScanInfo 是扫描的 JSON 元数据视图。刻意不含 Path：ScanInfo 会被嵌入
// 顶层结果返回，而 SearchContentResult / AggregateResult 自带
// json:"path" 字段，嵌入两份 path 会冲突。
type ScanInfo struct {
	ScanID    string `json:"scan_id"`
	Tool      string `json:"tool"`
	State     string `json:"state"`
	StartedAt string `json:"started_at"`
	ElapsedMs int64  `json:"elapsed_ms"`
	TimeoutMs int64  `json:"timeout_ms"`
}

// ScanSnapshot 是扫描某一时刻的状态与结果。Result 不进 JSON：渲染层
// 把它作为顶层结果字段直接输出。
type ScanSnapshot struct {
	ScanInfo
	Result any    `json:"-"`
	Error  string `json:"error,omitempty"`
}

// ScanManager 跟踪分离式只读扫描：活跃上限、有界保留与关停。零值不可
// 用，用 NewScanManager 创建。
type ScanManager struct {
	mu     sync.Mutex
	closed bool             // CancelAll 之后为真：Start 一律拒绝
	scans  map[string]*Scan // 运行中 + 保留中的已结束扫描
}

// NewScanManager 创建空的扫描管理器。
func NewScanManager() *ScanManager {
	return &ScanManager{scans: make(map[string]*Scan)}
}

// Start 启动一次分离式只读扫描并立即返回。run 收到一个独立于任何请求、
// 带 timeout 的 ctx，必须尊重它并尽量返回部分结果。timeout<=0 取
// defaultScanTimeout，超过 maxScanTimeout 会被压到该值。管理器已关闭
// 或活跃扫描已达 maxActiveScans 时拒绝：不注册、不启动 goroutine，返
// 回哨兵错误。
func (m *ScanManager) Start(tool, path string, timeout time.Duration, run func(context.Context) (any, error)) (*Scan, error) {
	if run == nil {
		return nil, errors.New("scan run function must not be nil")
	}
	if timeout <= 0 {
		timeout = defaultScanTimeout
	}
	if timeout > maxScanTimeout {
		timeout = maxScanTimeout
	}
	// cancel 在注册之前就位：CancelAll 一旦在 map 里看到本扫描，就能取消它。
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	sc := &Scan{
		ID:        uuid.NewString(),
		Tool:      tool,
		Path:      path,
		StartedAt: time.Now(),
		Timeout:   timeout,
		done:      make(chan struct{}),
		state:     ScanRunning,
		cancel:    cancel,
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		return nil, ErrScanManagerClosed
	}
	m.pruneLocked(time.Now())
	if n := m.activeLocked(); n >= maxActiveScans {
		m.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("%w: %d running (max %d); wait for one to finish or cancel one first",
			ErrTooManyScans, n, maxActiveScans)
	}
	m.scans[sc.ID] = sc
	m.mu.Unlock()

	go func() {
		sc.execute(ctx, cancel, run)
		m.mu.Lock()
		m.pruneLocked(time.Now())
		m.mu.Unlock()
	}()
	return sc, nil
}

// execute 在后台运行扫描并一次性发布终态：状态、结果与错误在锁内写好
// 之后才 close(done)，等待者醒来一定能读到最终结果。run 的 panic 在此
// 恢复成错误，避免拖垮整个服务。
func (sc *Scan) execute(ctx context.Context, cancel context.CancelFunc, run func(context.Context) (any, error)) {
	var (
		result any
		err    error
	)
	func() {
		defer func() {
			if p := recover(); p != nil {
				result, err = nil, fmt.Errorf("scan panicked: %v", p)
			}
		}()
		result, err = run(ctx)
	}()

	// 先取样终止原因再 cancel：cancel 会把未到期的 ctx 置为 Canceled，
	// 先看 ctx.Err() 才能区分超时与外部取消。
	state, errText := ScanCompleted, ""
	switch {
	case err != nil && errors.Is(err, context.DeadlineExceeded):
		state, errText = ScanTimedOut, err.Error() // 部分结果照常保留
	case err != nil && errors.Is(err, context.Canceled):
		state, errText = ScanCancelled, err.Error()
	case err != nil:
		state, errText = ScanFailed, err.Error()
	case ctx.Err() == context.DeadlineExceeded:
		// run 忽略了 ctx 仍返回 nil：以 ctx 为准，部分结果照常保留。
		state, errText = ScanTimedOut, ctx.Err().Error()
	case ctx.Err() == context.Canceled:
		state, errText = ScanCancelled, ctx.Err().Error()
	}
	cancel() // 释放 WithTimeout 的计时器等 ctx 资源

	sc.mu.Lock()
	sc.state = state
	sc.result = result
	sc.errText = errText
	sc.endedAt = time.Now()
	sc.mu.Unlock()
	close(sc.done)
}

// Await 阻塞至扫描结束、d 耗尽或调用方 ctx 结束（比如客户端放弃了本次
// 请求）。它不返回结果：结束后用 Snapshot 取。无论因何返回，扫描本身
// 都不受影响 —— 它只听自身 timeout 与 Cancel / CancelAll。d<=0 表示只
// 查不等：立即返回（select 的全 nil 分支会永久阻塞，不能走那条路）。
func (sc *Scan) Await(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-sc.done:
	case <-timer.C:
	case <-ctx.Done():
	}
}

// Snapshot 返回扫描当前状态的一致性快照。运行中每次调用的 elapsed_ms
// 都会前进；终态一经发布不可变，重复调用结果相同。
func (sc *Scan) Snapshot() ScanSnapshot {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	end := time.Now()
	if sc.state != ScanRunning {
		end = sc.endedAt
	}
	return ScanSnapshot{
		ScanInfo: ScanInfo{
			ScanID:    sc.ID,
			Tool:      sc.Tool,
			State:     sc.state,
			StartedAt: sc.StartedAt.Format(time.RFC3339Nano),
			ElapsedMs: end.Sub(sc.StartedAt).Milliseconds(),
			TimeoutMs: sc.Timeout.Milliseconds(),
		},
		Result: sc.result,
		Error:  sc.errText,
	}
}

// Cancel 取消运行中的扫描（幂等，对已结束的扫描无影响）。run 会收到
// ctx 取消，被期待返回已收集的部分结果。
func (sc *Scan) Cancel() {
	if c := sc.cancel; c != nil {
		c()
	}
}

// isRunning 报告扫描是否仍在运行。
func (sc *Scan) isRunning() bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.state == ScanRunning
}

// Lookup 返回指定 id 的扫描（运行中或保留中的已结束扫描），过期或未
// 知 id 返回 nil。
func (m *ScanManager) Lookup(id string) *Scan {
	if id == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(time.Now())
	return m.scans[id]
}

// List 返回全部已知扫描，按启动时间从新到旧排序。
func (m *ScanManager) List() []*Scan {
	m.mu.Lock()
	m.pruneLocked(time.Now())
	out := make([]*Scan, 0, len(m.scans))
	for _, sc := range m.scans {
		out = append(out, sc)
	}
	m.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a].StartedAt.After(out[b].StartedAt) })
	return out
}

// CancelAll 关停管理器：之后的 Start 一律拒绝（ErrScanManagerClosed），
// 正在运行的扫描全部取消（用于优雅关停），返回取消的个数。已结束的
// 保留记录不受影响。重复调用返回 0。
func (m *ScanManager) CancelAll() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0
	}
	m.closed = true
	n := 0
	for _, sc := range m.scans {
		if sc.isRunning() {
			sc.cancel()
			n++
		}
	}
	return n
}

// activeLocked 统计运行中的扫描个数。调用方持有 m.mu。
func (m *ScanManager) activeLocked() int {
	n := 0
	for _, sc := range m.scans {
		if sc.isRunning() {
			n++
		}
	}
	return n
}

// pruneLocked 惰性清理：先淘汰结束超过 scanRetention 的扫描，再把剩余
// 已结束扫描按完成时间从新到旧修剪到 maxRetainedScans 个。运行中的扫
// 描永不清理。调用方持有 m.mu。
func (m *ScanManager) pruneLocked(now time.Time) {
	type fin struct {
		id    string
		ended time.Time
	}
	var fins []fin
	for id, sc := range m.scans {
		sc.mu.Lock()
		state, ended := sc.state, sc.endedAt
		sc.mu.Unlock()
		if state == ScanRunning {
			continue
		}
		if now.Sub(ended) > scanRetention {
			delete(m.scans, id)
			continue
		}
		fins = append(fins, fin{id, ended})
	}
	if len(fins) <= maxRetainedScans {
		return
	}
	sort.Slice(fins, func(a, b int) bool { return fins[a].ended.After(fins[b].ended) })
	for _, f := range fins[maxRetainedScans:] {
		delete(m.scans, f.id)
	}
}
