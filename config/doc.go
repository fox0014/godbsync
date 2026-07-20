// Package config 定义 godbsync 同步任务的配置模型与校验、DSN 生成逻辑。
//
// 顶层 Config 聚合 source/target 两个 DBConfig（含 dialect、连接信息、schema/database）
// 与 tables 列表；每张表可独立指定同步模式（full/incremental）、批量参数、水位列等。
// Validate 在运行前做一次集中校验，DSN/各方言专用 DSN 由各方言分支生成。
package config
