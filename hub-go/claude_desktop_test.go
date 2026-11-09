package main

// Claude Desktop 角色名映射的回归测试（2026-11-09）。
//
// 背景：用户报「intl 线路不咋用，能不能只用聚合口的模型」。原映射表把
// claude-sonnet-* 写死到 intl.deepseek-v4.1-flash，intl 一停 Code 模式
// 整个 503。改成「首选 + 兜底」后，这里钉死三件事：
//  1. 每个角色名都能解析出一个非空 slug；
//  2. 首选 slug 在聚合目录里时必须原样选中（不瞎兜底）；
//  3. 首选 slug 不在目录里时必须换成目录里的模型（不返回死 slug）。

import (
	"strings"
	"testing"
)

// TestDesktopRoleTableCoversAllRoles 六个角色名都能解析出非空 slug。
func TestDesktopRoleTableCoversAllRoles(t *testing.T) {
	withAggList(t, []aggModel{
		{Line: "dom", Slug: "dom.deepseek-v4-flash", Backend: "deepseek-v4-flash", Display: "DeepSeek V4 Flash（国内）"},
		{Line: "qoder", Slug: "qoder.DeepSeek-Flash", Backend: "DeepSeek-Flash", Display: "DeepSeek Flash（Qoder）"},
		{Line: "qoder", Slug: "qoder.Qwen3.8-Flash", Backend: "Qwen3.8-Flash", Display: "Qwen3.8 Flash（Qoder）"},
	})
	for _, p := range desktopRolePrefs {
		if got := resolveDesktopRole(p.role); got == "" {
			t.Errorf("角色名 %s 解析为空 slug", p.role)
		}
	}
}

// TestDesktopRolePrefersConfiguredSlug 首选 slug 在目录里时必须原样选中。
func TestDesktopRolePrefersConfiguredSlug(t *testing.T) {
	withAggList(t, []aggModel{
		{Line: "dom", Slug: "dom.deepseek-v4-flash", Backend: "deepseek-v4-flash", Display: "DeepSeek V4 Flash（国内）"},
		{Line: "qoder", Slug: "qoder.Qwen3.8-Flash", Backend: "Qwen3.8-Flash", Display: "Qwen3.8 Flash（Qoder）"},
	})
	if got := resolveDesktopRole("claude-sonnet-5"); got != "dom.deepseek-v4-flash" {
		t.Errorf("claude-sonnet-5 = %q，期望 dom.deepseek-v4-flash", got)
	}
	if got := resolveDesktopRole("claude-opus-5"); got != "qoder.Qwen3.8-Flash" {
		t.Errorf("claude-opus-5 = %q，期望 qoder.Qwen3.8-Flash", got)
	}
}

// TestDesktopRoleFallsBackWhenPreferredMissing 首选不在目录里时换成目录里的模型。
//
// 这是修 intl 停摆的核心：目录里只有 dom/qoder 的模型时，claude-sonnet-*
// 不能再返回 intl.* 那种死 slug。
func TestDesktopRoleFallsBackWhenPreferredMissing(t *testing.T) {
	withAggList(t, []aggModel{
		{Line: "teleagent", Slug: "teleagent.chat-pro", Backend: "chat-pro", Display: "chat-pro（TeleAgent）"},
	})
	for _, role := range []string{"claude-sonnet-5", "claude-sonnet-4-6", "claude-opus-5", "claude-opus-4-8", "claude-fable-5", "claude-haiku-4-5"} {
		got := resolveDesktopRole(role)
		if got != "teleagent.chat-pro" {
			t.Errorf("%s = %q，期望兜底到 teleagent.chat-pro", role, got)
		}
	}
}

// TestDesktopRoleNeverReturnsIntlWhenIntlAbsent 目录里没有 intl 模型时不许返回 intl.*。
func TestDesktopRoleNeverReturnsIntlWhenIntlAbsent(t *testing.T) {
	withAggList(t, []aggModel{
		{Line: "dom", Slug: "dom.hy4free", Backend: "hy4free", Display: "hy4free（国内）"},
	})
	for _, p := range desktopRolePrefs {
		if got := resolveDesktopRole(p.role); strings.HasPrefix(got, "intl.") {
			t.Errorf("%s 仍返回 intl slug %q，会把 Code 打成 503", p.role, got)
		}
	}
}

// TestDesktopRoleEmptyCatalogKeepsSlug 聚合目录为空时退回首选 slug（不 panic、不返回空）。
func TestDesktopRoleEmptyCatalogKeepsSlug(t *testing.T) {
	withAggList(t, nil)
	for _, p := range desktopRolePrefs {
		if got := resolveDesktopRole(p.role); got != p.slug {
			t.Errorf("空目录下 %s = %q，期望首选 %q", p.role, got, p.slug)
		}
	}
}

// TestDesktopRoleDateSuffix Desktop 可能带日期后缀（claude-haiku-4-5-20251001）。
func TestDesktopRoleDateSuffix(t *testing.T) {
	withAggList(t, []aggModel{
		{Line: "teleagent", Slug: "teleagent.chat-flagship", Backend: "chat-flagship", Display: "chat-flagship（TeleAgent）"},
	})
	if got := resolveDesktopRole("claude-haiku-4-5-20251001"); got != "teleagent.chat-flagship" {
		t.Errorf("带日期后缀解析 = %q，期望 teleagent.chat-flagship", got)
	}
}

// TestDesktopRoleUnknownModel 非角色名返回空（交给后面的 gpt-*/动态 slug 分支）。
func TestDesktopRoleUnknownModel(t *testing.T) {
	withAggList(t, []aggModel{{Line: "dom", Slug: "dom.hy4free", Backend: "hy4free"}})
	for _, m := range []string{"gpt-6.1-sol", "dom.deepseek-v4-flash", "", "  "} {
		if got := resolveDesktopRole(m); got != "" {
			t.Errorf("非角色名 %q 应返回空，实际 %q", m, got)
		}
	}
}

// TestDesktopRoleResolvesThroughAggModel 表里每个 slug 都能被 resolveAggModel 解析。
//
// 防止「映射表指向一个聚合口根本没有的 slug」这种死链。
func TestDesktopRoleResolvesThroughAggModel(t *testing.T) {
	withAggList(t, []aggModel{
		{Line: "dom", Slug: "dom.deepseek-v4-flash", Backend: "deepseek-v4-flash", Display: "DeepSeek V4 Flash（国内）"},
		{Line: "qoder", Slug: "qoder.DeepSeek-Flash", Backend: "DeepSeek-Flash", Display: "DeepSeek Flash（Qoder）"},
		{Line: "qoder", Slug: "qoder.Qwen3.8-Flash", Backend: "Qwen3.8-Flash", Display: "Qwen3.8 Flash（Qoder）"},
		{Line: "qoder", Slug: "qoder.QoDuck", Backend: "QoDuck", Display: "QoDuck（Qoder）"},
		{Line: "teleagent", Slug: "teleagent.chat-pro", Backend: "chat-pro", Display: "chat-pro（TeleAgent）"},
		{Line: "teleagent", Slug: "teleagent.chat-flagship", Backend: "chat-flagship", Display: "chat-flagship（TeleAgent）"},
	})
	for _, e := range desktopRoleTable() {
		if _, ok := resolveAggModel(e.Slug); !ok {
			t.Errorf("角色 %s 映射到 %q，但 resolveAggModel 解析不了", e.Role, e.Slug)
		}
	}
}
