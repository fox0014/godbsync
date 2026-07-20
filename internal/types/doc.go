// Package types 定义跨方言共享的列元数据模型（Column）与同步过程中用到的基础类型。
//
// 这些类型在「源端 Reader 读出」与「目标端 Writer 写入」之间作为中性契约传递，
// 具体类型映射由目标方言的 GenerateCreate / 类型函数负责。
package types
