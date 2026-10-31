package main

// 控制台网页（嵌入二进制，无外部依赖）。
//
// 2026-10-13 UI 布局 v2（按 docs/2026-10-13-UI布局设计-v2.md 终稿重写）：
//   五页骨架：总览 / 线路与凭证 / 模型 / 用量 / 设置
//   一条信息只有一个家：线路详情只在「线路与凭证」，网关状态只在总览 hero，
//   维护类操作（探测/倍率/接管/上下文窗口）归「模型」与「设置」页。
//   既往拍板全部保留：额度永不自动轮询、限流不编恢复时刻、停止线路视觉降权、
//   模型限流显黄标、嵌入式单文件、深色跟随系统。
//   本文件只含 HTML/CSS/JS；后端 /api/* 一律未动。

// 2026-10-31 拆分：页面正文移到 assets/console.html，这里只留 go:embed 声明。
//
// 原先 1725 行 HTML/CSS/JS 是一个 Go 裸字符串：改一行 UI 也要动 Go 源码，
// 编辑器没有 HTML/JS 语法检查、无法单独格式化，review 时也看不出改的是样式还是逻辑。
// 拆成资源文件后仍然只有一个二进制（go:embed 编译期内联），但编辑器、格式化、
// grep 都能正常工作。变量名 webHTML 保持不变，hub_api.go 与 consolejs_test.go 无需改动。

import _ "embed"

//go:embed assets/console.html
var webHTML string
