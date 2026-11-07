package main

import (
	"errors"
	"testing"
)

// 探测的「权威性」判定（2026-11-07）。
//
// 只有真拿到官方实时清单（或代码内维护的权威静态表）才算 Authoritative，
// 同步时才可以据此删除已下架的模型；离线快照/兜底一律只增不删。

func TestProbeLineQoderFetchesOfficialCatalog(t *testing.T) {
	old := probeQoderFetch
	probeQoderFetch = func() ([]qoderModelEntry, error) {
		return []qoderModelEntry{
			{Key: "qfmodel", DisplayName: "Qwen3.8-Flash", PriceFactor: 0.1, IsFree: true},
			{Key: "lmodel", DisplayName: "QoDuck", PriceFactor: 0, IsFree: true},
		}, nil
	}
	t.Cleanup(func() { probeQoderFetch = old })

	line := probeLine(&Hub{}, "qoder")
	if !line.Authoritative {
		t.Fatal("抓到官方目录时必须标记 Authoritative，否则同步不会删除已下架模型")
	}
	var sawDuck, sawFlash bool
	for _, m := range line.Models {
		if !m.Official {
			t.Fatalf("官方目录条目必须标 Official：%+v", m)
		}
		switch m.ID {
		case "lmodel":
			sawDuck = m.Free
		case "qfmodel":
			sawFlash = m.Ratio == "0.1x" && !m.Free
		}
	}
	if !sawDuck {
		t.Fatalf("QoDuck 必须被探到且是免费：%+v", line.Models)
	}
	if !sawFlash {
		t.Fatalf("qfmodel 必须是 0.1x 非免费：%+v", line.Models)
	}
}

func TestProbeLineQoderNotAuthoritativeWhenFetchFails(t *testing.T) {
	old := probeQoderFetch
	probeQoderFetch = func() ([]qoderModelEntry, error) { return nil, errors.New("网络不通") }
	t.Cleanup(func() { probeQoderFetch = old })

	line := probeLine(&Hub{}, "qoder")
	if line.Authoritative {
		t.Fatal("抓不到官方目录时绝不能标权威，否则会把目录清空")
	}
	if len(line.Models) == 0 {
		t.Fatal("抓不到官方目录也要列内置快照，不能让用户看到空白")
	}
	for _, m := range line.Models {
		if m.Official {
			t.Fatalf("内置快照不是官方清单，不该标 Official：%+v", m)
		}
	}
}

func TestProbeLineTeleagentListsPublicModels(t *testing.T) {
	line := probeLine(&Hub{}, "teleagent")
	if !line.Authoritative {
		t.Fatal("TeleAgent 的清单是代码内维护的权威表，应标 Authoritative")
	}
	if len(line.Models) != len(teleagentPublicModels()) {
		t.Fatalf("应列出 %d 个公开模型，得到 %d：%+v", len(teleagentPublicModels()), len(line.Models), line.Models)
	}
	for _, m := range line.Models {
		if m.ID == "chat-nano" || m.ID == "tele-safety-answer" {
			t.Fatalf("隐藏/禁用的模型不该出现在探测结果：%+v", line.Models)
		}
		if !m.Official {
			t.Fatalf("权威静态表的条目要标 Official：%+v", m)
		}
	}
}

func TestProbeLineTraeAndOCAreAuthoritative(t *testing.T) {
	for _, key := range []string{"trae", "oc"} {
		line := probeLine(&Hub{}, key)
		if !line.Authoritative {
			t.Fatalf("%s 是代码内权威静态表，应标 Authoritative", key)
		}
		if len(line.Models) == 0 {
			t.Fatalf("%s 不该是空清单", key)
		}
		for _, m := range line.Models {
			if !m.Official {
				t.Fatalf("%s 的条目要标 Official：%+v", key, m)
			}
		}
	}
}
