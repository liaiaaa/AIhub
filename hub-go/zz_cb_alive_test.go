package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestZZCbAlive(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, err := cm.Headers()
	if err != nil {
		t.Fatalf("凭证刷新失败: %v", err)
	}
	b, s, _ := httpGetJSON(p.Backend+"/activity/growth/energy", h)
	var env struct {
		Data struct {
			Balance int `json:"balance"`
		} `json:"data"`
	}
	json.Unmarshal(b, &env)
	fmt.Printf("CodeBuddy号 HTTP %d 能量余额=%d\n", s, env.Data.Balance)
}
