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
