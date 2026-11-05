package main

// Qoder 凭据变更监听（2026-11-05）。
//
// 起因：Qoder 桌面端只有一份 auth.v1.dat，换号登录时**原地覆盖**，旧号凭据当场销毁
// （2026-11-05 实测：`acct-A` 换成 `acct-B` 后，全盘找不到旧号 token）。
// 手动「存快照」能防，但人会忘 —— 而且忘记的时机恰好就是换号那一刻。
//
// 这里的做法是「先留底，再放行」：
//   - 每次轮询把当前 auth.v1.dat 的内容记到 `qoder-accounts/_last-seen.dat`（0600）；
//   - 下次轮询发现内容变了 → 先把 **_last-seen.dat 里那份（旧号）** 存成
//     `auto-<时间戳>` 快照，再把新内容写进 _last-seen.dat。
//
// 也就是说：即使你直接退出登录、换号登录，旧号也会被自动留档。
// 轮询间隔 20 秒，代价是一次 531 字节的读文件 + 比较。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	qoderVaultLastSeen = "_last-seen.dat"
	qoderVaultPollSec  = 20
)

// qoderVaultLastSeenPath 上一份见过的凭据副本（不是快照，不参与列表）。
func qoderVaultLastSeenPath() string { return filepath.Join(qoderVaultRoot(), qoderVaultLastSeen) }

// qoderVaultWatchTick 跑一轮：检测变化 → 给旧内容留档 → 更新 _last-seen。
// 返回被自动留档的快照名（没变化时为空）。
func qoderVaultWatchTick() (string, error) {
	live, err := os.ReadFile(filepath.Join(qoderAppDataDir(), "auth.v1.dat"))
	if err != nil || len(live) == 0 {
		return "", nil // 客户端没登录 / 正在写：下一轮再看
	}
	seenPath := qoderVaultLastSeenPath()
	seen, _ := os.ReadFile(seenPath)

	if len(seen) == 0 {
		// 第一次运行：没有「旧号」可救，但要把**当前**账号收进仓库，
		// 否则列表里看不到正在用的这个号（用户会以为「只有一个」）。
		if _, err := ensureCurrentSnapshotted(live); err != nil {
			logHub("qoder 当前账号入库失败：%v", err)
		}
		return "", writeFile0600(seenPath, live)
	}
	if string(seen) == string(live) {
		// 内容没变：只补一次「当前账号在不在仓库里」（首次运行后可能被删掉）。
		if _, err := ensureCurrentSnapshotted(live); err != nil {
			logHub("qoder 当前账号入库失败：%v", err)
		}
		return "", nil
	}

	// 内容变了：先把旧的那份（seen）存成快照，再更新基线。
	// 去重：同一个账号来回切时，旧内容可能已经是某份快照，不重复存。
	archived := qoderVaultFindByContent(seen)
	if archived == "" {
		name := "auto-" + time.Now().Format("20060102-150405")
		meta, err := qoderVaultStore(name, seen)
		if err != nil {
			// 旧内容解不开（换了机器/密钥环变了）→ 不留档，但仍然更新基线，
			// 免得每次轮询都重试同一个坏文件。
			_ = writeFile0600(seenPath, live)
			return "", err
		}
		archived = meta.Name
		logHub("qoder 检测到凭据变更，旧账号已自动留档：%s（%s）", meta.Name, meta.Nick)
	}
	if err := writeFile0600(seenPath, live); err != nil {
		return archived, err
	}
	// 新账号也收进仓库，这样下拉里立刻能看到两个号。
	if _, err := ensureCurrentSnapshotted(live); err != nil {
		logHub("qoder 新账号入库失败：%v", err)
	}
	return archived, nil
}

// ensureCurrentSnapshotted 保证「当前凭据」在快照仓库里有一份（按内容判重）。
// 名字优先用昵称；昵称被别的账号占了就用 nick-2、nick-3…，再不行用 auto-current-时间戳。
func ensureCurrentSnapshotted(live []byte) (string, error) {
	if len(live) == 0 {
		return "", nil
	}
	if existing := qoderVaultFindByContent(live); existing != "" {
		return existing, nil
	}
	base := ""
	if plain, _, err := qoderOpenCreds(qoderAppDataDir(), live); err == nil {
		var c qoderCreds
		if json.Unmarshal(plain, &c) == nil {
			base = strings.TrimSpace(c.User.Name)
		}
	}
	if base == "" {
		base = "auto-current-" + time.Now().Format("20060102-150405")
	}
	for i := 0; i < 20; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i+1)
		}
		if _, err := os.Stat(filepath.Join(qoderVaultRoot(), name)); err == nil {
			// 名字已被别的账号占用：试下一个
			if old, rerr := os.ReadFile(qoderVaultPath(name)); rerr == nil && string(old) == string(live) {
				return name, nil
			}
			continue
		}
		meta, err := qoderVaultStore(name, live)
		if err != nil {
			return "", err
		}
		logHub("qoder 当前账号已入库：%s（%s）", meta.Name, meta.Nick)
		return meta.Name, nil
	}
	return "", fmt.Errorf("找不到可用的快照名（昵称 %q 冲突过多）", base)
}

// startQoderVaultWatch 后台轮询。stopCh 关闭即退出（测试用）。
func startQoderVaultWatch(stop <-chan struct{}) {
	go func() {
		defer recoverGoroutine("qoder_vault_watch.go")
		// 启动时先跑一轮：把当前凭据记成基线（若已有基线且不同，会立刻留档）。
		if _, err := qoderVaultWatchTick(); err != nil {
			logHub("qoder 凭据监听首轮失败：%v", err)
		}
		t := time.NewTicker(qoderVaultPollSec * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if _, err := qoderVaultWatchTick(); err != nil {
					logHub("qoder 凭据监听：%v", err)
				}
			}
		}
	}()
}

// writeFile0600 原子写（tmp + rename），权限 0600。
func writeFile0600(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
