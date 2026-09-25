package main

// 脱敏模块：对 system 角色里的"合规声明高频词"插入零宽空格（U+200B），
// 缓解 CodeBuddy 后端内容审核误判。对齐 desensitize.py，默认不启用。

import (
	"regexp"
	"sort"
	"strings"
)

const zwsp = "\u200b"

var sensitiveTerms = []string{
	"DoS",
	"DDoS",
	"exploit",
	"credential testing",
	"credential stuffing",
	"supply chain compromise",
	"supply-chain compromise",
	"detection evasion",
	"C2 frameworks",
	"C2 framework",
	"command and control",
	"malicious purposes",
	"malicious intent",
	"mass targeting",
	"brute force",
	"brute-force",
	"privilege escalation",
	"reverse shell",
	"remote code execution",
	"SQL injection",
	"XSS",
	"CSRF",
	"phishing",
	"malware",
	"ransomware",
	"keylogger",
	"rootkit",
	"backdoor",
	"botnet",
	"zero-day",
	"0day",
}

// 按词长降序编译大正则，避免短词先吃掉长词；忽略大小写。
var desensPattern = func() *regexp.Regexp {
	terms := append([]string{}, sensitiveTerms...)
	sort.Slice(terms, func(i, j int) bool { return len(terms[i]) > len(terms[j]) })
	for i := range terms {
		terms[i] = regexp.QuoteMeta(terms[i])
	}
	return regexp.MustCompile("(?i)" + strings.Join(terms, "|"))
}()

func desensitizeText(s string) string {
	if s == "" {
		return s
	}
	return desensPattern.ReplaceAllStringFunc(s, func(m string) string {
		if len(m) <= 1 {
			return m
		}
		return m[:1] + zwsp + m[1:]
	})
}

// desensitizeMessages 只处理指定角色（默认 system）的消息，返回新列表。
func desensitizeMessages(msgs []any, roles ...string) []any {
	roleSet := map[string]bool{}
	for _, r := range roles {
		roleSet[r] = true
	}
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		role, _ := mm["role"].(string)
		if !roleSet[role] {
			out = append(out, m)
			continue
		}
		nm := cloneMap(mm)
		switch c := mm["content"].(type) {
		case string:
			nm["content"] = desensitizeText(c)
		case []any:
			blocks := make([]any, 0, len(c))
			for _, b := range c {
				bm, ok := b.(map[string]any)
				if ok && bm["type"] == "text" {
					nb := cloneMap(bm)
					if t, ok := bm["text"].(string); ok {
						nb["text"] = desensitizeText(t)
					}
					blocks = append(blocks, nb)
				} else {
					blocks = append(blocks, b)
				}
			}
			nm["content"] = blocks
		}
		out = append(out, nm)
	}
	return out
}
