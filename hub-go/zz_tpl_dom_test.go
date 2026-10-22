package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestZZTplDom(t *testing.T) {
	dir := domCredDirs()["国内workbuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	cases := []struct {
		sid  int
		name string
	}{
		{17, "设计"}, {19, "邮件编辑"},
	}
	for _, c := range cases {
		body := fmt.Sprintf(`{"prompt":"回复一个字","model":"hy3","conversationOrigin":"agents","tags":["scene:%d"],"addonSelection":{"welcomeMode":"working","interactionMode":"craft","sceneId":%d},"plugins":[{"id":"welcomemode-work"},{"id":"interactionmode-craft"},{"id":"prompt-common"}]}`, c.sid, c.sid)
		b, s, _ := httpPostJSON(p.Backend+"/console/as/conversations/", h, []byte(body))
		var r struct {
			Code int    `json:"code"`
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		json.Unmarshal(gunzip(b), &r)
		fmt.Printf("%s code=%d HTTP %d id=%s\n", c.name, r.Code, s, r.Data.ID)
		time.Sleep(600 * time.Millisecond)
	}
}
