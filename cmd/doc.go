// Package cmd 实现 godbsync 的命令行入口与各子命令。
//
// 子命令包括：init（生成配置模板）、full（全量同步）、incr（增量同步）、
// daemon（常驻守护）、validate（一致性校验）、status（状态查看）、
// checkpoint（检查点操作）、monitor（Web 可视化看板）、version（版本号）。
// 包内按方言名在 init() 中 blank-import 各方言包，从而把 Reader/Writer 注册进 dialect 注册表。
package cmd
