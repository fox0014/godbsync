// Package sqlserver 实现 dialect 的 Reader 与 Writer，使 SQL Server 可同时作为同步源端与目标端。
//
// 驱动 github.com/microsoft/go-mssqldb（纯 Go 无 CGO）。目标端幂等写入使用
// MERGE（USING (VALUES ...) 多行源，受 2100 参数上限与单条语句 1000 行双重约束）；
// 原生不支持 CREATE TABLE IF NOT EXISTS，故建表前由 ensureTable 先查 TableExists。
// 在 init() 中注册 "sqlserver"。
package sqlserver
