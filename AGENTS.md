# godbsync — 开发规则

纯 Go 的多源数据库同步 CLI。零 Oracle 客户端（go-ora/v2）；MySQL 用 go-sql-driver；SQL Server 用 microsoft/go-mssqldb。
通过 `source.dialect` / `target.dialect` 配置方向，**已支持 Oracle ⇄ MySQL、SQL Server ⇄ MySQL、Oracle → SQL Server 双向/单向，以及 PostgreSQL ⇄ MySQL、PostgreSQL ⇄ Oracle、PostgreSQL ⇄ SQL Server 双向同步**
（新增数据库类型只需实现 `dialect.Reader` / `dialect.Writer` 并在 `init()` 注册，无需改编排层）。

## 命令
```bash
go build -ldflags="-s -w" -o godbsync.exe .
go test ./... / go vet ./... / gofmt -l .
```
子命令：`init` / `full` / `incr` / `daemon` / `validate` / `monitor` / `checkpoint` / `version`。

- `monitor`：查看同步状态，两种形态（底层共用 `computeStatusRow`）：
  - `monitor -cli`：终端报告（等同原 `status` 子命令）。只读展示各表同步状态：读取进度后端（断点 anchor / 增量水位）作为「最后同步位置」；
    非 `--quick` 时额外实时统计源/目标行数做一致性校验（需源+目标双连接），状态判定
    `OK`（一致）/`DIVERGED`（不一致）/`SYNCED`（有断点/水位但未实时比对）/`PENDING`（从未同步）。
    支持 `--watch N`（每 N 间隔刷新）与 `--quick`（仅读进度后端、不连库）。
  - `monitor`（或 `monitor -web`）：启动内嵌 HTTP 服务，浏览器可视化监控同步状态（默认 `:8080`）。
    纯 Go `net/http` + 内嵌 HTML（无 CDN，适配内网/离线）；与 `daemon` 解耦（monitor 只读进度后端，daemon 写）。
    新增 `cmd/monitor.go`（`runMonitor` + `/api/status` JSON + `collectOverview`，复用 `computeStatusRow`）；
    内嵌面板 `cmd/web/monitor.html`（浅色主题、自动轮询、状态徽章/进度条、刷新频率切换、连接失败提示）。
    标志：`-addr`（默认 `:8080`）、`-refresh`（前端刷新间隔，默认 `2s`）、`--quick`、`-table`；`Ctrl+C` 优雅关闭。
    > 注：原独立 `status` 子命令已合并进 `monitor -cli`，命令移除。

## 架构（依赖倒置，方向无关）
`cmd`（组合根，按方言名实例化） → `syncer`（唯一编排，方向无关） →
`dialect.Reader`（源） / `dialect.Writer`（目标） + `state`（进度） + `types`
- `oracle` 包：实现 `Reader`（作为源）**与** `Writer`（作为目标），`init()` 注册 `"oracle"`。
- `mysql`  包：实现 `Writer`（作为目标）**与** `Reader`（作为源），`init()` 注册 `"mysql"`。
- `sqlserver` 包：实现 `Reader`（作为源）**与** `Writer`（作为目标），`init()` 注册 `"sqlserver"`。
- `postgresql` 包：实现 `Reader`（作为源）**与** `Writer`（作为目标），`init()` 注册 `"postgresql"`。
- `syncer` **不直接 import** `oracle`/`mysql`/`sqlserver`/`postgresql`，只依赖 `dialect` 接口，故换方向/换库零改动。

> 已支持方向：Oracle ⇄ MySQL、SQL Server ⇄ MySQL、Oracle → SQL Server、PostgreSQL ⇄ MySQL、PostgreSQL ⇄ Oracle、PostgreSQL ⇄ SQL Server。
> PostgreSQL 作目标端用 `INSERT ... ON CONFLICT (pk) DO UPDATE` 幂等（无主键退化为普通 INSERT）；`postgresqlType` 认 Oracle/MySQL/SQL Server 源类型名。

### dialect 接口职责
`Reader`（源）：`Open` / `TableColumns` / `PrimaryKey` / `RowCount` / `ScanRow` /
`Placeholder`（oracle `:n`、mysql `?`、sqlserver `@pn`、postgresql `$n`）/ `QuoteIdentifier`（oracle `"x"`、mysql `` `x` ``、sqlserver `[x]`、postgresql `"x"`）/
`SupportsRowID`（仅 oracle=true，mysql/sqlserver=false）/ `Normalize` / `IsTimeType` / `IsNumericType` / `ListTables`（枚举源库基表，供通配符 * 整库展开）。
`Writer`（目标）：`Open` / `BulkUpsert` / `Truncate` / `TableExists` / `ExecDDL` /
`GenerateCreate` / `RowCount` / `PrimaryKey` / `QuoteIdentifier` / `Placeholder`。

可选扩展接口（目前仅 `mysql` 实现，按 Go 接口断言启用，syncer 对未实现者零感知）：
`TableMetaReader`（`TableMeta(db, tableRef)` 读源表 ENGINE/字符集/排序规则/注释）、
`TableMetaWriter`（`GenerateCreateWithMeta(...)` 在表尾还原表级选项）。

### 关键实现差异（已收进接口，syncer 不感知）
- **类型映射与 DDL**：`oracle` 包内 `oracleType` 生成 Oracle DDL，`mysql` 包内 `mysqlType`
  生成 MySQL DDL，`sqlserver` 包内 `sqlserverType` 生成 SQL Server DDL，`postgresql` 包内 `pgType`
  生成 PostgreSQL DDL；均由 `GenerateCreate` 调用，故类型知识住在其使用方。
  `mysqlType` 处理 Oracle、SQL Server 与 PostgreSQL 的源类型；`oracleType` 处理 MySQL、SQL Server 与 PostgreSQL 的源类型；`sqlserverType` 处理 Oracle、MySQL 与 PostgreSQL 的源类型；`pgType` 处理 Oracle、MySQL、SQL Server 与 PostgreSQL 的源类型。
  版本兼容：建表时 `Writer.GenerateCreate(db, ...)` 探测目标服务端版本并自动降级 DDL——
  MySQL 目标 5.5.x 用 `utf8`+`DATETIME`、5.6.x 加 `DATETIME(6)`、5.7+ 才用 `JSON`（更早 LONGTEXT）；
  Oracle 目标 9i 时 `FLOAT`/`DOUBLE` 源列降级为 `NUMBER`（10g+ 仍 `BINARY_FLOAT`/`BINARY_DOUBLE`）。
  探测失败回退现代版本，已测 5.7+/8.0、12c 路径不变。
  **MySQL→MySQL 保真**：`mysql.readerTableColumns` 用 `INFORMATION_SCHEMA.COLUMNS` 补充
  `types.Column.RawType`（`COLUMN_TYPE` 原文，如 `varchar(32)`/`int unsigned`/`enum('a','b')`）、
  `CharSet`/`Collation`、`Default`、`Comment`；`mysqlTypeForVersion` 见 `RawType` 即原样沿用，
  `GenerateCreate` 逐列输出字符集/排序规则、`NOT NULL`、`DEFAULT`、`COMMENT`。
  根因：go-sql-driver 未实现 `RowsColumnTypeLength`，`ColumnType.Length()` 恒返回 `ok=false`，
  旧路径下 `varchar(n)`/`char(n)` 的 `Length` 为 0 被降级成 `TEXT`/`CHAR(1)`，TEXT 列作主键报
  Error 1170；故不能仅按 `DBType` 重映射 MySQL 源列。`Length`/`Precision`/`Scale` 仍沿用旧路径取值
  （`Length` 保持 0），避免改变 MySQL→Oracle/SQL Server/PostgreSQL 的既有 DDL。
  版本降级：目标 <8.0 时 `*_0900_*` 排序规则降为 `utf8mb4_general_ci`/`utf8mb4_bin`；
  <5.7 的 `json` → `LONGTEXT`；<5.6.4 去掉时间类型微秒参数。
  **MySQL→MySQL 列属性与自增计数器**：`enrichColumnsFromInfoSchema` 从 `EXTRA` 解析
  `AutoIncrement` / `OnUpdateCurrentTimestamp` / `GenerationType`（`STORED`/`VIRTUAL GENERATED`），
  并从 `GENERATION_EXPRESSION` 取生成列表达式，同一查询 `LEFT JOIN INFORMATION_SCHEMA.TABLES`
  顺带取 `t.AUTO_INCREMENT` 存入 `AutoIncrementStart`（0=未知/NULL）。`mysqlColumnDef` 按
  「类型 → charset/collate → 生成列(`GENERATED ALWAYS AS (expr) VIRTUAL|STORED`，不再输出
  DEFAULT/AI/ON UPDATE) / 普通列(`NOT NULL → DEFAULT → AUTO_INCREMENT → ON UPDATE → COMMENT`)」
  输出；`AUTO_INCREMENT` 强制要求该列为主键首列，否则返回错误（MySQL 语义：自增列必须为索引首列）。
  `ON UPDATE CURRENT_TIMESTAMP` 仅对 `datetime`/`timestamp` 输出，精度取其类型精度
  （`datetime(6)` → `ON UPDATE CURRENT_TIMESTAMP(6)`）。
  `GenerateCreate` 末尾在存在自增列且 `AutoIncrementStart > 0` 时追加 ` AUTO_INCREMENT=<源值>`
  （表级计数器随源迁移，避免源库删过尾部后目标 id 段与源重叠；源值为 NULL 不输出，目标从
  `max(id)+1` 自然起始；目标已有数据的 `max(id)` 更大时 MySQL 会忽略该值）。
  表级属性**部分搬迁**：`mysql.Reader.TableMeta`（`dialect.TableMetaReader` 可选接口）读
  `INFORMATION_SCHEMA.TABLES` 的 `ENGINE`/`TABLE_COLLATION`/`TABLE_COMMENT`，字符集由
  `COLLATION_CHARACTER_SET_APPLICABILITY` 反查（不存在的兼容服务按排序规则名前缀兜底）；
  `Writer.GenerateCreateWithMeta`（`dialect.TableMetaWriter` 可选接口）在表尾输出
  `ENGINE=... DEFAULT CHARSET=... COLLATE=... COMMENT='...'`。`syncer.generateCreate` 仅在目标
  实现 `TableMetaWriter` 时取源表级属性，源方言不支持或读取失败则传零值回退旧输出。
  `mysqlTableOptions` 的降级规则：ENGINE 非法标识符→InnoDB；charset 非法→目标默认，
  并按版本降级（<5.5.3 utf8mb4→utf8、<8.0.29 utf8mb3→utf8）；collation 非法或与最终 charset
  前缀不匹配→省略 COLLATE；表注释为空→不输出 COMMENT，注释经 `mysqlQuotedString` 转义单引号/反斜杠。
  **明确不搬**：二级索引（KEY/UNIQUE/FULLTEXT）、外键。`ENGINE` 原样沿用，目标不支持该引擎时建表会报错。
- **幂等写入**：`mysql.BulkUpsert` = `INSERT ... ON DUPLICATE KEY UPDATE`（按占位符上限自动分块）；
  `oracle.BulkUpsert` = `MERGE`（按目标表主键缓存，命中更新非主键列、未命中插入；
  目标表无主键时退化为普通 `INSERT`）；
  `sqlserver.BulkUpsert` = `MERGE`（按目标表主键，`USING (VALUES ...) AS s` 子查询；
  无主键退化为普通 `INSERT`；分块受「2100 参数上限」与「单条语句 1000 行」双重约束；
  主键按（连接,表）缓存于 `sync.Map`，避免每个批次重复查询 `INFORMATION_SCHEMA`）。
- **Normalize**：Oracle 源把 `big.Int`/自定义 decimal 转字符串保精度；MySQL 源把 `[]byte`
  转字符串以便绑给 Oracle（否则被当 RAW）；SQL Server 源把 `[]byte` 转字符串，
  但 `UNIQUEIDENTIFIER`(16 字节) 须转规范 GUID 字符串（如 `xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`）
  写入 `VARCHAR(36)` 才不会损坏——该判定依赖 `Normalize(v, dbType)` 传入的列类型。
  注意 `dialect.Reader.Normalize` 签名为 `Normalize(v interface{}, dbType string) interface{}`，
  syncer 在调用处传入 `cols[i].DBType`。
- **全量锚点**：单列主键 → 按该列有序断点续传；Oracle 无主键 → `ROWID`；
  MySQL/SQL Server 源无单列主键/多列主键 → 退化为无序全量（不续传、不校验边界）。

### 进度存储（水位/断点）
`state` 包抽象为 `Backend` 接口，两种实现：
- `sqlite.go`：本地 SQLite 文件（`modernc.org/sqlite`，纯 Go 无 CGO），**默认内置**。
- `mysql.go`：进度存于「独立 MySQL」——`state.mysql` 段配置连接，与目标库解耦，
  **任意目标方言（含 Oracle）均可用**；未配置 `state.mysql` 且目标为 MySQL 时，
  回退复用目标连接（向后兼容旧用法）。
- 有序窗口 + 周期检查点（`internal/syncer/wmwindow.go`）同时服务于增量水位和全量断点：
  `transferFullConcurrent`（truncate 全量）与 `transferWithWatermark`（增量）均使用有序窗口，
  并发写入下仅推进「序号连续已提交批次」的最高锚点/水位，按 `watermark_checkpoint_interval`
  （默认 10s）周期持久化，中断只补剩余；失败批次（含 `ignore_errors`）冻结窗口，避免越界丢数据。
- truncate=true 全量同步支持断点续传：运行中记录断点，中断后重跑检测到断点时跳过 truncate、从断点
  续传（fresh=true 强制清空重新全量）。

## 约束
- 保持纯 Go，禁 CGO / Oracle 客户端驱动。SQLite 后端用 `modernc.org/sqlite`（纯 Go）；SQL Server 用 `microsoft/go-mssqldb`（纯 Go）满足此约束。
- SQLite 后端**默认内置**（无需 build tag）：配置文件 `state.backend: sqlite`（默认）在任何构建的二进制中均可直接用。
- 幂等写入走 `mysql.BulkUpsert` / `oracle.BulkUpsert` / `sqlserver.BulkUpsert`（方言内部实现，syncer 只调 `writer.BulkUpsert`）。
- 精度：`syncer.normalize` 把 `big.Int`/decimal 转字符串，勿破坏。
- 增量依赖单调水位列；首跑全量后记录水位，可断点续传。
- 进度后端切换经配置文件 `state` 段或 CLI `-state` 选项（sqlite/mysql），新增后端须实现 `state.Backend` 接口；
  `state.backend: mysql` 需在 `state.mysql` 段配置独立进度库连接（与目标库解耦，任意目标方言均可）；
  未配置 `state.mysql` 且目标为 MySQL 时回退复用目标连接（向后兼容）。
- SQL 占位符与标识符引用差异由方言 `Placeholder` / `QuoteIdentifier` 屏蔽，syncer 不硬编码。
- 日志用 `logrus`；错误用 `fmt.Errorf %w` 中文包裹，由 `cmd` 返回退出码。
- 表名/列名引用由方言 `QuoteIdentifier` 包裹；仅来自可信配置，**勿拼不可信输入**。
- **标识符白名单校验**：`config.Validate()` 已对 source/target 的 `host`/`database`/`schema`/`service` 与每张表的 `name`/`target`/`schema`/`watermark`/`column_mapping` 做格式白名单（仅 `[A-Za-z0-9_]`、限定名允许单级 `.`、host 允许 `. : - _ / [ ]`），拒绝引号/空格/分号等注入字符。**这层校验使 godbsync 在不信任配置来源（用户上传/多租户）时也能防 SQL/DSN 注入**；空字段跳过。放宽白名单（如支持中文标识符）须改用 `QuoteIdentifier` 内部转义，而非删除校验。

## 整库通配符同步
- `tables[].name` 设为 `"*"` 表示匹配 `source` 指定库（MySQL 用 `database`、Oracle/SQL Server/PostgreSQL 用 `schema`，空时各自默认 `user_tables`/`dbo`/`public`）下的**所有基表**（自动排除视图与系统库表）。
- 展开在 `syncer.Expand()` 中完成：遍历 `cfg.Tables`，遇 `name == "*"` 调用 `reader.ListTables` 枚举真实表名，生成同名、继承通配符条目 `mode`/`truncate`/`batch_size`/`ignore_errors` 的 `TableConfig`，再交给原有逐表流程（`init`/`full`/`incr`/`daemon`/`validate` 复用，编排零改动）。
- 约束（`config.Validate()` 强制）：`target` 须省略或为 `"*"`（整库同名映射）；`mode` 仅允许 `full`（增量需逐表 `watermark`，故 `*` 不支持 `incremental`）；`name == "*"` 跳过标识符白名单校验，展开后的真实表名在 `ListTables` 中已保证合法。
- 已知限制：`monitor` / `status` / `checkpoint` 子命令仅读配置或进度后端、不连源库枚举，配置里写 `"*"` 时它们只显示模板条目；实际已同步的真实表名记录在进度后端，后续可增强为从进度后端读取。
- **整库结构预检（防误清空/数据损坏）**：`full` 命令在写入/清空前调用 `syncer.PreSyncStructureCheck()`（仅当已展开即 `name == "*"` 时生效）。它对每张「已存在于目标库」的表读取源列、调用 `Writer.GenerateAlter` 比对目标表现有列：发现**目标缺列**生成 `ADD COLUMN`、**列类型不一致**生成 `MODIFY`/`ALTER COLUMN`，目标多出而源没有的列不参与（不误删业务列）。任一表结构不一致即打印完整变更 SQL 并**退出（返回非 0），且不执行任何 truncate / 写入，目标库保持原状**（还原语义）；目标表不存在的表跳过（将新建，无需比对）。该预检只读目标库、不执行 DDL。`GenerateAlter` 已加入 `dialect.Writer` 接口，四个方言各自实现（复用各自 `readerTableColumns` 与类型映射函数，类型差异用同一条类型映射函数双向重建后比对，确保同义类型不被误报）。

## 扩展
- 新数据库类型 → 新建包实现 `dialect.Reader` + `dialect.Writer`，在 `init()` 注册，
  并在 `cmd/root.go` 增加 blank import；`syncer` 一行都不用改。
- 新类型映射 → 改对应方言包的 `oracleType` / `mysqlType` / `sqlserverType`。
  （`sqlserverType` 把非布尔 `TINYINT` 映射 `SMALLINT` 防 MySQL `TINYINT UNSIGNED` 溢出；`UNIQUEIDENTIFIER` 保留原类型。）
- 新 DDL 规则 → 改对应方言包的 `GenerateCreate`。
- 新同步模式 → `syncer` 加方法 + `cmd/root.go` 加 `case`，勿绕过 `Syncer` 直接拼 SQL。
- 新配置项 → `config` 加字段 + `Validate()` 校验，并同步 `examples/`。

## 局限
- 不处理源端删除（无 CDC）。
- 目标库需提前建好：MySQL 需 `CREATE DATABASE`；Oracle 需确保目标用户/schema 有建表权限；SQL Server 需确保用户有建表权限。
- 一批数据驻留内存；单批 `INSERT`/MERGE 受目标库长度/包大小限制（MySQL `max_allowed_packet`、Oracle 占位符上限、SQL Server 2100 参数上限），工具已自动分块。SQL Server 的 `MERGE ... USING (VALUES ...)` 还受单条语句最多 1000 行约束，分块已同时遵守。
- SQL Server 连接加密：`DBConfig.Encrypt` 留空默认 `disable`（兼容自签名/内网）；设 `true` 时生成 `encrypt=true;trustservercertificate=true`（适配 Azure SQL / 强制加密实例）。
- SQL Server 版本下限 **2008+**（建表普通 `CREATE TABLE` + 幂等 `MERGE ... USING (VALUES ...)`，均 2008 起支持）。**SQL Server 原生不支持 `CREATE TABLE IF NOT EXISTS` 语法**，故 `GenerateCreate` 只生成普通 `CREATE TABLE`，重复建表防护由 `ensureTable` 先查 `TableExists` 负责（与 Oracle 方言一致）。SQL Server 源端分页走 syncer 通用有序锚点（`WHERE 锚点列 > ? ORDER BY`），不依赖 `OFFSET/FETCH`（2012+），故源端同样 2008+ 可用。
- 无单列主键且非 Oracle（无 ROWID）的源表不支持全量断点续传，退化为无序全量。
- PostgreSQL 目标端版本下限 **9.1+**（建表用 `CREATE TABLE IF NOT EXISTS`，自 9.1 起支持）；幂等用 `INSERT ... ON CONFLICT (pk) DO UPDATE`，需目标表有主键（无主键退化普通 INSERT）。PG 类型体系稳定，建表不做版本探测。
- 目标库为 Oracle 或 SQL Server 时进度后端可用 `sqlite` 或「独立 MySQL」（在 `state.mysql` 段配置任意 MySQL），不再只能用 sqlite。
