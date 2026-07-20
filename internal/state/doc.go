// Package state 抽象同步进度（增量水位 / 全量断点检查点）的持久化后端。
//
// Backend 接口统一两类实现：sqlite（默认，纯 Go 无 CGO，存放在本地文件）与
// mysql（进度存于「独立 MySQL」，与目标库解耦，任意目标方言均可用）。syncer 通过
// 接口读写进度，不关心底层存储。
package state
