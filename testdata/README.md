# godbsync 同步矩阵测试环境

一套「起两个库 → 造数据 → 建表 → 全量 → 校验 → 增量 → 再校验」的回归环境，
GitHub Actions 与本地共用同一份种子数据和同一套操作脚本。

```
testdata/
├── seed/                 四个方言的同构种子数据（源端表结构 + 数据）
│   ├── mysql.sql         MySQL 5.5+ 兼容
│   ├── postgresql.sql
│   ├── sqlserver.sql
│   ├── oracle.sql
│   └── mysql-modern.sql  仅 8.0+：JSON / 0900 排序规则，用于测 DDL 降级
├── ci/dbctl.sh           等待就绪 / 建库 / 执行 SQL（方言差异全收敛在这里）
└── local/
    ├── docker-compose.yml 本地 5 个库容器
    └── run.sh             本地一键跑某个方向
```

## 探针表：每张表针对一类缺陷

| 表 | 行数 | 针对什么 |
| --- | --- | --- |
| `T_ANCHOR` | 1200 | 单列主键 + 单调时间列。1200 行按 `batch_size=400` 切 3 批，覆盖有序窗口、并发提交、断点续传、增量水位 |
| `T_TYPES` | 6 | 类型边界：`DECIMAL(38,10)` 满精度、`BIGINT` 极值、微秒时间、二进制、emoji/换行/单引号、**全 NULL 行** |
| `T_RESERVED` | 3 | 保留字列名 `ORDER`/`GROUP`/`DESC`/`KEY`，验证四个方言的 `QuoteIdentifier` |
| `T_NOPK` | 50 | 无主键表。Oracle 源走 ROWID 断点，其余方言退化为无序全量 — 同一张表测两条代码路径 |
| `T_EMPTY` | 0 | 空表，验证建表成功且空结果集不报错 |

标识符全部大写是硬要求：`oracle.Writer.GenerateCreate` 用双引号原样建表，而
`TableExists` 查 `all_tables` 时会 `ToUpper`；小写表名会让「表已存在」判定失效，
二次同步直接 `ORA-00955`。

## CI 里的 13 个方向

4 个方言两两组合共 6 对、双向 12 个方向，外加 `mysql2mysql` 作为基线。

- **fast 层**（`mysql2mysql`、`pg2mysql`、`sqlserver2mysql`）：push / PR 都跑，几分钟出结果
- **full 层**（含 Oracle 的 9 个方向）：仅手动触发与每周 cron 跑（Oracle 镜像 ~2.5GB、就绪 ~90s）
- **实验性**（`oracle2sqlserver`、`sqlserver2oracle`）：`AGENTS.md` 未明确声明支持，
  `continue-on-error` 处理，失败不红但结果可见

另有独立 job `mysql-versions`：源固定 `mysql:8.0`，目标跑 `8.0 / 5.7 / 5.6 / 5.5`，
专门验证 `GenerateCreate` 的版本探测与降级（JSON→LONGTEXT、`0900`→`general_ci`、
`DATETIME(6)`→`DATETIME`）。5.6/5.5 官方镜像已停维护，标记为非严格。

断言不用 grep 日志，直接看 `godbsync validate` 的退出码 —— 它内部做
「行数比对 + 主键抽样逐列比对 + 另一区间校验和」，比文本匹配可靠。

## 本地跑

```bash
# 起容器（只起需要的两个，避免白等 Oracle）
docker compose -f testdata/local/docker-compose.yml up -d mysql1 mysql2

# 跑一个方向：等就绪 → 灌数据 → 建表 → 全量 → 校验
bash testdata/local/run.sh mysql2mysql

# 换方向
bash testdata/local/run.sh oracle2mysql

# 只跑某一阶段（表已建好、想单独调 validate 时）
STAGE=validate bash testdata/local/run.sh oracle2mysql

# 清理
bash testdata/local/run.sh down
```

本地端口：`mysql1 33061` / `mysql2 33062` / `pg 5433` / `mssql 14330` / `oracle 15210`。
配置与状态文件落在 `testdata/local/.work/`（已 gitignore）。

## 首次运行可能红的地方（有意为之）

这些不是 CI 配置问题，而是**探针表专门要撞的边界**，红了说明发现了真实缺陷：

1. **写 Oracle 的方向**（`*2oracle`）：`T_TYPES.C_TXT` 是 7500 字符的 CLOB。
   超过 4000 字节的字符串绑到 CLOB 列时，若 `oracle.BulkUpsert` 的数组绑定按
   VARCHAR2 处理，会报 `ORA-01461`。这是 CLOB 大字段写入的真实上限问题。
2. **`T_TYPES` 第 3 行**：`C_VC`/`C_TXT` 是空串。Oracle 语义下 `''` 即 `NULL`，
   与另外三个方言不同。`valuesEqual` 认定 `nil` 与 `''` 相等，故校验能过，
   但目标端的实际存储值有差异。
3. **`T_MODERN` 的 `JSON` 列**：源 8.0 读出的 JSON 是规范化后的文本，
   5.5 目标降级为 LONGTEXT 存原文，两边格式可能不同。

## 改种子数据的纪律

四份 `seed/*.sql` 必须保持**同名、同列序、同语义类型**。任何一处改动都要同步另外三份，
否则某个方向测到的就不是同一个东西了。`T_TYPES` 的行数、`T_ANCHOR` 的行数被
`id` 顺序依赖（`T_NOPK` 从 `T_ANCHOR` 取前 50 行），改行数请一并检查。

## 口令与端口

CI 里由 `env.DB_PASS` 单一来源控制，但 `services` 段无法引用它，故容器 env 里有
字面量副本 —— **改口令要改 11 处**（1 个 `env.DB_PASS` + 2 个 service 各 5 个
`*PASSWORD`），否则容器口令与 godbsync 连接用的不一致。

口令需满足：SQL Server 复杂度（大小写+数字，≥8 位）、不含 `# ! $ @`。
