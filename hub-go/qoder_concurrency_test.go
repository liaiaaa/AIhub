package main

// 并发压力测试：验证「目录热替换」与「并发读取」之间没有数据竞争 / panic。
//
// 背景：Start() 会在 goroutine 里异步刷新目录（setCatalog 写），
// 同时任意数量的请求会并发读（resolveModel / catalogSnapshot / handleModels）。
// 本机无 gcc，-race 不可用，因此用高频读写 + 交叉校验来暴露问题。
// 若需要更强的竞态检测，可在有 C 编译器的环境执行：
//
//	CGO_ENABLED=1 go test -race -run TestCatalogConcurrentHotSwap -v

import (
	"net/http/httptest"
	"sync"
	"testing"
)

func TestCatalogConcurrentHotSwap(t *testing.T) {
	p := NewQoderProxy()

	// 两份交替替换的目录，用于制造持续热替换
	catA := []qoderModelEntry{
		{Key: "alpha", DisplayName: "Alpha-Model", Vendor: "qoder-cn"},
		{Key: "beta", DisplayName: "Beta-Model", Vendor: "zhipu"},
	}
	catB := []qoderModelEntry{
		{Key: "gamma", DisplayName: "Gamma-Model", Vendor: "deepseek"},
		{Key: "delta", DisplayName: "Delta-Model", Vendor: "alibaba"},
	}

	const (
		writers = 4
		readers = 8
		rounds  = 400
	)

	// 写者与读者分开计数：读者只有在 close(stop) 之后才退出，
	// 若共用一个 WaitGroup，Wait 会永远等不到读者 → 死锁。
	var writersWG sync.WaitGroup
	var readersWG sync.WaitGroup
	stop := make(chan struct{})

	// 写者：不断热替换目录
	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func(id int) {
			defer writersWG.Done()
			for i := 0; i < rounds; i++ {
				if i%2 == 0 {
					p.setCatalog(catA)
				} else {
					p.setCatalog(catB)
				}
			}
		}(w)
	}

	// 读者：并发解析 + 快照 + 渲染 /v1/models（收到 stop 后退出）
	for r := 0; r < readers; r++ {
		readersWG.Add(1)
		go func(id int) {
			defer readersWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// 解析（可能命中任一份目录，也可能都未命中→透传）
				_ = p.resolveModel("Alpha-Model")
				_ = p.resolveModel("gamma")
				_ = p.resolveModel("GLM-5.3")
				_ = p.resolveModel("不存在的模型")

				// 快照必须始终非空（setCatalog 不会清空目录）
				snap := p.catalogSnapshot()
				if len(snap) == 0 {
					t.Errorf("catalogSnapshot 返回空目录（不应发生）")
					return
				}

				// 渲染 /v1/models 不应 panic
				rec := httptest.NewRecorder()
				p.handleModels(rec, httptest.NewRequest("GET", "/v1/models", nil))
				if rec.Code != 200 {
					t.Errorf("handleModels status = %d, want 200", rec.Code)
					return
				}
			}
		}(r)
	}

	// 先等写者完成，再通知读者停止、等读者退出（顺序不能反，否则死锁）
	writersWG.Wait()
	close(stop)
	readersWG.Wait()

	// 收尾：目录必须处于两份之一且非空
	final := p.catalogSnapshot()
	if len(final) != 2 {
		t.Fatalf("最终目录条目数 = %d, want 2", len(final))
	}
	if final[0].Key != "alpha" && final[0].Key != "gamma" {
		t.Fatalf("最终目录内容异常：%+v", final)
	}

	// 热替换后别名表必须与目录一致（不能出现"目录换了但别名没换"）
	got := p.resolveModel(final[0].DisplayName)
	if got != final[0].Key {
		t.Errorf("热替换后别名解析不一致：resolveModel(%q) = %q, want %q",
			final[0].DisplayName, got, final[0].Key)
	}
}
