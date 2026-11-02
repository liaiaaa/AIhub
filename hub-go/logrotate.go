package main

// 日志轮转（2026-11-02 用户要求「轮转加上吧」）。
//
// 背景：`hub.log` 与各线路的 `converter-*.log` 原来是一个文件无限追加，
// 实测 converter-qoder.log 已经 18MB、converter-trae.log 也有几十万行。
// 出问题翻日志要 tail 半天，磁盘也只增不减。
//
// 这里做的轮转很朴素，不引第三方库、不依赖 logrotate：
//
//	<name>.log 超过阈值 → 改名 <name>.log.1
//	原 <name>.log.1 顺延为 .2，以此类推，超过保留数的最老一个直接删除
//
// 阈值与保留数可用环境变量调：
//
//	AIHUB_LOG_MAX_MB  单个日志文件上限（默认 10MB）
//	AIHUB_LOG_KEEP    保留几个历史文件（默认 3，即 .1/.2/.3）
//
// 说明：轮转是「写入时检查」，不是定时任务 —— 没有新日志就不会触发，
// 但对排障场景（正在写、写完看）已经够用，也省了一个后台 goroutine。

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	logMaxMBDefault = 10
	logKeepDefault  = 3
)

// logMaxBytesHook 让测试能把阈值调到几百字节（MB 粒度没法在单测里触发）。
var logMaxBytesHook = func() int64 {
	mb := logMaxMBDefault
	if v := strings.TrimSpace(os.Getenv("AIHUB_LOG_MAX_MB")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			mb = n
		}
	}
	return int64(mb) << 20
}

// logKeepHook 同理，测试可覆盖保留份数。
var logKeepHook = func() int {
	if v := strings.TrimSpace(os.Getenv("AIHUB_LOG_KEEP")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return logKeepDefault
}

// rotateLogFile 把 path 滚动一份：path → path.1 → path.2 …，多余的删除。
// 调用方负责保证没有并发写在同一个文件上（FileLogger 有锁）。
func rotateLogFile(path string) {
	keep := logKeepHook()
	if keep < 1 {
		keep = 1
	}
	// 最老的一份先删，给顺延腾位置
	_ = os.Remove(fmt.Sprintf("%s.%d", path, keep))
	for i := keep - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", path, i)
		dst := fmt.Sprintf("%s.%d", path, i+1)
		if _, err := os.Stat(src); err == nil {
			_ = os.Rename(src, dst)
		}
	}
	_ = os.Rename(path, path+".1")
}

// rotateIfOversize 在「每次打开写」的路径上检查一次体积（logHub 用）。
func rotateIfOversize(path string) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() < logMaxBytesHook() {
		return
	}
	rotateLogFile(path)
}
