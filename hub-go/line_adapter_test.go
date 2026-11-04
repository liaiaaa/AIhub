package main

// 2026-11-02 A2 第二步：线路适配器注册表的回归锁定。
//
// 目的：保证「新增一条线路」只有三处要动（lineSpecs 元数据 / lineAdapter 实现 /
// lineAdapters 注册），并且注册表与元数据表不能各自漂移。

import "testing"

func TestEveryLineSpecHasAdapter(t *testing.T) {
	for _, spec := range lineSpecs {
		factory, ok := lineAdapters[spec.Key]
		if !ok {
			t.Errorf("线路 %q 在 lineSpecs 里有、注册表里没有适配器", spec.Key)
			continue
		}
		ad := factory(&Hub{})
		if ad == nil {
			t.Errorf("线路 %q 的构造器返回 nil", spec.Key)
			continue
		}
		if ad.Key() != spec.Key {
			t.Errorf("适配器 Key()=%q 与 lineSpecs 的 %q 不一致", ad.Key(), spec.Key)
		}
	}
	// 反向：注册表里不能有元数据表里不存在的线路
	known := map[string]bool{}
	for _, spec := range lineSpecs {
		known[spec.Key] = true
	}
	for key := range lineAdapters {
		if !known[key] {
			t.Errorf("注册表里有 %q，但 lineSpecs 里没有这条线路的元数据", key)
		}
	}
}

func TestAdapterStatusOnStoppedHub(t *testing.T) {
	// 未初始化的 Hub：所有适配器必须能安全回答「没在跑」，不能 panic
	h := &Hub{}
	for key := range lineAdapters {
		ad, _ := h.adapterFor(key)
		if ad.Running() {
			t.Errorf("%s 在空 Hub 上不该是运行态", key)
		}
	}
}

func TestZcodeAdapterAsksForApiKey(t *testing.T) {
	h := &Hub{}
	ad, ok := h.adapterFor("zcode")
	if !ok {
		t.Fatal("zcode 适配器缺失")
	}
	started, msg := ad.Start()
	if started || msg == "" {
		t.Fatalf("zcode 的 Start 应返回失败+提示，实际 %v %q", started, msg)
	}
}

func TestTraeAdapterModelsMirrorTraeModels(t *testing.T) {
	h := &Hub{}
	ad, _ := h.adapterFor("trae")
	got := ad.Models()
	if len(got) != len(TraeModels) {
		t.Fatalf("trae 适配器模型数 %d，TraeModels %d", len(got), len(TraeModels))
	}
	if len(got) != 25 {
		t.Errorf("TraeModels 应为 25 个，实际 %d", len(got))
	}
}

func TestAdapterForUnknownLine(t *testing.T) {
	h := &Hub{}
	if _, ok := h.adapterFor("不存在的线路"); ok {
		t.Error("未知线路不该返回适配器")
	}
}
