// Package mysql 实现 dialect 的 Reader 与 Writer，使 MySQL 可同时作为同步源端与目标端。
//
// 驱动 github.com/go-sql-driver/mysql（纯 Go 无 CGO）。目标端幂等写入使用
// INSERT ... ON DUPLICATE KEY UPDATE；DDL 生成会按目标 MySQL 版本自动降级
// （如 5.5/5.6 用 utf8、DATETIME 而非 DATETIME(6)）。在 init() 中注册 "mysql"。
package mysql
