package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"io"
)

func TestZZCb403(t *testing.T) {
	for _, cred := range []string{"CodeBuddy凭证", "国内workbuddy凭证"} {
		dir := domCredDirs()[cred]
		p := NewDomProxyDir(dir)
		cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
		h, _ := cm.Headers()
		req, _ := http.NewRequest("POST", p.Backend+"/v2/chat/completions",
			strings.NewReader(`{"model":"hy3","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":8}`))
		for k, v := range h { req.Header.Set(k, v) }
		req.Header.Set("Content-Type", "application/json")
		cli := &http.Client{Timeout: 30 * time.Second}
		resp, err := cli.Do(req)
		if err != nil { fmt.Println(cred, "ERR", err); continue }
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if len(body) > 260 { body = body[:260] }
		fmt.Printf("%s: HTTP %d %s\n", cred, resp.StatusCode, strings.ReplaceAll(string(body), "\n", " | "))
	}
}
