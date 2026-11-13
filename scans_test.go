package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// scanStartBlocked 启动一个阻塞在 release 上的扫描：通过关闭 release（或
// 取消/超时 ctx）精确控制结束时机，避免测试里睡固定时长。
func scanStartBlocked(t *testing.T, m *ScanManager, timeout time.Duration, release chan struct{}, partial any) *Scan {
	t.Helper()
	sc, err := m.Start("remote_search_content", "/tmp/scan-test", timeout, func(ctx context.Context) (any, error) {
		select {
		case <-release:
			return "full-result", nil
		case <-ctx.Done():
			// 模拟真实搜索行为：超时/取消时交回已收集的部分结果。
			return partial, ctx.Err()
		}
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return sc
}

// scanWaitDone 事件驱动地等扫描到达终态（5s 兜底防挂死）。
func scanWaitDone(t *testing.T, sc *Scan) {
	t.Helper()
	sc.Await(context.Background(), 5*time.Second)
	select {
	case <-sc.done:
	default:
		t.Fatal("scan did not finish within 5s")
	}
}

func TestScanRunToCompletion(t *testing.T) {
	m := NewScanManager()
	release := make(chan struct{})
	sc := scanStartBlocked(t, m, time.Minute, release, "partial")

	// 启动即注册，运行中立即可查。
	if m.Lookup(sc.ID) != sc {
		t.Fatal("running scan not registered")
	}
	if snap := sc.Snapshot(); snap.State != ScanRunning || snap.Result != nil {
		t.Fatalf("initial snapshot: %+v", snap)
	}
	// 非阻塞 Await 立即返回，扫描不受影响。
	sc.Await(context.Background(), 0)
	if s := sc.Snapshot().State; s != ScanRunning {
		t.Fatalf("zero-wait Await disturbed the scan: %s", s)
	}

	close(release)
	scanWaitDone(t, sc)
	snap := sc.Snapshot()
	if snap.State != ScanCompleted || snap.Result != "full-result" || snap.Error != "" {
		t.Fatalf("final snapshot: %+v", snap)
	}
	if snap.ScanID != sc.ID || snap.Tool != "remote_search_content" || snap.TimeoutMs != time.Minute.Milliseconds() {
		t.Fatalf("snapshot metadata: %+v", snap.ScanInfo)
	}
	// done 关闭后零等待 Await 必须立刻返回且状态不变。
	sc.Await(context.Background(), 0)
	if s := sc.Snapshot().State; s != ScanCompleted {
		t.Fatalf("state changed after finish: %s", s)
	}
}

func TestScanTimeoutClamping(t *testing.T) {
	m := NewScanManager()
	release := make(chan struct{})
	defer close(release)

	sc := scanStartBlocked(t, m, 0, release, nil) // <=0 → 默认
	if sc.Timeout != defaultScanTimeout {
		t.Fatalf("default timeout: %v", sc.Timeout)
	}
	if ms := sc.Snapshot().TimeoutMs; ms != defaultScanTimeout.Milliseconds() {
		t.Fatalf("snapshot timeout: %d", ms)
	}
	sc2 := scanStartBlocked(t, m, 2*time.Hour, release, nil) // >max → 压到上限
	if sc2.Timeout != maxScanTimeout {
		t.Fatalf("max timeout clamp: %v", sc2.Timeout)
	}
	if ms := sc2.Snapshot().TimeoutMs; ms != maxScanTimeout.Milliseconds() {
		t.Fatalf("clamped snapshot timeout: %d", ms)
	}
}

func TestScanTimeoutRetainsPartialResult(t *testing.T) {
	m := NewScanManager()
	// run 交回部分结果 + ctx.Err()：应记为 timed_out 且保留结果。
	sc, err := m.Start("remote_search_content", "/x", 25*time.Millisecond, func(ctx context.Context) (any, error) {
		<-ctx.Done()
		return "partial", ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	scanWaitDone(t, sc)
	snap := sc.Snapshot()
	if snap.State != ScanTimedOut || snap.Result != "partial" {
		t.Fatalf("timed-out scan: %+v", snap)
	}
	if snap.Error == "" {
		t.Fatal("timed_out snapshot should carry the deadline error")
	}

	// run 交回部分结果但谎报 nil 错误：以 ctx 为准仍是 timed_out。
	sc2, err := m.Start("remote_search_content", "/x", 25*time.Millisecond, func(ctx context.Context) (any, error) {
		<-ctx.Done()
		return "partial2", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	scanWaitDone(t, sc2)
	if snap := sc2.Snapshot(); snap.State != ScanTimedOut || snap.Result != "partial2" {
		t.Fatalf("ignored-deadline scan: %+v", snap)
	}
}

func TestScanCancelKeepsPartialResult(t *testing.T) {
	m := NewScanManager()
	sc, err := m.Start("remote_aggregate", "/x", time.Minute, func(ctx context.Context) (any, error) {
		<-ctx.Done()
		return 42, nil // 取消后仍交回部分结果，且不报错
	})
	if err != nil {
		t.Fatal(err)
	}
	sc.Cancel()
	sc.Cancel() // 幂等
	scanWaitDone(t, sc)
	snap := sc.Snapshot()
	if snap.State != ScanCancelled || snap.Result != 42 {
		t.Fatalf("cancelled scan: %+v", snap)
	}
	if snap.ScanID != sc.ID || snap.Tool != "remote_aggregate" || snap.TimeoutMs != time.Minute.Milliseconds() {
		t.Fatalf("cancelled metadata: %+v", snap.ScanInfo)
	}
}

func TestScanStartSurvivesCallerContext(t *testing.T) {
	// 请求侧 ctx 断开（客户端放弃）不得影响扫描：这是分离式的核心语义。
	m := NewScanManager()
	release := make(chan struct{})
	sc := scanStartBlocked(t, m, time.Minute, release, "partial")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { sc.Await(ctx, time.Minute); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Await ignored a done caller context")
	}
	if s := sc.Snapshot().State; s != ScanRunning {
		t.Fatalf("caller ctx cancel stopped the scan: %s", s)
	}
	close(release)
	scanWaitDone(t, sc)
	if s := sc.Snapshot().State; s != ScanCompleted {
		t.Fatalf("scan should complete after caller ctx died: %s", s)
	}
}

func TestScanAwaitWakesOnTimer(t *testing.T) {
	m := NewScanManager()
	release := make(chan struct{})
	defer close(release)
	sc := scanStartBlocked(t, m, time.Minute, release, nil)

	start := time.Now()
	sc.Await(context.Background(), 30*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Fatalf("Await returned too early: %v", elapsed)
	}
	if s := sc.Snapshot().State; s != ScanRunning {
		t.Fatalf("timer expiry ended the scan: %s", s)
	}
}

func TestScanPanicRecoveredAsFailure(t *testing.T) {
	m := NewScanManager()
	sc, err := m.Start("remote_search_content", "/x", time.Minute, func(context.Context) (any, error) {
		panic("boom")
	})
	if err != nil {
		t.Fatal(err)
	}
	scanWaitDone(t, sc) // panic 被恢复：done 正常关闭，服务不崩
	snap := sc.Snapshot()
	if snap.State != ScanFailed || snap.Result != nil {
		t.Fatalf("panicked scan: %+v", snap)
	}
	if !strings.Contains(snap.Error, "boom") {
		t.Fatalf("panic error not retained: %q", snap.Error)
	}
}

func TestScanManagerActiveLimit(t *testing.T) {
	m := NewScanManager()
	release := make(chan struct{})
	defer close(release)

	var scans []*Scan
	for i := 0; i < maxActiveScans; i++ {
		scans = append(scans, scanStartBlocked(t, m, time.Minute, release, "partial"))
	}
	// 占满后必须拒绝，且不注册、不起 goroutine。
	_, err := m.Start("remote_search_content", "/x", time.Minute, func(context.Context) (any, error) {
		return nil, nil
	})
	if !errors.Is(err, ErrTooManyScans) {
		t.Fatalf("active limit not enforced: %v", err)
	}
	if got := len(m.List()); got != maxActiveScans {
		t.Fatalf("rejected scan leaked into the registry: %d", got)
	}

	// 取消一个释放槽位后即可再次启动。
	last := scans[len(scans)-1]
	last.Cancel()
	scanWaitDone(t, last)
	sc, err := m.Start("remote_search_content", "/x", time.Minute, func(context.Context) (any, error) {
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("Start after a slot freed: %v", err)
	}
	scanWaitDone(t, sc)
}

func TestScanManagerShutdown(t *testing.T) {
	m := NewScanManager()
	release := make(chan struct{})
	defer close(release)
	var scans []*Scan
	for i := 0; i < 2; i++ {
		scans = append(scans, scanStartBlocked(t, m, time.Minute, release, "partial"))
	}
	if n := m.CancelAll(); n != 2 {
		t.Fatalf("CancelAll cancelled %d scans, want 2", n)
	}
	for _, sc := range scans {
		scanWaitDone(t, sc)
		if s := sc.Snapshot().State; s != ScanCancelled {
			t.Fatalf("shutdown state: %s", s)
		}
	}
	// 关停后拒绝新的 Start；重复 CancelAll 幂等返回 0。
	if _, err := m.Start("remote_search_content", "/x", time.Minute, func(context.Context) (any, error) {
		return nil, nil
	}); !errors.Is(err, ErrScanManagerClosed) {
		t.Fatalf("closed manager accepted Start: %v", err)
	}
	if n := m.CancelAll(); n != 0 {
		t.Fatalf("second CancelAll returned %d", n)
	}
	// 已结束的记录在关停后仍可查（结果不被销毁）。
	if m.Lookup(scans[0].ID) == nil {
		t.Fatal("finished scan lost after shutdown")
	}
}

func TestScanManagerListNewestFirst(t *testing.T) {
	m := NewScanManager()
	var want []*Scan
	for i := 0; i < 3; i++ {
		sc, err := m.Start("remote_search_content", fmt.Sprintf("/x%d", i), time.Minute,
			func(context.Context) (any, error) { return i, nil })
		if err != nil {
			t.Fatal(err)
		}
		scanWaitDone(t, sc)
		// 人为把 StartedAt 拉开固定间隔，保证排序断言稳定。
		sc.mu.Lock()
		sc.StartedAt = time.Now().Add(time.Duration(i) * time.Second)
		sc.mu.Unlock()
		want = append(want, sc)
	}
	list := m.List()
	if len(list) != 3 {
		t.Fatalf("list length: %d", len(list))
	}
	for i := 1; i < len(list); i++ {
		if !list[i-1].StartedAt.After(list[i].StartedAt) {
			t.Fatalf("list not newest-first: %v then %v", list[i-1].StartedAt, list[i].StartedAt)
		}
	}
}

// finishedScan 直接构造一个已结束的扫描塞进注册表（不跑 goroutine），
// 用来验证惰性修剪的保留/淘汰规则。endedAt/StartedAt 由调用方指定。
func finishedScan(id string, started, ended time.Time, state string) *Scan {
	sc := &Scan{ID: id, Tool: "remote_search_content", StartedAt: started, Timeout: time.Minute,
		done: make(chan struct{}), state: state, endedAt: ended}
	close(sc.done)
	return sc
}

func TestScanManagerRetentionAndBound(t *testing.T) {
	m := NewScanManager()
	now := time.Now()
	for i := 0; i < maxRetainedScans; i++ {
		ended := now.Add(-time.Duration(i) * time.Minute) // 0..15 分钟前，均在保留期内
		m.scans[fmt.Sprintf("s%02d", i)] = finishedScan(fmt.Sprintf("s%02d", i), ended.Add(-time.Minute), ended, ScanCompleted)
	}
	m.scans["expired"] = finishedScan("expired", now.Add(-2*time.Hour), now.Add(-scanRetention-time.Minute), ScanCompleted)
	// 运行中的扫描哪怕“最老”也不能被修剪。
	running := &Scan{ID: "running", Tool: "remote_search_content", StartedAt: now.Add(-2 * time.Hour),
		Timeout: time.Minute, done: make(chan struct{}), state: ScanRunning}
	m.scans["running"] = running

	if m.Lookup("expired") != nil {
		t.Fatal("expired scan still visible")
	}
	if m.Lookup("s00") == nil || m.Lookup("running") == nil {
		t.Fatal("retained/running scan missing")
	}

	// 多塞一个更新的已完成扫描：最早完成的那个（s15）应被修剪。
	m.scans["newest"] = finishedScan("newest", now.Add(-time.Second), now, ScanCompleted)
	m.Lookup("s00") // 触发惰性 prune
	if m.Lookup("s15") != nil {
		t.Fatal("earliest-ended finished scan not pruned at the bound")
	}
	if m.Lookup("newest") == nil || m.Lookup("s14") == nil || m.Lookup("running") == nil {
		t.Fatal("wrong scans pruned at the bound")
	}

	// 修剪按完成时间而非启动时间：启动晚但完成早的先被淘汰。
	m2 := NewScanManager()
	m2.scans["late-start-early-end"] = finishedScan("late-start-early-end", now.Add(-time.Hour), now.Add(-30*time.Minute), ScanCompleted)
	m2.scans["early-start-late-end"] = finishedScan("early-start-late-end", now.Add(-2*time.Hour), now.Add(-time.Minute), ScanCompleted)
	for i := 0; i < maxRetainedScans-1; i++ { // 填满剩余名额（留 1 个），完成时间都更晚
		ended := now.Add(-time.Duration(i) * time.Second)
		m2.scans[fmt.Sprintf("f%02d", i)] = finishedScan(fmt.Sprintf("f%02d", i), ended.Add(-time.Minute), ended, ScanCompleted)
	}
	m2.Lookup("warm") // 触发惰性 prune
	if m2.Lookup("late-start-early-end") != nil {
		t.Fatal("prune should drop the earliest-ended scan, not the earliest-started")
	}
	if m2.Lookup("early-start-late-end") == nil {
		t.Fatal("later-ended scan should survive")
	}
}

func TestScanResultImmutableAfterFinish(t *testing.T) {
	m := NewScanManager()
	release := make(chan struct{})
	sc := scanStartBlocked(t, m, time.Minute, release, "partial")
	close(release)
	scanWaitDone(t, sc)

	first := sc.Snapshot()
	for i := 0; i < 3; i++ {
		next := sc.Snapshot()
		if next.State != first.State || next.Result != first.Result || next.Error != first.Error ||
			next.ElapsedMs != first.ElapsedMs || next.StartedAt != first.StartedAt {
			t.Fatalf("finished scan snapshot mutated: %+v vs %+v", next, first)
		}
	}
}

func TestScanConcurrentReadersAndCancel(t *testing.T) {
	// -race 的目标：终态发布/关 done 与并发 Snapshot/List/Lookup/Cancel。
	m := NewScanManager()
	const rounds = 8
	for i := 0; i < rounds; i++ {
		release := make(chan struct{})
		sc, err := m.Start("remote_search_content", "/x", time.Minute, func(ctx context.Context) (any, error) {
			select {
			case <-release:
				return "done", nil
			case <-ctx.Done():
				return "partial", ctx.Err()
			}
		})
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		var wg sync.WaitGroup
		for r := 0; r < 3; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 50; j++ {
					_ = sc.Snapshot()
					_ = m.List()
					_ = m.Lookup(sc.ID)
				}
			}()
		}
		if i%2 == 0 {
			sc.Cancel()
		} else {
			close(release)
		}
		scanWaitDone(t, sc)
		wg.Wait()
	}
	for _, sc := range m.List() {
		if s := sc.Snapshot().State; s != ScanCompleted && s != ScanCancelled {
			t.Fatalf("unexpected terminal state: %s", s)
		}
	}
}

func TestScanManagerRejectsNilRun(t *testing.T) {
	m := NewScanManager()
	if _, err := m.Start("remote_search_content", "/x", time.Minute, nil); err == nil {
		t.Fatal("nil run accepted")
	}
}

func TestScanManagerLookupEmpty(t *testing.T) {
	if NewScanManager().Lookup("") != nil {
		t.Fatal("empty id should not resolve")
	}
}
