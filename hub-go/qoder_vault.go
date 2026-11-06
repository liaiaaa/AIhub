package main

// Qoder 账号快照（2026-11-05）。
//
// 背景：Qoder 线路是「单账号」结构 —— 每次启动都解
// `~/.config/com.qodercn.app.stable/auth.v1.dat`。用户在桌面端换号登录时，
// 这个文件被整体覆盖，**旧号的凭据就此丢失**（2026-11-05 实测：注册新号后
// 旧号无法找回，只能重新登录）。
//
// 这里给一份「快照仓库」：把当前的 auth.v1.dat 复制到
// `<数据根>/qoder-accounts/<名字>/auth.v1.dat`，并记下昵称/uid/到期时间，
// 需要时再写回去。
//
// 加密说明：文件本身是 Electron safeStorage 信封（Linux 上是
// keyring 口令 + PBKDF2 + AES-CBC，见 proxy_qoder_oskey_other.go），
// 密钥与「本机 + 本用户」绑定。所以快照**只在同一台机器的同一用户下可用**，
// 换机或换用户都解不开 —— 这不是缺陷，是这套加密的边界。
//
// 切换前必须关掉 Qoder 桌面端：它退出时会把自己内存里的登录态写回
// auth.v1.dat，覆盖我们刚切过去的那份。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const qoderVaultDirName = "qoder-accounts"

// qoderAccountMeta 一份快照的元信息（存 <名字>/meta.json）。
type qoderAccountMeta struct {
	Name      string `json:"name"`
	Nick      string `json:"nick"`
	UID       string `json:"uid"`
	Email     string `json:"email"`
	ExpiresAt string `json:"expiresAt"`
	SavedAt   string `json:"savedAt"`
	Size      int    `json:"size"`
	// Current 仅用于列表展示：这份快照与当前桌面端文件字节一致。
	Current bool `json:"current,omitempty"`
}

func qoderVaultRoot() string { return dataPath(qoderVaultDirName) }

// qoderVaultPath 返回某份快照的 auth.v1.dat 路径。
func qoderVaultPath(name string) string {
	return filepath.Join(qoderVaultRoot(), name, "auth.v1.dat")
}

// sanitizeVaultName 只允许安全字符，避免 ../ 之类跑出数据根。
func sanitizeVaultName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("快照名不能为空")
	}
	for _, r := range name {
		ok := r == '-' || r == '_' || r == '.' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= 0x4e00 && r <= 0x9fff) // 允许中文名
		if !ok {
			return "", fmt.Errorf("快照名 %q 含非法字符（只允许中英文、数字、- _ .）", name)
		}
	}
	return name, nil
}

// qoderVaultList 列出全部快照，按保存时间倒序。
func qoderVaultList() ([]qoderAccountMeta, error) {
	root := qoderVaultRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	live, _ := os.ReadFile(filepath.Join(qoderAppDataDir(), "auth.v1.dat"))
	var out []qoderAccountMeta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, "auth.v1.dat"))
		if err != nil {
			continue
		}
		meta := qoderAccountMeta{Name: e.Name(), Size: len(b)}
		if mb, err := os.ReadFile(filepath.Join(dir, "meta.json")); err == nil {
			_ = json.Unmarshal(mb, &meta)
			meta.Name = e.Name() // 目录名是权威
		}
		meta.Current = len(live) > 0 && string(live) == string(b)
		out = append(out, meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SavedAt > out[j].SavedAt })
	return out, nil
}

// qoderVaultSave 把**当前**桌面端凭据存成一份快照。
func qoderVaultSave(name string) (qoderAccountMeta, error) {
	livePath := filepath.Join(qoderAppDataDir(), "auth.v1.dat")
	b, err := os.ReadFile(livePath)
	if err != nil {
		return qoderAccountMeta{}, fmt.Errorf("读不到桌面端凭据（%s）：%w", livePath, err)
	}
	return qoderVaultStore(name, b)
}

// qoderVaultStore 把**给定字节**存成一份快照（同名覆盖前留 .bak-时间戳）。
// 与 qoderVaultSave 的区别：这份字节不必是当前文件 —— 变更监听用它保存
// 「刚刚被覆盖掉的旧凭据」。
func qoderVaultStore(name string, b []byte) (qoderAccountMeta, error) {
	name, err := sanitizeVaultName(name)
	if err != nil {
		return qoderAccountMeta{}, err
	}
	// 先确认这份凭据真能解开，否则存进去也没用。
	plain, _, err := qoderOpenCreds(qoderAppDataDir(), b)
	if err != nil {
		return qoderAccountMeta{}, fmt.Errorf("当前凭据解不开，不保存：%w", err)
	}
	var creds qoderCreds
	_ = json.Unmarshal(plain, &creds)

	dir := filepath.Join(qoderVaultRoot(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return qoderAccountMeta{}, err
	}
	dst := filepath.Join(dir, "auth.v1.dat")
	if old, err := os.ReadFile(dst); err == nil && len(old) > 0 {
		bak := dst + ".bak-" + time.Now().Format("20060102-150405")
		_ = os.WriteFile(bak, old, 0o600)
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return qoderAccountMeta{}, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return qoderAccountMeta{}, err
	}

	meta := qoderAccountMeta{
		Name:      name,
		Nick:      creds.User.Name,
		UID:       creds.User.ID,
		Email:     creds.User.Email,
		ExpiresAt: creds.ExpiresAt,
		SavedAt:   time.Now().Format("2006-01-02 15:04:05"),
		Size:      len(b),
	}
	mb, _ := json.MarshalIndent(meta, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "meta.json"), mb, 0o600)
	return meta, nil
}

// qoderVaultFindByContent 找一份与给定字节完全相同的快照；没有则返回空串。
// 用于去重：同一个账号反复切换时不该每次都产生一份 auto-*。
func qoderVaultFindByContent(b []byte) string {
	root := qoderVaultRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		old, err := os.ReadFile(filepath.Join(root, e.Name(), "auth.v1.dat"))
		if err == nil && len(old) == len(b) && string(old) == string(b) {
			return e.Name()
		}
	}
	return ""
}

// qoderVaultFindByUID 找同一账号（uid 相同）最新的快照名；没有返回空串。
//
// 为什么按 uid 而不是按内容判重（2026-11-06 用户报「实际就 2 个凭证，为什么显示四个」）：
// token 每次刷新字节都变，按内容判重会把**同一个账号**存成多份快照
// （实测仓库里 nick5461534038 有 3 份：本体 + -2 + auto-20261106-172045，uid 全同）。
// 一个账号只该有一条，刷新时覆盖更新即可。
func qoderVaultFindByUID(uid string) string {
	if uid == "" {
		return ""
	}
	all, err := qoderVaultList()
	if err != nil {
		return ""
	}
	for _, a := range all { // qoderVaultList 已按 SavedAt 倒序 → 第一个就是最新的
		if a.UID == uid {
			return a.Name
		}
	}
	return ""
}

// qoderVaultDedupeByUID 同一 uid 只保留最新一份（入参需已按 SavedAt 倒序）。
// uid 为空的条目原样保留 —— 没有身份就没法判重，宁可多显示一条也不要误删。
func qoderVaultDedupeByUID(in []qoderAccountMeta) []qoderAccountMeta {
	seen := make(map[string]bool, len(in))
	out := make([]qoderAccountMeta, 0, len(in))
	for _, a := range in {
		if a.UID != "" {
			if seen[a.UID] {
				continue
			}
			seen[a.UID] = true
		}
		out = append(out, a)
	}
	return out
}

// qoderVaultNameForLive 给「桌面端当前凭据」挑一个快照名：
// 同 uid 已有快照 → 复用那个名字（覆盖更新）；否则用昵称，昵称被占了用 nick-2/-3；再不行 auto-时间戳。
func qoderVaultNameForLive(b []byte) string {
	plain, _, err := qoderOpenCreds(qoderAppDataDir(), b)
	if err != nil {
		return ""
	}
	var creds qoderCreds
	_ = json.Unmarshal(plain, &creds)
	if n := qoderVaultFindByUID(creds.User.ID); n != "" {
		return n
	}
	return ""
}

// qoderVaultPruneDupes 清理历史遗留的重复快照：同一 uid 只留最新一份。
//
// 保守规则（宁可留错也不误删）：
//   - uid 为空的不参与（判不了身份）；
//   - 只删 auto-* / 带 -2/-3 后缀的**旧**副本；用昵称建的原始条目即使不是最新也不动，
//     避免把用户手工存的快照删掉。
func qoderVaultPruneDupes() int {
	all, err := qoderVaultList()
	if err != nil {
		return 0
	}
	// 按 uid 分组：qoderVaultList 已按 SavedAt 倒序，每组第一条即最新
	type grp struct {
		keep   qoderAccountMeta
		others []qoderAccountMeta
	}
	groups := map[string]*grp{}
	order := []string{}
	for _, a := range all {
		if a.UID == "" {
			continue
		}
		g := groups[a.UID]
		if g == nil {
			groups[a.UID] = &grp{keep: a}
			order = append(order, a.UID)
			continue
		}
		g.others = append(g.others, a)
	}
	// 昵称占用表（非 auto-* 的条目占着可读名）
	taken := map[string]bool{}
	for _, a := range all {
		if !strings.HasPrefix(a.Name, "auto-") && a.Nick != "" {
			taken[a.Nick] = true
		}
	}
	changed := 0
	isDupName := func(name string) bool {
		if strings.HasPrefix(name, "auto-") {
			return true
		}
		if i := strings.LastIndex(name, "-"); i > 0 {
			return isAllDigits(name[i+1:])
		}
		return false
	}
	for _, uid := range order {
		g := groups[uid]
		nick := g.keep.Nick
		// 1) 删同账号旧副本：auto-* / 数字后缀 / 名字就是昵称但已被新版取代的那条
		for _, a := range g.others {
			if !isDupName(a.Name) && a.Name != nick {
				continue // 手工起的、和昵称不同的名字：不动
			}
			if err := qoderVaultDelete(a.Name); err != nil {
				logHub("qoder 重复快照 %s 清理失败：%v", a.Name, err)
				continue
			}
			changed++
			if a.Name == nick {
				taken[nick] = false
			}
			logHub("qoder 清理重复快照：%s（账号 %s 保留更新的那份 %s）", a.Name, uid, g.keep.Name)
		}
		// 2) 留下的若是 auto-*，且昵称已无人占用 → 改名成可读昵称
		if strings.HasPrefix(g.keep.Name, "auto-") && nick != "" && !taken[nick] {
			if err := qoderVaultRename(g.keep.Name, nick); err != nil {
				logHub("qoder 快照改名 %s → %s 失败：%v", g.keep.Name, nick, err)
			} else {
				changed++
				taken[nick] = true
				logHub("qoder 快照改名：%s → %s（同账号只剩这一份，用可读昵称）", g.keep.Name, nick)
			}
		}
	}
	return changed
}

// qoderVaultRename 把一份快照改名（目录 + meta.json 里的 name）。
func qoderVaultRename(from, to string) error {
	from, err := sanitizeVaultName(from)
	if err != nil {
		return err
	}
	to, err = sanitizeVaultName(to)
	if err != nil {
		return err
	}
	if from == to || to == "" {
		return nil
	}
	src := filepath.Join(qoderVaultRoot(), from)
	dst := filepath.Join(qoderVaultRoot(), to)
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("目标名 %q 已存在", to)
	}
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	mp := filepath.Join(dst, "meta.json")
	mb, err := os.ReadFile(mp)
	if err != nil {
		return nil // 目录已改名成功，meta 缺失不影响可用性
	}
	var meta qoderAccountMeta
	if json.Unmarshal(mb, &meta) != nil {
		return nil
	}
	meta.Name = to
	if nb, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = os.WriteFile(mp, nb, 0o600)
	}
	return nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// qoderVaultUse 把某份快照写回桌面端凭据文件（切换账号）。
// 当前凭据若还没快照过，会先自动存一份 `auto-<时间戳>`，避免「切过去就丢」。
func qoderVaultUse(name string) (string, error) {
	name, err := sanitizeVaultName(name)
	if err != nil {
		return "", err
	}
	src := qoderVaultPath(name)
	b, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("快照 %q 不存在（%s）", name, src)
	}
	if _, _, err := qoderOpenCreds(qoderAppDataDir(), b); err != nil {
		return "", fmt.Errorf("快照 %q 解不开（换机器/换用户后密钥不同）：%w", name, err)
	}

	liveDir := qoderAppDataDir()
	livePath := filepath.Join(liveDir, "auth.v1.dat")
	// 先把当前这份存下来，防止「切走就找不回」——这正是当初丢号的原因。
	autoName := "auto-" + time.Now().Format("20060102-150405")
	backedUp := false
	if cur, err := os.ReadFile(livePath); err == nil && len(cur) > 0 {
		if string(cur) != string(b) {
			// 当前这份（要切走的号）按 uid 判重：同账号已有快照就覆盖它，
			// 不再新增一条 —— 否则每切一次号，列表就多一条同账号的快照。
			if dst := qoderVaultNameForLive(cur); dst != "" {
				autoName = dst
			}
			if _, err := qoderVaultSave(autoName); err != nil {
				return "", fmt.Errorf("切换前自动备份当前凭据失败，已中止：%w", err)
			}
			backedUp = true
		}
	}
	tmp := livePath + ".tmp-hubswitch"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, livePath); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if backedUp {
		return fmt.Sprintf("已切到快照 %q（原凭据已备份为 %q）", name, autoName), nil
	}
	return fmt.Sprintf("已切到快照 %q（与原凭据相同，未重复备份）", name), nil
}

// qoderVaultDelete 删除一份快照（快照内的 .bak 一并删）。
func qoderVaultDelete(name string) error {
	name, err := sanitizeVaultName(name)
	if err != nil {
		return err
	}
	dir := filepath.Join(qoderVaultRoot(), name)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("快照 %q 不存在", name)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
	return os.Remove(dir)
}

// qoderVaultCurrent 当前桌面端凭据的元信息（用于列表里标出「当前」）。
func qoderVaultCurrent() (qoderAccountMeta, error) {
	livePath := filepath.Join(qoderAppDataDir(), "auth.v1.dat")
	b, err := os.ReadFile(livePath)
	if err != nil {
		return qoderAccountMeta{}, err
	}
	plain, _, err := qoderOpenCreds(qoderAppDataDir(), b)
	if err != nil {
		return qoderAccountMeta{}, err
	}
	var creds qoderCreds
	_ = json.Unmarshal(plain, &creds)
	return qoderAccountMeta{
		Name:      "(当前)",
		Nick:      creds.User.Name,
		UID:       creds.User.ID,
		Email:     creds.User.Email,
		ExpiresAt: creds.ExpiresAt,
		Size:      len(b),
		Current:   true,
	}, nil
}
