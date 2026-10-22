package main

import (
	"bufio"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestZZCbStream(t *testing.T) {
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	req, _ := http.NewRequest("POST", p.Backend+"/v2/chat/completions",
		strings.NewReader(`{"model":"hy3","messages":[{"role":"user","content":"说一个字"}],"stream":true,"max_tokens":8}`))
	for k, v := range h {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	cli := &http.Client{Timeout: 40 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		fmt.Println("STREAM ERR:", err)
		return
	}
	defer resp.Body.Close()
	fmt.Printf("STREAM HTTP %d\n", resp.StatusCode)
	sc := bufio.NewScanner(resp.Body)
	n := 0
	for sc.Scan() && n < 12 {
		ln := sc.Text()
		if strings.Contains(ln, "risk") || strings.Contains(ln, "风控") || strings.Contains(ln, "4003") || strings.Contains(ln, "6004") || strings.Contains(ln, "delta") || strings.Contains(ln, "error") {
			fmt.Println("  ", ln[:min(160, len(ln))])
			n++
		}
	}
}
