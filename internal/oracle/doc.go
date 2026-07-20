// Package oracle 实现 dialect 的 Reader 与 Writer，使 Oracle 可同时作为同步源端与目标端。
//
// 驱动 github.com/sijms/go-ora/v2（纯 Go 零客户端依赖）。目标端幂等写入使用
// MERGE（按目标表主键缓存，无主键时退化为普通 INSERT）；支持 ROWID 作为全量断点锚点。
// 在 init() 中注册 "oracle"。
package oracle
