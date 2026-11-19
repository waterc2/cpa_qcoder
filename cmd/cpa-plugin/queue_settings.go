package main

import (
	"qoder2api/account"
	"qoder2api/internal/bridge"
	"qoder2api/logger"
)

// applyQueueSettingsFromSettings forwards the console-owned queue knobs
// (settings.json: queue_max_retries / queue_wait_cap_sec / queue_wait_default_sec /
// queue_wait_first_token / queue_wait_budget_sec) into the bridge.
//
// Why the plugin has to do this itself: bridge.ApplyQueueSettings is the only
// way to change those package-level values, and before 2026-11-18 it was called
// exclusively from service.go / main.go — i.e. the **standalone server**. The
// plugin never called it, so under CPA every queue knob silently stayed at its
// bridge default no matter what the operator wrote into settings.json
// (QueueWaitFirstToken=true, QueueWaitBudget=30m among them). The plugin has no
// HTTP listener of its own, so nothing else would ever apply them either.
//
// This does not change any default by itself: a field that is absent still falls
// back to the bridge default (ApplyQueueSettings treats ≤0 / nil that way). What
// it fixes is that an explicit value now reaches the plugin instead of rotting
// in the file.
//
// Effective waiting is bounded by three things, and all three are now real:
//  1. PerAccountQueueWait / MaxQueueWait (env) — the slot deadline, which
//     streamWithSlot honours since the 2026-11-18 fix;
//  2. QueueMaxRetries when queue_wait_first_token=false;
//  3. QueueWaitBudget — the absolute backstop.
// The startup log line below prints 1 and 3 together so a mis-tuned deployment is
// visible without reading code.
func applyQueueSettingsFromSettings() {
	st, err := account.LoadSettings()
	if err != nil {
		logger.Warn("queue settings: load settings failed: %v (keeping bridge defaults)", err)
		return
	}
	if st == nil {
		st = &account.Settings{}
	}
	bridge.ApplyQueueSettings(
		st.QueueMaxRetries,
		st.QueueWaitCapSec,
		st.QueueWaitDefaultSec,
		st.EffectiveQueueWaitFirstToken(),
		st.QueueWaitBudgetSec,
	)
	// bridge.ApplyQueueSettings 自己已经打了 retries/wait_cap/budget 那行；
	// 这里只补它看不到的部分：真正把等待封顶的 slot deadline（env 驱动）。
	logger.Info("queue slot deadline for this plugin: max_wait=%s per_account_wait=%s (0/unset = bridge default; bounds same-account queue wait since 2026-11-18)",
		bridge.MaxQueueWait, bridge.PerAccountQueueWait)
}
