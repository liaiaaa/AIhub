package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestZZZk(t *testing.T) {
	dir := domCredDirs()["国内workbuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	// 收集昨天卡 working 的会话 id
	var ids []string
	var names []string
	for page := 1; page <= 2; page++ {
		b, _, _ := httpGetJSON(p.Backend+fmt.Sprintf("/console/as/conversations/?limit=20&page=%d", page), h)
		var r struct {
			Data struct {
				Conversations []struct {
					ID     string `json:"id"`
					Name   string `json:"name"`
					Status string `json:"status"`
				} `json:"conversations"`
			} `json:"data"`
		}
		json.Unmarshal(gunzip(b), &r)
		for _, c := range r.Data.Conversations {
			old := strings.Contains(c.Name, "拼豆图纸") || strings.Contains(c.Name, "吉事通") ||
				strings.Contains(c.Name, "适合支") || strings.Contains(c.Name, "高考真题") ||
				strings.Contains(c.Name, "拉起专家团执行：写一份月初") || strings.Contains(c.Name, "下载2026年")
			if old && c.Status == "working" {
				ids = append(ids, c.ID)
				names = append(names, c.Name[:min(12, len([]rune(c.Name)))])
			}
		}
	}
	fmt.Printf("候选删除 %d 条\n", len(ids))
	for i, id := range ids {
		req, _ := http.NewRequest("POST", p.Backend+"/console/as/conversations/"+id+"/delete", strings.NewReader(""))
		for k, v := range h {
			req.Header.Set(k, v)
		}
		resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		nm := names[i]
		if len([]rune(nm)) > 12 {
			nm = string([]rune(nm)[:12])
		}
		fmt.Printf("DEL %s => %v %d\n", nm, err, resp.StatusCode)
		resp.Body.Close()
		time.Sleep(300 * time.Millisecond)
	}
}
