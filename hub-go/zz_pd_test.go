package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestZZPd(t *testing.T) {
	dir := domCredDirs()["国内workbuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	body := `{"prompt":"给我展示1张捷风的拼豆图纸样例","model":"hy3","conversationOrigin":"agents","manifest":{"envs":[{"key":"X_EXPERT_ID","value":"ex_xQHHD1u5Jkua"},{"key":"X_EXPERT_LOCALE","value":"zh"}]}}`
	b, s, _ := httpPostJSON(p.Backend+"/console/as/conversations/", h, []byte(body))
	ds := string(gunzip(b))
	var r struct {
		Code int    `json:"code"`
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal([]byte(ds), &r)
	fmt.Printf("POST code=%d HTTP %d id=%s\n", r.Code, s, r.Data.ID)
	if r.Data.ID != "" {
		time.Sleep(6 * time.Second)
		d, _, _ := httpGetJSON(p.Backend+"/console/as/conversations/"+r.Data.ID, h)
		dd := string(gunzip(d))
		fmt.Println("绑定验证 X_EXPERT_ID:", strings.Contains(dd, "ex_xQHHD1u5Jkua"))
	}
}
