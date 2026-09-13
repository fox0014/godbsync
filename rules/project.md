---
description: godbsync 开发规范与架构约束
globs: **/*.go,**/*.yaml
alwaysApply: true
---

# godbsync 开发规则

纯 Go 的 Oracle→MySQL 同步 CLI。零 Oracle 客户端（go-ora/v2）；MySQL 用 go-sql-driver。入口 `main.go`→`cmd.Execute()`。

## 命令
```bash
go build -ldflags="-s -w" -o godbsync.exe .
go test ./... / go vet ./... / gofmt -l .
```
子命令：`init` / `full` / `incr` / `daemon` / `validate` / `version`。

## 架构（勿越层）
`cmd` → `syncer`（唯一编排）→ `oracle`/`mysql`(IO) / `schema` → `mapping` + `types`
- 类型映射：`mapping.MySQLType` ｜ DDL：`schema.GenerateCreate` ｜ 写入/水位：`mysql`（`BulkUpsert`、`godbsync_watermark`）

## 约束
- 保持纯 Go，禁 CGO / Oracle 客户端驱动。
- 幂等写入走 `mysql.BulkUpsert`（`INSERT ... ON DUPLICATE KEY UPDATE`）。
- 精度：`syncer.normalize` 把 `big.Int`/decimal 转字符串，勿破坏。
- 增量依赖单调水位列；首跑全量后记录水位，可断点续传。
- SQL 占位符：Oracle 用 `:1`/`:2`，MySQL 用 `?`，**不可混用**。
- 日志用 `logrus`；错误用 `fmt.Errorf %w` 中文包裹，由 `cmd` 返回退出码。
- 表名/列名反引号包裹；仅来自可信配置，**勿拼不可信输入**。

## 扩展
- 新类型映射→`mapping.MySQLType`；新 DDL→`schema.GenerateCreate`；新写入→`mysql.BulkUpsert`。
- 新同步模式→`syncer` 加方法 + `cmd/root.go` 加 `case`，勿绕过 `Syncer` 直接拼 SQL。
- 新配置项→`config` 加字段 + `Validate()` 校验，并同步 `examples/config.yaml`。

## 局限
- 不处理源端删除（无 CDC）。
- 目标库需提前 `CREATE DATABASE` 建好。
- 一批数据驻留内存；单批 `INSERT` 受 MySQL `max_allowed_packet` 限制，`batch_size` 过大可能失败。
