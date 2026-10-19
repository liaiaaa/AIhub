package main

// 2026-10-15 回归：失效代理不得拖垮整条线路。
//
// 背景：进程启动时继承的 https_proxy 指向已关闭的本地端口时，intl 线路
// 所有请求都 `proxyconnect tcp: ... connection refused` → 502 全线不可用。
// 现在代理地址先做 TCP 可达性探测，不可达就回落直连。

import (
	"net"
	"net/url"
	"testing"
	"time"
)

func TestProxyReachableRejectsDeadPort(t *testing.T) {
	// 挑一个确定没人监听的端口（用 1 号端口，非 root 不可能占用）
	u := &url.URL{Scheme: "http", Host: "127.0.0.1:1"}
	if proxyReachable(u) {
		t.Fatal("127.0.0.1:1 不该被判为可达")
	}
}

func TestProxyReachableAcceptsLiveListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听器失败：%v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	u := &url.URL{Scheme: "http", Host: addr}
	if !proxyReachable(u) {
		t.Fatalf("正在监听的 %s 应判为可达", addr)
	}
}

// 探测结果带 TTL：过期后重新探测，代理恢复能被发现。
func TestProxyReachableCacheExpires(t *testing.T) {
	u := &url.URL{Scheme: "http", Host: "127.0.0.1:1"}
	if proxyReachable(u) {
		t.Fatal("初次应为不可达")
	}
	// 手动把缓存时间拨老，模拟 TTL 过期
	proxyAliveMu.Lock()
	proxyAliveCache["127.0.0.1:1"] = proxyAliveEntry{alive: false, at: time.Now().Add(-2 * proxyAliveTTL)}
	proxyAliveMu.Unlock()

	// 起一个真的监听器占用不了 1 号端口，这里只验证「过期后会重新探测」：
	// 重新探测仍不可达，但缓存时间被刷新
	proxyReachable(u)
	proxyAliveMu.Lock()
	at := proxyAliveCache["127.0.0.1:1"].at
	proxyAliveMu.Unlock()
	if time.Since(at) > time.Second {
		t.Fatal("TTL 过期后应重新探测并刷新时间戳")
	}
}

// 系统代理字段为空时不误判为可达。
func TestProxyReachableEmptyHost(t *testing.T) {
	if proxyReachable(&url.URL{Scheme: "http"}) {
		t.Fatal("空 Host 不该判为可达")
	}
	if proxyReachable(nil) {
		t.Fatal("nil 不该判为可达")
	}
}
