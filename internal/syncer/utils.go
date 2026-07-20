package syncer

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"strconv"
	"time"
)

// valuesEqual 比较两侧单元格是否表示相同值：先比规范化字符串，
// 再回退到数值比较（处理 NUMBER/DECIMAL 表示差异，如 1.5 vs 1.50）。
func valuesEqual(a, b interface{}) bool {
	if valueKey(a) == valueKey(b) {
		return true
	}
	af, aok := toFloat64(a)
	bf, bok := toFloat64(b)
	return aok && bok && af == bf
}

// valueKey 将单元格值规范为可比的字符串表示。
func valueKey(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(x)
	case time.Time:
		return x.Format(time.RFC3339Nano)
	case *big.Int:
		return x.String()
	case *big.Rat:
		f, _ := x.Float64()
		return strconv.FormatFloat(f, 'g', -1, 64)
	case string:
		return x
	default:
		return fmt.Sprintf("%v", v)
	}
}

// toFloat64 将数值型单元格解析为 float64（用于跨库数值比较）。
func toFloat64(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int8:
		return float64(x), true
	case int16:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint8:
		return float64(x), true
	case uint16:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float32:
		return float64(x), true
	case float64:
		return x, true
	case *big.Int:
		f, _ := new(big.Rat).SetFrac(x, big.NewInt(1)).Float64()
		return f, true
	case *big.Rat:
		f, _ := x.Float64()
		return f, true
	case []byte:
		f, err := strconv.ParseFloat(string(x), 64)
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

// rowChecksum 计算单行（主键 + 各列）的 SHA256 指纹。
func rowChecksum(key, vals []interface{}) [32]byte {
	h := sha256.New()
	for _, k := range key {
		h.Write([]byte(valueKey(k)))
		h.Write([]byte{0})
	}
	h.Write([]byte{1})
	for _, v := range vals {
		h.Write([]byte(valueKey(v)))
		h.Write([]byte{0})
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// xorSum 合并两个 SHA256 指纹（逐字节异或），用于聚合整批采样校验和。
func xorSum(a, b [32]byte) [32]byte {
	var r [32]byte
	for i := range a {
		r[i] = a[i] ^ b[i]
	}
	return r
}
