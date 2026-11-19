package bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"qoder2api/account"
)

// queueDeadlineServer 总是回一个「真队列」（queueCount>0 且 serviceAvailable=true，
// 见 queueEnvelopeFrame），即 queueWorthSameAccountRetry 会放行同账号重试的情形。
func queueDeadlineServer(t *testing.T, retryAfterSec int, hits *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, queueEnvelopeFrame(retryAfterSec))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestQueueSameAccountWaitRespectsSlotDeadline 同账号排队等待必须被调用方给的
// 截止时刻封顶。
//
// 线上现场（2026-11-18，app.log + ~/.qoder2api-cpa/logs）：Qoder 上游对 dfmodel
// 真排队（「前方约 1 人」）时，插件在同一账号上连等 6 次 ≈2m30s 仍不上抛：
//
//	20:42:29 upstream queued (model congested), waiting 30s before retry … (try 1, budget 30m0s)
//	20:45:02 …(try 6, waited ~2m30s, budget 30m0s)
//
// 这一层当时只认 QueueWaitBudget（默认 30 分钟）；外层 CallQoderWithOpts 算好的
// MaxQueueWait / PerAccountQueueWait 截止时刻在这里**一个都不生效**。后果不是
// 「慢」而是**路由失效**：插件不返回错误，host 就永远拿不到失败信号，也就没机会
// 把该请求回落到其它 provider（workbuddy / CodeBuddy），客户端只能挂到自己断开
// （`499 | 2m27s`）。裸名 deepseek-v4.1-flash 因此看起来像「CodeBuddy 的模型用不了」。
//
// 断言用线上同款参数（firstToken=true、budget=30m）：只有 deadline 能救。
// 没有本修复，这里会睡满一整个 retryAfter（30s）后再打第二次上游。
func TestQueueSameAccountWaitRespectsSlotDeadline(t *testing.T) {
	guardSettings(t, account.Settings{
		MinRequestGapMs:     1,
		QueueWaitFirstToken: boolPtr(true), // 线上默认：首字前一直等
		QueueMaxRetries:     10,
		QueueWaitDefaultSec: 30,
		QueueWaitBudgetSec:  1800, // 线上默认 30m：这一层不能指望它兜底
	})
	ApplyQueueSettings(10, 30, 30, true, 1800)
	t.Cleanup(func() { ApplyQueueSettings(0, 0, 0, true, 0) })

	hits := 0
	srv := queueDeadlineServer(t, 30, &hits) // 上游说：再等 30s

	b := newAffinityBridge(t, srv.URL, "dl-1")
	body := map[string]interface{}{"model_config": map[string]interface{}{"key": "auto"}}

	start := time.Now()
	deadline := start.Add(2 * time.Second) // 模拟 PerAccountQueueWait=2s
	_, err := b.streamWithSlot(context.Background(), b.pool.slots[0], []string{srv.URL}, body, nil, deadline, func(Delta) {})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("deadline 到期必须上抛错误，否则 host 永远拿不到失败信号、无法回落其它 provider")
	}
	if !isUpstreamQueue(err) {
		t.Errorf("上抛的必须是队列类错误（host 据此归类为上游拥塞），got %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("elapsed = %v，应在 deadline(2s) 处收手；说明截止时刻没被这一层采纳", elapsed)
	}
	if hits != 1 {
		t.Errorf("upstream hits = %d, want 1（必须在第一个 retryAfter 之前放弃，而不是等完再检查）", hits)
	}
}

// TestQueueDeadlineDoesNotCutShortWhenGenerous 反向护栏：截止时刻宽松时，
// 真队列仍应照常同账号等待重试——修复不能变成「逢队列就立刻失败」，那会把
// 本来等 30s 就能成的请求白白转给别人。
func TestQueueDeadlineDoesNotCutShortWhenGenerous(t *testing.T) {
	guardSettings(t, account.Settings{
		MinRequestGapMs:     1,
		QueueWaitFirstToken: boolPtr(true),
		QueueMaxRetries:     10,
		QueueWaitDefaultSec: 1,
		QueueWaitBudgetSec:  1800,
	})
	ApplyQueueSettings(10, 30, 1, true, 1800)
	t.Cleanup(func() { ApplyQueueSettings(0, 0, 0, true, 0) })

	hits := 0
	srv := queueDeadlineServer(t, 1, &hits) // 每次只等 1s

	b := newAffinityBridge(t, srv.URL, "dl-2")
	body := map[string]interface{}{"model_config": map[string]interface{}{"key": "auto"}}

	start := time.Now()
	deadline := start.Add(4 * time.Second) // 足够容纳数次 1s 等待
	_, err := b.streamWithSlot(context.Background(), b.pool.slots[0], []string{srv.URL}, body, nil, deadline, func(Delta) {})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("上游一直在排队，最终仍应上抛")
	}
	if elapsed < 2*time.Second {
		t.Errorf("elapsed = %v, 截止时刻宽松时应照常等待重试，不能秒放弃", elapsed)
	}
	if hits < 3 {
		t.Errorf("upstream hits = %d, want >=3（1s 一次、4s 截止），deadline 不应提前掐断", hits)
	}
}

// TestQueueRecoverySurfacesQueueErrorToCaller 打通外层：streamWithQueueRecovery
// 拿到同账号的 deadline 后，必须在它到期时把错误交给调用方（→ 换号 / 回落其它
// provider），而不是继续在这一层内部兜圈子。
func TestQueueRecoverySurfacesQueueErrorToCaller(t *testing.T) {
	guardSettings(t, account.Settings{
		MinRequestGapMs:     1,
		QueueWaitFirstToken: boolPtr(true),
		QueueMaxRetries:     10,
		QueueWaitDefaultSec: 30,
		QueueWaitBudgetSec:  1800,
	})
	ApplyQueueSettings(10, 30, 30, true, 1800)
	t.Cleanup(func() { ApplyQueueSettings(0, 0, 0, true, 0) })

	hits := 0
	srv := queueDeadlineServer(t, 30, &hits)

	b := newAffinityBridge(t, srv.URL, "dl-3", "dl-4")
	body := map[string]interface{}{"model_config": map[string]interface{}{"key": "auto"}}
	slot := b.pool.slots[0]

	start := time.Now()
	emitted, err := b.streamWithQueueRecovery(context.Background(), slot, []string{srv.URL}, body, nil,
		time.Now().Add(2*time.Second), func(Delta) {})
	elapsed := time.Since(start)

	if emitted {
		t.Fatal("不应发出任何内容")
	}
	if err == nil {
		t.Fatal("必须上抛错误")
	}
	if elapsed > 6*time.Second {
		t.Errorf("elapsed = %v, 外层 deadline 到期后应迅速上抛，让上层换号/回落", elapsed)
	}
}
