package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestZZPd2(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["国内workbuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	bodies := []string{
		`{"prompt":"给我展示1张捷风的拼豆图纸样例","model":"hy3","mode":"working","conversationOrigin":"agents","expertId":"ex_xQHHD1u5Jkua","expert":{"id":"ex_xQHHD1u5Jkua","locale":"zh"}}`,
		`{"prompt":"给我展示1张捷风的拼豆图纸样例","model":"hy3","conversationOrigin":"agents","expertId":"ex_xQHHD1u5Jkua","expertLocale":"zh"}`,
	}
	for i, body := range bodies {
		b, s, _ := httpPostJSON(p.Backend+"/console/as/conversations/", h, []byte(body))
		ds := string(gunzip(b))
		var r struct {
			Code int    `json:"code"`
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		json.Unmarshal([]byte(ds), &r)
		fmt.Printf("v%d code=%d HTTP %d id=%s\n", i, r.Code, s, r.Data.ID)
		if r.Data.ID != "" {
			time.Sleep(5 * time.Second)
			d, _, _ := httpGetJSON(p.Backend+"/console/as/conversations/"+r.Data.ID, h)
			fmt.Printf("v%d 绑定=%v\n", i, strings.Contains(string(gunzip(d)), "ex_xQHHD1u5Jkua"))
			if strings.Contains(string(gunzip(d)), "ex_xQHHD1u5Jkua") {
				return
			}
		}
	}
}
