// Package dialect 定义数据库方言的读写抽象（Reader/Writer 接口）与注册表。
//
// syncer 仅依赖本包接口编排同步，不直接 import 任何具体驱动包。
// 新增一种数据库的同步能力，只需在其包内实现 Reader（作源端）和/或 Writer（作目标端），
// 并在 init() 中调用 RegisterReader/RegisterWriter 注册；syncer 通过 NewReader/NewWriter
// 按配置里的方言名取实例，换方向、换库零改编排层。
package dialect
