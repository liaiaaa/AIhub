package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestZZTplPost(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	cases := []struct {
		sid  int
		name string
	}{
		{5, "深度研究"}, {7, "文档处理"}, {9, "数据分析"},
	}
	for _, c := range cases {
		body := fmt.Sprintf(`{"prompt":"回复一个字","model":"hy3","conversationOrigin":"agents","tags":["scene:%d"],"addonSelection":{"welcomeMode":"working","interactionMode":"craft","sceneId":%d},"plugins":[{"id":"welcomemode-work"},{"id":"interactionmode-craft"},{"id":"prompt-common"}]}`, c.sid, c.sid)
		b, s, _ := httpPostJSON(p.Backend+"/console/as/conversations/", h, []byte(body))
		var r struct {
			Code int `json:"code"`
		}
		json.Unmarshal(gunzip(b), &r)
		fmt.Printf("%s POST code=%d HTTP %d\n", c.name, r.Code, s)
		time.Sleep(700 * time.Millisecond)
	}
}
