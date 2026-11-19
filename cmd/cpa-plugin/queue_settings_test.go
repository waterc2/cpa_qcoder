package main

import (
	"testing"
	"time"

	"qoder2api/account"
	"qoder2api/internal/bridge"
)

// withQueueDefaults pins the bridge queue vars to a known baseline for the
// duration of a test, because ApplyQueueSettings writes package-level state.
func withQueueDefaults(t *testing.T) {
	t.Helper()
	omr, owc, owd, oft, ob := bridge.QueueMaxRetries, bridge.QueueWaitCap, bridge.QueueWaitDefault, bridge.QueueWaitFirstToken, bridge.QueueWaitBudget
	bridge.ApplyQueueSettings(2, 30, 15, true, 1800)
	t.Cleanup(func() {
		bridge.QueueMaxRetries, bridge.QueueWaitCap, bridge.QueueWaitDefault, bridge.QueueWaitFirstToken, bridge.QueueWaitBudget = omr, owc, owd, oft, ob
	})
}

func writeQueueSettings(t *testing.T, budgetSec, retries int, firstToken *bool) {
	t.Helper()
	st, err := account.LoadSettings()
	if err != nil || st == nil {
		t.Fatalf("load settings: %v", err)
	}
	st.QueueWaitBudgetSec = budgetSec
	st.QueueMaxRetries = retries
	st.QueueWaitFirstToken = firstToken
	if err := account.SaveSettings(st); err != nil {
		t.Fatalf("save settings: %v", err)
	}
}

// TestApplyQueueSettingsFromSettings settings.json 里的排队值必须真的落到 bridge。
//
// 插件模式（CPA）以前没有任何路径会调 bridge.ApplyQueueSettings ——上游只在
// service.go/main.go（独立服务）里调。所以 operator 写进 settings.json 的
// queue_* 字段在插件里是**纯装饰**，排队行为永远等于包级默认值。
func TestApplyQueueSettingsFromSettings(t *testing.T) {
	withQueueDefaults(t)
	firstToken := false
	writeQueueSettings(t, 90, 3, &firstToken)

	applyQueueSettingsFromSettings()

	if bridge.QueueWaitBudget != 90*time.Second {
		t.Errorf("QueueWaitBudget = %v, want 90s（settings.json 的值没进插件）", bridge.QueueWaitBudget)
	}
	if bridge.QueueMaxRetries != 3 {
		t.Errorf("QueueMaxRetries = %d, want 3", bridge.QueueMaxRetries)
	}
	if bridge.QueueWaitFirstToken != false {
		t.Errorf("QueueWaitFirstToken = %v, want false（显式关掉「首字前一直等」必须被采纳）", bridge.QueueWaitFirstToken)
	}
}

// TestApplyQueueSettingsWiredIntoConfigure 钉住**接线本身**：
// configure() 必须调用 applyQueueSettingsFromSettings。
//
// 单独测 applier 没用——真正的回归是"忘了在插件启动路径上调它"（这正是
// 2026-11-18 之前的状态）。用空 config 走一遍真实入口：initDataDir() 之后
// 立刻 early-return，能证明调用点在 early-return 之前。
func TestApplyQueueSettingsWiredIntoConfigure(t *testing.T) {
	withQueueDefaults(t)
	writeQueueSettings(t, 45, 1, nil)

	configure(nil) // 空 config：只跑 initDataDir + 排队设置下发

	if bridge.QueueWaitBudget != 45*time.Second {
		t.Errorf("configure() 没下发排队设置：QueueWaitBudget = %v, want 45s", bridge.QueueWaitBudget)
	}
	if bridge.QueueMaxRetries != 1 {
		t.Errorf("configure() 没下发排队设置：QueueMaxRetries = %d, want 1", bridge.QueueMaxRetries)
	}
}

// TestApplyQueueSettingsAbsentKeepsBridgeDefaults 缺字段不得变成"默认开/关新行为"：
// settings.json 里没写的 queue_* 必须保持 bridge 默认，而不是被清零。
func TestApplyQueueSettingsAbsentKeepsBridgeDefaults(t *testing.T) {
	withQueueDefaults(t)
	writeQueueSettings(t, 0, 0, nil)

	applyQueueSettingsFromSettings()

	if bridge.QueueWaitBudget != 30*time.Minute {
		t.Errorf("QueueWaitBudget = %v, want 30m（缺字段必须回 bridge 默认，不能清成 0）", bridge.QueueWaitBudget)
	}
	if bridge.QueueMaxRetries != 2 {
		t.Errorf("QueueMaxRetries = %d, want 2 (bridge default)", bridge.QueueMaxRetries)
	}
	if !bridge.QueueWaitFirstToken {
		t.Error("QueueWaitFirstToken 缺字段时应为 true（bridge 默认），不得翻转")
	}
}
