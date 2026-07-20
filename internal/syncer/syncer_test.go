package syncer

import (
	"crypto/sha256"
	"math/big"
	"reflect"
	"testing"
	"time"

	"godbsync/config"
	"godbsync/internal/oracle"
	"godbsync/internal/types"
)

// newTestSyncer 构造一个仅用于单元测试的 Syncer，使用 Oracle 方言的 Reader（无需真实连接）。
func newTestSyncer() *Syncer {
	return &Syncer{reader: &oracle.Reader{}}
}

// testReader 是复用的 Oracle 方言 Reader 实例，避免 oracle.Reader{}.Method() 在 if 赋值中触发解析歧义。
var testReader = &oracle.Reader{}

// TestNormalize 验证扫描值的归一化逻辑：基础类型原样保留，
// 超大数值（如 big.Int）转为字符串以保精度。
func TestNormalize(t *testing.T) {
	if got := testReader.Normalize(nil, ""); got != nil {
		t.Errorf("normalize(nil) = %v, want nil", got)
	}
	if got := testReader.Normalize(int64(42), ""); got != int64(42) {
		t.Errorf("normalize(int64) = %v (%T)", got, got)
	}
	if got := testReader.Normalize(float64(1.5), ""); got != float64(1.5) {
		t.Errorf("normalize(float64) = %v (%T)", got, got)
	}
	if got := testReader.Normalize("x", ""); got != "x" {
		t.Errorf("normalize(string) = %v (%T)", got, got)
	}
	bigVal := big.NewInt(1234567890123456789)
	got := testReader.Normalize(bigVal, "")
	want := "1234567890123456789"
	if got != want {
		t.Errorf("normalize(big.Int) = %v (%T), want %v", got, got, want)
	}
	if _, ok := testReader.Normalize(time.Now(), "").(time.Time); !ok {
		t.Errorf("normalize(time.Time) 未保留 time.Time 类型")
	}
}

// TestNormalize_AllBasicTypes 覆盖类型断言中所有显式列出的基础类型。
func TestNormalize_AllBasicTypes(t *testing.T) {
	tests := []struct {
		name  string
		input interface{}
	}{
		{"int", int(1)},
		{"int8", int8(1)},
		{"int16", int16(1)},
		{"int32", int32(1)},
		{"int64", int64(1)},
		{"uint", uint(1)},
		{"uint8", uint8(1)},
		{"uint16", uint16(1)},
		{"uint32", uint32(1)},
		{"uint64", uint64(1)},
		{"float32", float32(1.5)},
		{"float64", float64(1.5)},
		{"string", "hello"},
		{"[]byte", []byte("hello")},
		{"bool", true},
		{"time.Time", time.Now()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := testReader.Normalize(tt.input, "")
			if !reflect.DeepEqual(got, tt.input) {
				t.Errorf("normalize(%v) = %v (%T), want equal", tt.input, got, got)
			}
		})
	}
}

// TestNormalize_FallbackToString 验证未知类型降级为字符串表示。
func TestNormalize_FallbackToString(t *testing.T) {
	type customType struct{}
	got := testReader.Normalize(customType{}, "")
	s, ok := got.(string)
	if !ok {
		t.Fatalf("未知类型应转为 string, got %T", got)
	}
	if s == "" {
		t.Error("字符串表示不应为空")
	}
}

// TestWatermarkRoundTrip 验证水位值的序列化与反序列化（时间/数值）一致。
func TestWatermarkRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 30, 0, 123, time.UTC)
	s := formatWatermark(now)
	syn := newTestSyncer()
	parsed, err := syn.parseWatermark(s, types.Column{DBType: "TIMESTAMP"})
	if err != nil {
		t.Fatalf("parseWatermark(time) error: %v", err)
	}
	pt, ok := parsed.(time.Time)
	if !ok {
		t.Fatalf("parseWatermark(time) = %T, want time.Time", parsed)
	}
	if !pt.Equal(now) {
		t.Errorf("时间水位往返不一致: %v != %v", pt, now)
	}

	s = formatWatermark(int64(987654321))
	parsed, err = syn.parseWatermark(s, types.Column{DBType: "NUMBER"})
	if err != nil {
		t.Fatalf("parseWatermark(number) error: %v", err)
	}
	if parsed != int64(987654321) {
		t.Errorf("数值水位往返不一致: %v (%T)", parsed, parsed)
	}
}

// TestParseWatermark_RFC3339Fallback 时间类解析支持 RFC3339（无纳秒后缀）。
func TestParseWatermark_RFC3339Fallback(t *testing.T) {
	s := "2026-07-09T12:30:00Z"
	syn := newTestSyncer()
	parsed, err := syn.parseWatermark(s, types.Column{DBType: "TIMESTAMP"})
	if err != nil {
		t.Fatalf("RFC3339 解析失败: %v", err)
	}
	pt, ok := parsed.(time.Time)
	if !ok {
		t.Fatalf("应为 time.Time, got %T", parsed)
	}
	if pt.Year() != 2026 || pt.Month() != 7 || pt.Day() != 9 {
		t.Errorf("解析时间不正确: %v", pt)
	}
}

// TestParseWatermark_InvalidTime 非法时间字符串应返回错误。
func TestParseWatermark_InvalidTime(t *testing.T) {
	syn := newTestSyncer()
	_, err := syn.parseWatermark("not-a-time", types.Column{DBType: "TIMESTAMP"})
	if err == nil {
		t.Error("非法时间字符串应返回错误")
	}
}

// TestParseWatermark_FloatFallback 整数解析失败后回落为 float64。
func TestParseWatermark_FloatFallback(t *testing.T) {
	syn := newTestSyncer()
	parsed, err := syn.parseWatermark("3.14", types.Column{DBType: "NUMBER"})
	if err != nil {
		t.Fatalf("浮点数解析失败: %v", err)
	}
	f, ok := parsed.(float64)
	if !ok {
		t.Fatalf("应为 float64, got %T", parsed)
	}
	if f != 3.14 {
		t.Errorf("want 3.14, got %v", f)
	}
}

// TestParseWatermark_StringFallback 非时间/数值列原样返回字符串。
func TestParseWatermark_StringFallback(t *testing.T) {
	syn := newTestSyncer()
	parsed, err := syn.parseWatermark("hello", types.Column{DBType: "VARCHAR2"})
	if err != nil {
		t.Fatalf("字符串解析失败: %v", err)
	}
	if parsed != "hello" {
		t.Errorf("want hello, got %v", parsed)
	}
}

// TestFormatWatermark_String 普通值格式化为字符串。
func TestFormatWatermark_String(t *testing.T) {
	if got := formatWatermark("hello"); got != "hello" {
		t.Errorf("formatWatermark(string) = %q, want hello", got)
	}
	if got := formatWatermark(int64(42)); got != "42" {
		t.Errorf("formatWatermark(int64) = %q, want 42", got)
	}
}

// TestFindWatermarkColumn_Found 按名称找到水位列。
func TestFindWatermarkColumn_Found(t *testing.T) {
	cols := []types.Column{
		{Name: "ID"},
		{Name: "UPDATED_AT", DBType: "TIMESTAMP"},
		{Name: "NAME"},
	}
	col, idx, err := findWatermarkColumn(cols, "updated_at")
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Errorf("水位列索引应为 1, got %d", idx)
	}
	if col.Name != "UPDATED_AT" {
		t.Errorf("水位列名应为 UPDATED_AT, got %s", col.Name)
	}
}

// TestFindWatermarkColumn_NotFound 未找到时返回错误。
func TestFindWatermarkColumn_NotFound(t *testing.T) {
	cols := []types.Column{{Name: "A"}, {Name: "B"}}
	_, _, err := findWatermarkColumn(cols, "C")
	if err == nil {
		t.Error("找不到水位列应返回错误")
	}
}

// TestIsTimeType 验证各时间类型的识别。
func TestIsTimeType(t *testing.T) {
	for _, typ := range []string{"DATE", "TIMESTAMP", "TIMESTAMP(6)", "TIMESTAMP_WITH_TZ", "TIMESTAMP_WITH_LOCAL_TZ", "DATETIME"} {
		if !testReader.IsTimeType(typ) {
			t.Errorf("%s 应被识别为时间类型", typ)
		}
	}
	for _, typ := range []string{"VARCHAR2", "NUMBER", "CLOB"} {
		if testReader.IsTimeType(typ) {
			t.Errorf("%s 不应被识别为时间类型", typ)
		}
	}
}

// TestIsNumericType 验证各数值类型的识别。
func TestIsNumericType(t *testing.T) {
	for _, typ := range []string{"NUMBER", "NUMERIC", "FLOAT", "BINARY_FLOAT", "BINARY_DOUBLE", "DOUBLE", "DOUBLE PRECISION", "REAL"} {
		if !testReader.IsNumericType(typ) {
			t.Errorf("%s 应被识别为数值类型", typ)
		}
	}
	for _, typ := range []string{"VARCHAR2", "DATE", "CLOB"} {
		if testReader.IsNumericType(typ) {
			t.Errorf("%s 不应被识别为数值类型", typ)
		}
	}
}

// TestFilterTables 验证表过滤逻辑。
func TestFilterTables(t *testing.T) {
	tables := []config.TableConfig{
		{Name: "USERS"},
		{Name: "ORDERS"},
		{Name: "PRODUCTS", Target: "MY_PRODUCTS"},
	}
	s := &Syncer{cfg: &config.Config{Tables: tables}}

	// 空 filter 返回全部
	got := s.filterTables(nil)
	if len(got) != 3 {
		t.Errorf("空 filter 应返回全部 3 张表, got %d", len(got))
	}

	// 按源表名过滤（大小写不敏感）
	got = s.filterTables([]string{"users", "orders"})
	if len(got) != 2 {
		t.Errorf("filter 2 张表应返回 2, got %d", len(got))
	}

	// 按目标表名过滤
	got = s.filterTables([]string{"my_products"})
	if len(got) != 1 || got[0].Name != "PRODUCTS" {
		t.Errorf("按目标表名过滤应找到 PRODUCTS, got %v", got)
	}
}

// ---- valuesEqual ----

func TestValuesEqual_IdenticalStrings(t *testing.T) {
	if !valuesEqual("hello", "hello") {
		t.Error("相同字符串应相等")
	}
}

func TestValuesEqual_DifferentStrings(t *testing.T) {
	if valuesEqual("hello", "world") {
		t.Error("不同字符串应不等")
	}
}

func TestValuesEqual_BothNil(t *testing.T) {
	if !valuesEqual(nil, nil) {
		t.Error("nil == nil")
	}
}

func TestValuesEqual_NilVsEmptyString(t *testing.T) {
	// valueKey(nil)=="" 且 valueKey("")==""，故 nil 与空串判定相等
	if !valuesEqual(nil, "") {
		t.Error("nil 与空串应判定相等（valueKey 均返回 \"\"）")
	}
}

func TestValuesEqual_NumericViaFloat64(t *testing.T) {
	// NUMBER(10,2) 列可能返回不同底层类型，但数值相同
	if !valuesEqual(int64(100), "100") {
		t.Error("数值 100 与 \"100\" 应通过 float64 比较判定相等")
	}
	if !valuesEqual(float64(1.5), "1.5") {
		t.Error("1.5 与 \"1.5\" 应相等")
	}
}

func TestValuesEqual_DifferentNumeric(t *testing.T) {
	if valuesEqual(int64(42), int64(24)) {
		t.Error("42 != 24")
	}
}

func TestValuesEqual_TimeMatch(t *testing.T) {
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	if !valuesEqual(now, now) {
		t.Error("同一 time.Time 应相等")
	}
}

func TestValuesEqual_TimeMismatch(t *testing.T) {
	a := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	b := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	if valuesEqual(a, b) {
		t.Error("不同时间应不等")
	}
}

func TestValuesEqual_BytesIdentical(t *testing.T) {
	if !valuesEqual([]byte("abc"), []byte("abc")) {
		t.Error("相同 []byte 应相等")
	}
}

func TestValuesEqual_BytesDifferent(t *testing.T) {
	if valuesEqual([]byte("abc"), []byte("xyz")) {
		t.Error("不同 []byte 应不等")
	}
}

func TestValuesEqual_BytesVsString(t *testing.T) {
	if !valuesEqual([]byte("hello"), "hello") {
		t.Error("[]byte(\"hello\") 与 \"hello\" 应通过 valueKey 比较判定相等")
	}
}

func TestValuesEqual_DecimalFormatRoundTrip(t *testing.T) {
	// 源端 NUMBER 可能返回 "1.50" 而目标端返回 float64(1.5)，应视为相等
	if !valuesEqual("1.50", float64(1.5)) {
		t.Error("\"1.50\" 与 1.5 应通过 float64 比较判定相等")
	}
}

// ---- valueKey ----

func TestValueKey_Nil(t *testing.T) {
	if got := valueKey(nil); got != "" {
		t.Errorf("nil → \"\", got %q", got)
	}
}

func TestValueKey_Bytes(t *testing.T) {
	if got := valueKey([]byte("abc")); got != "abc" {
		t.Errorf("[]byte → string, got %q", got)
	}
}

func TestValueKey_Time(t *testing.T) {
	tm := time.Date(2026, 7, 12, 10, 30, 0, 123456789, time.UTC)
	got := valueKey(tm)
	want := tm.Format(time.RFC3339Nano)
	if got != want {
		t.Errorf("time.Time → RFC3339Nano, got %q", got)
	}
}

func TestValueKey_BigInt(t *testing.T) {
	bi := big.NewInt(1234567890123456789)
	got := valueKey(bi)
	if got != "1234567890123456789" {
		t.Errorf("big.Int → decimal str, got %q", got)
	}
}

func TestValueKey_BigRat(t *testing.T) {
	br := new(big.Rat).SetFloat64(3.14)
	got := valueKey(br)
	if got != "3.14" {
		t.Errorf("big.Rat → float64 str, got %q", got)
	}
}

func TestValueKey_String(t *testing.T) {
	if got := valueKey("hello"); got != "hello" {
		t.Errorf("string 原样, got %q", got)
	}
}

func TestValueKey_Int(t *testing.T) {
	if got := valueKey(int(42)); got != "42" {
		t.Errorf("int → \"42\", got %q", got)
	}
}

func TestValueKey_Float64(t *testing.T) {
	if got := valueKey(float64(1.5)); got != "1.5" {
		t.Errorf("float64 → \"1.5\", got %q", got)
	}
}

func TestValueKey_Bool(t *testing.T) {
	got := valueKey(true)
	if got != "true" {
		t.Errorf("bool → \"true\", got %q", got)
	}
}

// ---- toFloat64 ----

func TestToFloat64_Nil(t *testing.T) {
	_, ok := toFloat64(nil)
	if ok {
		t.Error("nil 应返回 false")
	}
}

func TestToFloat64_IntTypes(t *testing.T) {
	tests := []struct {
		name string
		v    interface{}
		want float64
	}{
		{"int", int(42), 42},
		{"int8", int8(8), 8},
		{"int16", int16(16), 16},
		{"int32", int32(32), 32},
		{"int64", int64(64), 64},
		{"uint", uint(7), 7},
		{"uint8", uint8(8), 8},
		{"uint16", uint16(16), 16},
		{"uint32", uint32(32), 32},
		{"uint64", uint64(64), 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := toFloat64(tt.v)
			if !ok {
				t.Fatalf("应返回 true")
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestToFloat64_FloatTypes(t *testing.T) {
	tests := []struct {
		name string
		v    interface{}
		want float64
	}{
		{"float32", float32(1.5), 1.5},
		{"float64", float64(2.5), 2.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := toFloat64(tt.v)
			if !ok {
				t.Fatalf("应返回 true")
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestToFloat64_BigInt(t *testing.T) {
	bi := big.NewInt(999888777666555444)
	got, ok := toFloat64(bi)
	if !ok {
		t.Fatal("big.Int 应可转为 float64")
	}
	if got <= 0 {
		t.Errorf("正值应大于 0, got %v", got)
	}
}

func TestToFloat64_BigRat(t *testing.T) {
	br := new(big.Rat).SetFloat64(3.14)
	got, ok := toFloat64(br)
	if !ok {
		t.Fatal("big.Rat 应可转为 float64")
	}
	if got != 3.14 {
		t.Errorf("got %v, want 3.14", got)
	}
}

func TestToFloat64_Bytes(t *testing.T) {
	got, ok := toFloat64([]byte("123.456"))
	if !ok {
		t.Fatal("数字字符串 []byte 应可转为 float64")
	}
	if got != 123.456 {
		t.Errorf("got %v, want 123.456", got)
	}
}

func TestToFloat64_BytesInvalid(t *testing.T) {
	_, ok := toFloat64([]byte("not-a-number"))
	if ok {
		t.Error("非数字 []byte 应返回 false")
	}
}

func TestToFloat64_String(t *testing.T) {
	got, ok := toFloat64("99.99")
	if !ok {
		t.Fatal("数字字符串应可转为 float64")
	}
	if got != 99.99 {
		t.Errorf("got %v, want 99.99", got)
	}
}

func TestToFloat64_StringInvalid(t *testing.T) {
	_, ok := toFloat64("hello")
	if ok {
		t.Error("非数字 string 应返回 false")
	}
}

func TestToFloat64_Bool(t *testing.T) {
	_, ok := toFloat64(true)
	if ok {
		t.Error("bool 类型不应转为 float64")
	}
}

func TestToFloat64_Time(t *testing.T) {
	_, ok := toFloat64(time.Now())
	if ok {
		t.Error("time.Time 类型不应转为 float64")
	}
}

// ---- rowChecksum ----

func TestRowChecksum_Deterministic(t *testing.T) {
	key := []interface{}{int64(1)}
	vals := []interface{}{"hello", int64(42)}
	s1 := rowChecksum(key, vals)
	s2 := rowChecksum(key, vals)
	if s1 != s2 {
		t.Error("相同输入的校验和应一致")
	}
}

func TestRowChecksum_DifferentKey(t *testing.T) {
	vals := []interface{}{"x"}
	s1 := rowChecksum([]interface{}{int64(1)}, vals)
	s2 := rowChecksum([]interface{}{int64(2)}, vals)
	if s1 == s2 {
		t.Error("不同主键的校验和应不同")
	}
}

func TestRowChecksum_DifferentVals(t *testing.T) {
	key := []interface{}{int64(1)}
	s1 := rowChecksum(key, []interface{}{"a"})
	s2 := rowChecksum(key, []interface{}{"b"})
	if s1 == s2 {
		t.Error("不同列值的校验和应不同")
	}
}

func TestRowChecksum_NonEmpty(t *testing.T) {
	s := rowChecksum([]interface{}{"pk"}, []interface{}{"v"})
	var zero [32]byte
	if s == zero {
		t.Error("正常输入的校验和不应为零值")
	}
}

func TestRowChecksum_UsesSHA256Hash(t *testing.T) {
	// rowChecksum 使用分隔符 0x00 和 0x01 区分 key 和 val
	key := []interface{}{"k1"}
	vals := []interface{}{"v1"}
	got := rowChecksum(key, vals)

	h := sha256.New()
	h.Write([]byte("k1")) // valueKey(string)
	h.Write([]byte{0})
	h.Write([]byte{1})
	h.Write([]byte("v1"))
	h.Write([]byte{0})
	var want [32]byte
	copy(want[:], h.Sum(nil))
	if got != want {
		t.Errorf("校验和与手动 SHA256 结果不一致")
	}
}

func TestRowChecksum_NilValues(t *testing.T) {
	// nil 在 valueKey 中转为空串，应能正常计算
	s := rowChecksum([]interface{}{nil}, []interface{}{nil, "x"})
	var zero [32]byte
	if s == zero {
		t.Error("含 nil 的校验和不应为零值")
	}
}

// ---- xorSum ----

func TestXorSum_ZeroPlusZero(t *testing.T) {
	var a, b [32]byte
	r := xorSum(a, b)
	var zero [32]byte
	if r != zero {
		t.Error("0^0 = 0")
	}
}

func TestXorSum_Associative(t *testing.T) {
	// (a ^ b) ^ c == a ^ (b ^ c)
	var a, b, c [32]byte
	a[0], b[1], c[2] = 1, 2, 3
	r1 := xorSum(xorSum(a, b), c)
	r2 := xorSum(a, xorSum(b, c))
	if r1 != r2 {
		t.Error("异或应满足结合律")
	}
}

func TestXorSum_Commutative(t *testing.T) {
	var a, b [32]byte
	a[0], b[1] = 7, 13
	if xorSum(a, b) != xorSum(b, a) {
		t.Error("异或应满足交换律")
	}
}

func TestXorSum_SelfCancelling(t *testing.T) {
	var a [32]byte
	a[0], a[31] = 0xFF, 0xAA
	if xorSum(a, a) != ([32]byte{}) {
		t.Error("a ^ a = 0")
	}
}

func TestXorSum_ProducesOutput(t *testing.T) {
	var a [32]byte
	a[0] = 0x0F
	var b [32]byte
	b[0] = 0xF0
	r := xorSum(a, b)
	if r[0] != 0xFF {
		t.Errorf("got 0x%02X, want 0xFF", r[0])
	}
	for i := 1; i < 32; i++ {
		if r[i] != 0 {
			t.Errorf("byte[%d]=0x%02X want 0", i, r[i])
		}
	}
}
