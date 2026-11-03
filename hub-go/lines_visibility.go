package main

// 线路显隐（2026-11-03 用户要求：不增删线路，只把暂时用不到的隐藏起来）。
//
// 用户原话：「就基于我目前已有的线路就行，就比如我这一个月用不到 mimo 的，
// 我就可以把 mimo 那条线路隐藏起来」「删除线路只是视觉上看不到他了，就是给他
// 隐藏起来就行，变成不能启动不能操作的，然后在主页面看不到的」。
//
// 语义（隐藏 = 隐藏，不是删除，配置文件与凭据一律不动）：
//   - 控制台主页 / 线路页不显示这条线路的磁贴
//   - 不能启动（POST /api/<line>/start 直接拒），已在跑的在隐藏时先停掉
//   - 不参与「重启后自动恢复」（last_lines 过滤）
//   - 不参与聚合：模型不进 /v1/models，也不能被加进聚合白名单
//
// 存储：数据根 `lines.json`，形如 {"hidden":["mimo"]}；0600、tmp+rename 原子写。
// 只存「隐藏了谁」——内置线路表仍在代码里（lineSpecs），不存在配置文件里，
// 这样升级带新线路时老配置不会把它挡掉。

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
)

const linesConfigFile = "lines.json"

// linesConfig 是 lines.json 的结构。
type linesConfig struct {
	Hidden []string `json:"hidden"`
}

var linesVisState = struct {
	mu   sync.RWMutex
	set  map[string]bool
	load bool
}{}

// hiddenLines 返回当前隐藏的线路集合（只读副本）。首次调用读盘，之后走内存。
func hiddenLines() map[string]bool {
	linesVisState.mu.RLock()
	if linesVisState.load {
		out := make(map[string]bool, len(linesVisState.set))
		for k, v := range linesVisState.set {
			out[k] = v
		}
		linesVisState.mu.RUnlock()
		return out
	}
	linesVisState.mu.RUnlock()

	linesVisState.mu.Lock()
	defer linesVisState.mu.Unlock()
	if !linesVisState.load {
		linesVisState.set = loadHiddenLinesFromDisk()
		linesVisState.load = true
	}
	out := make(map[string]bool, len(linesVisState.set))
	for k, v := range linesVisState.set {
		out[k] = v
	}
	return out
}

// lineHidden 某条线路是否被隐藏。
func lineHidden(key string) bool {
	linesVisState.mu.RLock()
	if linesVisState.load {
		v := linesVisState.set[key]
		linesVisState.mu.RUnlock()
		return v
	}
	linesVisState.mu.RUnlock()
	return hiddenLines()[key]
}

// loadHiddenLinesFromDisk 读 lines.json。文件不存在/损坏都当成「没有隐藏任何线路」，
// 并顺手过滤掉已知线路表里不存在的名字（用户手改配置写错名字不该影响启动）。
func loadHiddenLinesFromDisk() map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(dataPath(linesConfigFile))
	if err != nil {
		return out
	}
	var cfg linesConfig
	if json.Unmarshal(b, &cfg) != nil {
		logHub("lines.json 解析失败，按「没有隐藏线路」处理")
		return out
	}
	known := map[string]bool{}
	for _, k := range lineKeys() {
		known[k] = true
	}
	for _, k := range cfg.Hidden {
		if known[k] && k != "" {
			out[k] = true
		}
	}
	return out
}

// setLineHidden 隐藏/显示一条线路并落盘。返回错误说明（空 = 成功）。
//
// 隐藏正在运行的线路时调用方应先把线路停掉（本函数只管配置，不碰运行态）。
func setLineHidden(key string, hidden bool) error {
	known := false
	for _, k := range lineKeys() {
		if k == key {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("未知线路：%s", key)
	}

	linesVisState.mu.Lock()
	if !linesVisState.load {
		linesVisState.set = loadHiddenLinesFromDisk()
		linesVisState.load = true
	}
	if hidden {
		linesVisState.set[key] = true
	} else {
		delete(linesVisState.set, key)
	}
	keys := make([]string, 0, len(linesVisState.set))
	for k := range linesVisState.set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	linesVisState.mu.Unlock()

	return saveHiddenLines(keys)
}

// saveHiddenLines 原子写 lines.json（tmp + rename，0600）。
func saveHiddenLines(keys []string) error {
	b, err := json.MarshalIndent(linesConfig{Hidden: keys}, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	path := dataPath(linesConfigFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		logHub("写 lines.json 失败：%v", err)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		logHub("替换 lines.json 失败：%v", err)
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// visibleLineKeys 未被隐藏的线路（按 lineSpecs 顺序）。用于启动时恢复、
// 聚合白名单校验、控制台渲染。
func visibleLineKeys() []string {
	hidden := hiddenLines()
	out := make([]string, 0, len(lineSpecs))
	for _, s := range lineSpecs {
		if !hidden[s.Key] {
			out = append(out, s.Key)
		}
	}
	return out
}

// lineVisibilityErr 是「线路被隐藏」的统一提示文案。
func lineVisibilityErr(key string) string {
	return "线路已隐藏（在「线路与凭证」页点「显示」恢复）：" + key
}
