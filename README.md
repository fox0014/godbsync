# godbsync — 多源数据库同步工具（Go）

纯 Go 实现，零 Oracle/SQL Server 客户端依赖，支持全量/增量同步、守护模式、数据校验、**可视化监控（Web 仪表盘）**。
通过 `source.dialect` / `target.dialect` 配置同步方向，**已支持 Oracle ⇄ MySQL、SQL Server ⇄ MySQL、Oracle → SQL Server 双向/单向同步，以及 PostgreSQL ⇄ MySQL、PostgreSQL ⇄ Oracle、PostgreSQL ⇄ SQL Server 双向同步**（新增数据库类型只需实现 `dialect.Reader` / `dialect.Writer` 并在 `init()` 注册）。
进度默认存本地 SQLite，也可存独立 MySQL（与目标库解耦，任意目标方言均可用）。

## 支持的数据库版本

兼容性取决于目标端生成的 DDL 类型/子句与 Oracle 增量用到的 `MERGE`，**与具体小版本无关**。**已实测：MySQL 8 ⇄ Oracle 12c 双向同步。**

**MySQL（源 / 目标）**
- 推荐 **5.7+**（含 8.0、8.4；MariaDB 10.2+）：`utf8mb4`、`DATETIME(6)` 微秒精度、`JSON` 类型全支持
- **5.5 / 5.6 同样开箱即用**：工具在 `init` 建表时自动探测目标服务端版本并按版本降级 DDL——5.5.x 用 `utf8` + `DATETIME`（无微秒），5.6.x 再加 `DATETIME(6)`，5.7+ 才用 `JSON`（更早版本自动降级为 `LONGTEXT`）。无需任何配置
- < 5.5.3：不支持（无 `utf8mb4` 且时间精度兼容写法仍受限）
- 驱动 `go-sql-driver/mysql` v1.10 兼容 MySQL 4.1+ 线协议，支持 8.0 默认 `caching_sha2_password`

**Oracle（源 / 目标）**
- 推荐 **10g+**（含 11g、12c、18c、19c、21c、23c）
- 连接：12c+ 多租户用 `conn_type: service`（服务名）；11g 及更早用 `conn_type: sid`
- **9i 开箱即用**：`init` 建表时自动探测版本，`FLOAT`/`DOUBLE` 源列在 9i 上自动降级为 `NUMBER`（10g+ 仍用 `BINARY_FLOAT`/`BINARY_DOUBLE`），无需配置
- 8i 及更早：不支持（无 `TIMESTAMP`、无 `MERGE`）
- 驱动 `go-ora` 纯 Go，零 Oracle 客户端依赖

**SQL Server（源 / 目标）**
- 推荐 **2008+**（含 2012、2014、2016、2019、2022）：建表用普通 `CREATE TABLE`，幂等写入用 `MERGE ... USING (VALUES ...)`，二者均自 2008 起支持；SQL Server 原生**不支持** `CREATE TABLE IF NOT EXISTS` 语法（与 MySQL/Oracle 不同），重复建表防护由工具先查表存在性负责
- 实际建议 2012+（2008 已结束生命周期）
- 驱动 `microsoft/go-mssqldb` 纯 Go，零客户端依赖；连接加密默认 `disable`（兼容自签名/内网），连 Azure SQL 或强制加密实例设 `encrypt: true`

**PostgreSQL（源 / 目标）**
- 作为**源端**：支持 PostgreSQL → MySQL / PostgreSQL → Oracle / PostgreSQL → SQL Server。驱动 `github.com/lib/pq` 纯 Go，零客户端依赖
- 作为**目标端**：支持 MySQL → PostgreSQL / Oracle → PostgreSQL / SQL Server → PostgreSQL。幂等写入用 `INSERT ... ON CONFLICT (pk) DO UPDATE`（无主键时退化为普通 INSERT）；建表用 `CREATE TABLE IF NOT EXISTS`（PG 9.1+ 支持）；类型映射 `postgresql` 包内置，认 Oracle/MySQL/SQL Server 的源类型名
- 版本：PostgreSQL 9.1+（目标端 `CREATE TABLE IF NOT EXISTS` 自 9.1 起；源端主键查询、`$n` 占位符通用）。连接 `sslmode` 默认 `disable`（兼容自签名/内网），可用 `encrypt` 字段覆盖为 `require`/`verify-ca`/`verify-full`

> 小版本（如 8.0.11 vs 8.0.35、12.1 vs 19c）无差异；工具不依赖任何小版本专有语法。

## 快速开始

```bash
# 构建（默认内置 MySQL + SQLite 两种进度后端）
go build -ldflags="-s -w" -o godbsync.exe .

# 1) 建表
godbsync init -config config.yaml

# 2) 全量同步（支持断点续传）
godbsync full -config config.yaml                          # 全量
godbsync full -config config.yaml -table EMPLOYEES         # 指定表
godbsync full -config config.yaml -table EMPLOYEES -fresh  # 清除断点重做

# 3) 增量同步（基于水位列推进，首次全量拉取后记录水位）
godbsync incr -config config.yaml

# 4) 守护模式（定时轮询增量）
godbsync daemon -config config.yaml -interval 1m

# 5) 数据校验（行数比对 + 主键抽样内容校验）
godbsync validate -config config.yaml

# 6) 查看同步状态（断点/水位 + 源/目标实时行数一致性）
godbsync monitor -cli -config config.yaml                  # 终端展示各表状态（含实时行数一致性）
godbsync monitor -cli -config config.yaml --watch 2s      # 每 2s 刷新一次
godbsync monitor -cli -config config.yaml --quick         # 仅读断点/水位，不连库、不统计行数
godbsync monitor -config config.yaml                       # 启动 Web 仪表盘（默认 :8080）

# 7) 断点管理
godbsync checkpoint                    # 列出全部断点
godbsync checkpoint -table EMPLOYEES   # 查看单表
godbsync checkpoint -clear -table EMPLOYEES  # 清除单表断点
godbsync checkpoint -clear -all        # 清除全部断点

# 进度存本地（默认即 SQLite，无需 `-state`）
godbsync full -config config.yaml
godbsync checkpoint                         # 默认读本地 godbsync.db

# CLI 覆盖配置文件 state 段（例如临时切到 MySQL 后端）
godbsync full -config config.yaml -state mysql
godbsync full -config config.yaml -state file=./other.db
```

## 可视化监控（monitor）

`monitor` 子命令启动一个**内嵌 HTTP 服务**（纯 Go `net/http` + 内嵌 HTML，无任何外部 CDN/JS 依赖，适配内网/离线环境），浏览器打开即可实时查看各表同步状态。面板展示：源→目标链路、状态汇总卡片（已一致/不一致/待同步/已同步/错误）、各表状态徽章、源/目标行数、差值、进度条与最后同步位置，前端按间隔自动轮询刷新，支持暂停与切换刷新频率。

与 `daemon` 解耦：`monitor` 只读进度后端（+ 非 `--quick` 时的源/目标实时行数），`daemon` 负责写入进度，因此二者可分别运行于不同进程——先起 `daemon` 跑同步，再起 `monitor` 看盘。

```bash
# 启动 Web 监控（默认监听 :8080，浏览器打开 http://127.0.0.1:8080/ ）
godbsync monitor -config config.yaml

# 指定监听地址与前端刷新间隔
godbsync monitor -config config.yaml -addr 127.0.0.1:9000 -refresh 5s

# 仅读断点/水位，不连库统计实时行数（适合没有源/目标库权限只查进度后端的场景）
godbsync monitor -config config.yaml --quick

# 只监控部分表
godbsync monitor -config config.yaml -table EMPLOYEES,DEPT
```

- `-addr`：HTTP 监听地址，默认 `:8080`（如 `127.0.0.1:9000` 仅本机可访问）。
- `-refresh`：前端自动刷新间隔，默认 `2s`（接受 `1s`/`5s`/`30s` 等 `time.Duration`）。
- `--quick`：同 `status --quick`，仅展示断点/水位，不连接源/目标库统计实时行数；不指定则连接双库做一致性校验（状态 `OK`/`DIVERGED`/`ERR` 据此判定）。
- `-table`：逗号分隔的表名过滤（匹配源表名或目标表名）。
- `Ctrl+C` 优雅停止。
- 数据接口：`GET /api/status` 返回 JSON（`source`/`target`/`tables`/`state_counts` 等），便于接入自有监控/告警系统。

## 配置

复制 `examples/config.yaml` 为 `config.yaml`，关键字段：

| 字段 | 说明 |
| --- | --- |
| `source.dialect` / `target.dialect` | 源/目标数据库类型：`oracle`、`mysql`、`sqlserver` 或 `postgresql` |
| `oracle.conn_type` | `service`（默认）或 `sid`（仅 dialect=oracle 时生效） |
| `tables[].mode` | `full`（全量）或 `incremental`（增量） |
| `tables[].watermark` | 增量模式必填，单调列名（如 `LAST_MODIFIED`） |
| `tables[].truncate` | 全量写入前是否清空目标表 |
| `tables[].batch_size` | 每批写入行数 |
| `tables[].ignore_errors` | 单批失败是否跳过继续 |
| `tables[].target` | 目标表名（省略则与源表同名；Oracle 目标可写 `OWNER.TABLE` 指定属主） |
| `tables[].schema` | 源表属主（Oracle/SQL Server 源为 schema，MySQL 源为数据库，PostgreSQL 源默认 `public`） |
| `tables[].column_mapping` | 源列名 → 目标列名的映射 |
| `source.encrypt` / `target.encrypt` | PostgreSQL/SQL Server SSL 模式（如 `disable`、`require`、`verify-ca`，默认 `disable`） |
| `workers` | 并发写入 worker 数（全局，默认 `4`） |
| `auto_validate` | 同步完成后自动校验（默认 `true`） |
| `validate_sample_size` | 内容校验抽样行数（默认 `100`） |
| `validate_sample_random` | 抽样起点是否随机（默认 `false`） |
| `watermark_checkpoint_interval` | 增量水位/全量断点周期检查点间隔秒（默认 `10`） |
| `state.backend` | 进度后端：`sqlite`（默认，本地文件）或 `mysql`（独立 MySQL） |
| `state.path` | SQLite 文件路径，默认 `godbsync.db`（仅 `sqlite` 后端有效） |
| `state.mysql` | 独立 MySQL 进度库连接信息（与目标库解耦，任意目标方言均可用） |

## 整库通配符同步
`tables[].name` 设为 `"*"` 可一次性同步整个源库的所有基表（自动排除视图与系统库表），无需逐张表声明。完整示例见 `examples/config.mysql2mysql.fulldb.yaml`（MySQL 整库 → MySQL）：

```yaml
tables:
  - name: "*"        # 匹配源库所有基表
    target: "*"      # 目标同名（可省略）
    mode: full
    truncate: true
    batch_size: 2000
```

- 展开在同步启动时（`syncer.Expand()`）由 `reader.ListTables` 枚举源库真实表名完成，目标表与源表同名，并继承 `mode`/`truncate`/`batch_size` 等选项；`init`/`full`/`incr`/`daemon`/`validate` 全部复用。
- 约束：整库通配符仅支持 `full`（增量需逐表 `watermark`）；`target` 须省略或为 `"*"`。
- 局限：`monitor`/`status` 等只读配置/进度后端的子命令不展开 `"*"`，只显示模板条目（已同步真实表名在进度后端中）。
- **结构不一致自动拦截**：`full` 在清空前会对每张已存在的目标表做结构预检（`PreSyncStructureCheck`）。若目标表缺列或列类型与源不一致，工具会打印对应的 `ALTER TABLE` 变更 SQL 并**直接退出（不 truncate、不写入，目标库保持原状）**，需你先执行生成的 DDL 再重跑。仅当通配符整库（`name: "*"`）时启用。

## 进度存储（配置文件 `state` 段）

```yaml
state:
  backend: sqlite       # sqlite（默认，本地纯 Go 实现）或 mysql（存入目标库）
  path: godbsync.db     # SQLite 文件路径（仅 backend=sqlite 时生效）
```

CLI `-state` 标志可覆盖配置文件：

| 取值 | 含义 |
| --- | --- |
| 不传（默认） | 使用配置文件 `state` 段，配置文件也未配置时默认 `sqlite` + `godbsync.db` |
| `mysql` | 存目标库 `godbsync_watermark` / `godbsync_checkpoint` |
| `file` | 存本地 `./godbsync_state.db` |
| `file=/path/to.db` | 指定路径的 SQLite 文件 |

- **MySQL 后端**：进度与数据同库，备份一致；多实例并发安全（目标库行锁天然串行化）。需建两张 `godbsync_*` 表。
- **SQLite 后端**：不污染目标库、无需 DDL 权限、单文件便携。数据提交与进度不属同一事务（靠幂等 upsert 兜底）。**多实例并发勿用**。

## 全量断点续传

`full`（未开 `truncate`）中断后重跑同一条命令即自动续传，无需额外参数：

- **锚点**：优先单列主键；Oracle 源无主键时回退 `ROWID`；MySQL/SQL Server 源无单列主键/多列主键时退化为无序全量（仍可读写，只是不支持断点续传）
- **幂等**：MySQL 目标用 `INSERT ... ON DUPLICATE KEY UPDATE`，Oracle 目标用 `MERGE`，SQL Server 目标用 `MERGE`，均保证已写入行不重复
- **重做**：`-fresh` 清除断点强制从头全量
- **`truncate: true`**：清空目标表 + 并发全量，运行中周期写检查点，中断后可续传（检测到断点时跳过 truncate）

## 特性

- **双向同步**：`Oracle ⇄ MySQL`、`SQL Server ⇄ MySQL`、`PostgreSQL ⇄ MySQL`、`PostgreSQL ⇄ Oracle`、`PostgreSQL ⇄ SQL Server`，由 `source.dialect` / `target.dialect` 决定方向，无需改代码
- **零客户端依赖**：Oracle [go-ora](https://github.com/sijms/go-ora) + MySQL `go-sql-driver/mysql` + SQL Server `microsoft/go-mssqldb` + PostgreSQL `lib/pq`（均纯 Go，无 CGO）
- **自动建表**：`init` 根据源列类型生成目标方言的 DDL（MySQL/Oracle/SQL Server/PostgreSQL 目标各自生成对应方言 DDL）
- **全量 + 断点续传**：分批幂等 upsert，truncate 与非 truncate 模式均支持中断续传
- **增量 + 周期检查点**：基于水位列持续推进，并发写入下周期持久化已提交水位
- **并发写入**：`workers` 配置批量并发插入
- **数据校验**：行数比对 + 主键抽样 SHA256 校验和；`auto_validate` 同步后自动触发
- **同步状态查看**：`monitor -cli` 终端报告 / `monitor`（或 `-web`）Web 仪表盘，只读进度后端（断点/水位）；`--verify` 实时 COUNT(*) 一致性校验，支持 `--watch N` 持续刷新、`--quick` 仅读进度后端
- **守护模式**：`daemon` 定时轮询增量
- **进度可配置**：默认本地 SQLite（`godbsync.db`），目标库为 MySQL 时也可一键切到目标库

## 目录结构

```
godbsync/
├── main.go
├── cmd/root.go             # 命令行分发（组合根，按方言实例化 Reader/Writer）
├── config/config.go        # 配置加载与校验（source/target 对称）
├── internal/
│   ├── dialect/            # Reader/Writer 接口 + 方言注册表（依赖倒置核心）
│   ├── types/              # 共享列元数据
│   ├── oracle/             # 实现 Reader（源）+ Writer（目标）
│   ├── mysql/              # 实现 Writer（目标）+ Reader（源）
│   ├── sqlserver/          # 实现 Reader（源）+ Writer（目标）
│   ├── state/              # 进度后端（MySQL + SQLite）
│   └── syncer/             # 同步编排核心（方向无关）
└── examples/
    ├── config.yaml                  # Oracle → MySQL 示例
    ├── config.mysql2oracle.yaml     # MySQL → Oracle 示例
    ├── config.sqlserver2mysql.yaml  # SQL Server → MySQL 示例
    ├── config.mysql2sqlserver.yaml  # MySQL → SQL Server 示例
    ├── config.oracle2sqlserver.yaml # Oracle → SQL Server 示例
    ├── config.pg2mysql.yaml         # PostgreSQL → MySQL 示例
    ├── config.mysql2pg.yaml         # MySQL → PostgreSQL 示例
    ├── config.pg2oracle.yaml        # PostgreSQL → Oracle 示例
    ├── config.oracle2pg.yaml        # Oracle → PostgreSQL 示例
    ├── config.sqlserver2pg.yaml     # SQL Server → PostgreSQL 示例
    ├── config.mysql2mysql.fulldb.yaml      # MySQL 整库 → MySQL（通配符 * 全库）
    └── config.mysql2oracle.statemysql.yaml  # MySQL → Oracle（状态存独立 MySQL）
```

## 类型映射

**Oracle → MySQL：**

| Oracle | MySQL |
| --- | --- |
| `VARCHAR2/NVARCHAR2(n)` | `VARCHAR(n)`（超长 → `TEXT`/`LONGTEXT`） |
| `CHAR/NCHAR(n)` | `CHAR(n)` |
| `NUMBER(p,s)` 整数 | `TINYINT`/`SMALLINT`/`INT`/`BIGINT`（超范围 → `DECIMAL`） |
| `NUMBER(p,s)` 小数 | `DECIMAL(p,s)` |
| `FLOAT`/`DOUBLE` | `DOUBLE` |
| `DATE`/`TIMESTAMP*` | `DATETIME(6)` |
| `CLOB`/`NCLOB` | `LONGTEXT` |
| `BLOB`/`RAW` | `LONGBLOB` |

**MySQL → Oracle：**

| MySQL | Oracle |
| --- | --- |
| `VARCHAR/NVARCHAR(n)` | `VARCHAR2(n)`（>4000 → `CLOB`） |
| `CHAR/NCHAR(n)` | `CHAR(n)` |
| `INT`/`BIGINT`/`SMALLINT`/`TINYINT`/`MEDIUMINT` | `NUMBER(10,0)` / `NUMBER(19,0)` / `NUMBER(5,0)` / `NUMBER(3,0)` |
| `DECIMAL(p,s)` / `NUMERIC` | `NUMBER(p,s)` |
| `FLOAT` / `DOUBLE` / `REAL` | `BINARY_FLOAT` / `BINARY_DOUBLE` |
| `DATE` / `DATETIME` / `TIMESTAMP` | `TIMESTAMP` |
| `TEXT` / `LONGTEXT` | `CLOB` |
| `BLOB` / `LONGBLOB` | `BLOB` |
| `JSON` | `CLOB` |
| `BOOLEAN` / `TINYINT(1)` | `NUMBER(1,0)` |

**SQL Server → MySQL：**

| SQL Server | MySQL |
| --- | --- |
| `NVARCHAR(n)` / `VARCHAR(n)` | `VARCHAR(n)`（超长 → `LONGTEXT`） |
| `NCHAR(n)` / `CHAR(n)` | `CHAR(n)` |
| `INT` / `BIGINT` / `SMALLINT` / `TINYINT` | 同名 |
| `DECIMAL(p,s)` / `NUMERIC` / `MONEY` / `SMALLMONEY` | `DECIMAL(p,s)` |
| `FLOAT` / `REAL` | `DOUBLE` |
| `DATETIME` / `DATETIME2` / `SMALLDATETIME` / `DATETIMEOFFSET` / `DATE` / `TIME` | `DATETIME(6)` |
| `NTEXT` / `XML` / `TEXT` | `LONGTEXT` |
| `BINARY` / `VARBINARY` / `IMAGE` | `LONGBLOB` |
| `BIT` | `TINYINT(1)` |
| `UNIQUEIDENTIFIER` | `VARCHAR(36)` |

**MySQL → SQL Server：**

| MySQL | SQL Server |
| --- | --- |
| `VARCHAR(n)` / `NVARCHAR(n)` | `NVARCHAR(n)`（>4000 → `NVARCHAR(MAX)`） |
| `CHAR(n)` / `NCHAR(n)` | `NCHAR(n)` |
| `INT` / `BIGINT` / `SMALLINT` / `TINYINT` | 同名 |
| `DECIMAL(p,s)` / `NUMERIC` | `DECIMAL(p,s)` |
| `FLOAT` | `REAL` / `DOUBLE` → `FLOAT(53)` |
| `DATETIME` / `TIMESTAMP` / `DATE` / `TIME` | `DATETIME2` |
| `TEXT` / `LONGTEXT` / `TINYTEXT` / `MEDIUMTEXT` | `NVARCHAR(MAX)` |
| `BLOB` / `LONGBLOB` / `TINYBLOB` / `MEDIUMBLOB` | `VARBINARY(MAX)` |
| `JSON` | `NVARCHAR(MAX)` |
| `BOOLEAN` / `TINYINT(1)` | `BIT` |
| `ENUM` / `SET` | `NVARCHAR(255)` |

**各类源 → PostgreSQL（目标端）：**

| 源类型（MySQL/Oracle/SQL Server） | PostgreSQL |
| --- | --- |
| `VARCHAR(n)` / `NVARCHAR(n)` / `VARCHAR2(n)` / `NVARCHAR2(n)` | `VARCHAR(n)`（超长 → `TEXT`） |
| `CHAR(n)` / `NCHAR(n)` | `CHAR(n)` |
| `INT` / `INTEGER` / `MEDIUMINT` / `SERIAL` | `INTEGER` |
| `BIGINT` / `BIGSERIAL` | `BIGINT` |
| `SMALLINT` / `SMALLSERIAL` | `SMALLINT` |
| `TINYINT` | `SMALLINT`（兼容无符号溢出） |
| `DECIMAL(p,s)` / `NUMERIC` / `NUMBER` | `NUMERIC(p,s)` |
| `MONEY` / `SMALLMONEY` | `NUMERIC(19,4)` |
| `FLOAT` / `BINARY_FLOAT` / `REAL` | `REAL` |
| `DOUBLE` / `BINARY_DOUBLE` | `DOUBLE PRECISION` |
| `BOOLEAN` / `BOOL` / `BIT` | `BOOLEAN` |
| `DATE` | `DATE` |
| `TIME` / `TIMETZ` | `TIME` |
| `DATETIME` / `DATETIME2` / `TIMESTAMP` / Oracle 时间类型 | `TIMESTAMP` |
| `TIMESTAMPTZ` / `DATETIMEOFFSET` | `TIMESTAMPTZ` |
| `TEXT` / `LONGTEXT` / `CLOB` / `NCLOB` / `NTEXT` / `XML` | `TEXT`（XML → `XML`） |
| `BLOB` / `LONGBLOB` / `RAW` / `VARBINARY` / `IMAGE` | `BYTEA` |
| `JSON` / `JSONB` | `JSONB` |
| `UUID` / `UNIQUEIDENTIFIER` | `UUID` |
| `ENUM` / `SET` | `TEXT` |
| `YEAR` | `SMALLINT` |

**PostgreSQL（源端）→ MySQL：**

| PostgreSQL | MySQL |
| --- | --- |
| `VARCHAR(n)` / `BPCHAR` | `VARCHAR(n)`（超长 → `LONGTEXT`） |
| `CHAR(n)` | `CHAR(n)`（>255 → `VARCHAR(255)`） |
| `INT2` / `SMALLSERIAL` | `SMALLINT` |
| `INT4` / `SERIAL` | `INT` |
| `INT8` / `BIGSERIAL` | `BIGINT` |
| `DECIMAL(p,s)` / `NUMERIC` | `DECIMAL(p,s)` |
| `FLOAT4` | `FLOAT` |
| `FLOAT8` / `DOUBLE PRECISION` | `DOUBLE` |
| `DATE` / `TIMESTAMP` / `TIMESTAMPTZ` / `TIME` / `TIMETZ` | `DATETIME(6)` |
| `TEXT` / `CITEXT` / `NAME` | `LONGTEXT` |
| `BYTEA` | `LONGBLOB` |
| `BOOLEAN` / `BOOL` | `TINYINT(1)` |
| `JSON` / `JSONB` | `JSON`（5.7+）/ `LONGTEXT`（<5.7） |
| `UUID` | `VARCHAR(36)` |
| `ENUM` | `VARCHAR(255)` |
| `XML` | `LONGTEXT` |

**PostgreSQL（源端）→ Oracle：**

| PostgreSQL | Oracle |
| --- | --- |
| `VARCHAR(n)` / `BPCHAR` | `VARCHAR2(n)`（>4000 → `CLOB`） |
| `INT2` / `SMALLSERIAL` | `NUMBER(5,0)` |
| `INT4` / `SERIAL` | `NUMBER(10,0)` |
| `INT8` / `BIGSERIAL` | `NUMBER(19,0)` |
| `DECIMAL(p,s)` / `NUMERIC` | `NUMBER(p,s)` |
| `FLOAT4` / `FLOAT8` / `DOUBLE PRECISION` / `REAL` | `BINARY_FLOAT` / `BINARY_DOUBLE`（10g+） |
| `DATE` / `TIMESTAMP` / `TIMESTAMPTZ` / `TIME` | `TIMESTAMP` |
| `TEXT` / `CITEXT` / `NAME` | `CLOB` |
| `BYTEA` | `BLOB` |
| `BOOLEAN` / `BOOL` | `NUMBER(1,0)` |
| `JSON` / `JSONB` | `CLOB` |
| `UUID` | `VARCHAR2(36)` |
| `ENUM` | `VARCHAR2(255)` |
| `YEAR` | `NUMBER(4,0)` |

**PostgreSQL（源端）→ SQL Server：**

| PostgreSQL | SQL Server |
| --- | --- |
| `VARCHAR(n)` / `BPCHAR` | `NVARCHAR(n)`（>4000 → `NVARCHAR(MAX)`） |
| `INT2` / `SMALLSERIAL` | `SMALLINT` |
| `INT4` / `SERIAL` | `INT` |
| `INT8` / `BIGSERIAL` | `BIGINT` |
| `DECIMAL(p,s)` / `NUMERIC` | `DECIMAL(p,s)` |
| `FLOAT4` | `REAL` |
| `FLOAT8` / `DOUBLE PRECISION` / `REAL` | `FLOAT(53)` |
| `DATE` / `TIMESTAMP` / `TIMESTAMPTZ` / `TIME` | `DATETIME2` |
| `TEXT` / `CITEXT` / `NAME` | `NVARCHAR(MAX)` |
| `BYTEA` | `VARBINARY(MAX)` |
| `BOOLEAN` / `BOOL` | `BIT` |
| `JSON` / `JSONB` | `NVARCHAR(MAX)` |
| `UUID` | `UNIQUEIDENTIFIER` |
| `ENUM` | `NVARCHAR(255)` |
| `YEAR` | `SMALLINT` |

> Oracle `NUMBER` 超出 `int64` 精度时以字符串透传，保留完整精度。

**SQL Server 同步要点：**
- `UNIQUEIDENTIFIER` 经 go-mssqldb 返回 16 字节，工具会自动规整为规范 GUID 字符串（如 `xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`）写入 MySQL `VARCHAR(36)`，不会损坏。
- `TINYINT`：仅 `TINYINT(1)` 映射为 `BIT`（布尔语义）；其余 `TINYINT` 一律映射为 `SMALLINT`，避免 MySQL `TINYINT UNSIGNED`(0–255) 溢出 SQL Server `TINYINT`(-128–127)。
- SQL Server 目标库批写入用 `MERGE ... USING (VALUES ...)`，单条语句最多 1000 行，工具已自动分块；建表用 `CREATE TABLE IF NOT EXISTS`。
- 连接加密：SQL Server 源/目标的 `encrypt` 字段留空默认 `encrypt=disable`（兼容自签名/内网）；连 Azure SQL 或开启强制加密的实例设 `encrypt: true`（自动加 `trustservercertificate=true`）。

**时间列精度**：工具建表用 `DATETIME(6)`（微秒），但旧表可能只有 `DATETIME`（秒级），写入时小数秒会四舍五入导致校验不一致。修复：`ALTER TABLE \`<表>\` MODIFY \`<列>\` DATETIME(6);` 后重跑全量。

## 构建

```bash
go build -ldflags="-s -w" -o godbsync.exe .      # Windows
$env:GOOS="linux"; $env:GOARCH="amd64"; go build -ldflags="-s -w" -o godbsync .   # 交叉编译→Linux
```

> 需要 Go 1.25+；国内 `go env -w GOPROXY=https://goproxy.cn,direct`。

## 日志输出
```
PS F:\code\company\godbsync> .\godbsync.exe full -config config.yaml
time="2026-07-13 15:08:54" level=info msg="源 Oracle <ORACLE_HOST>:1521/orcl (schema=<SCHEMA>) → 目标 MySQL <MYSQL_HOST>:3306/<DB>"
time="2026-07-13 15:08:54" level=info msg="目标表 bas_sn_type_relation_new 已存在，跳过建表（源表 FAN_TEST）" app=godbsync
time="2026-07-13 15:08:54" level=info msg="目标表 bas_sn_type_relation_new 存在断点=200000（pk），跳过清空，从断点续传 ID > 200000" app=godbsync
time="2026-07-13 15:08:55" level=info msg="开始全量同步 <SCHEMA>.FAN_TEST -> bas_sn_type_relation_new" app=godbsync
time="2026-07-13 15:09:25" level=info msg="  ... 已写入 962000/14821251 行 (6.5%) (bas_sn_type_relation_new) | 速度 31718 row/s | 预计剩余 7m16s" app=godbsync
time="2026-07-13 15:09:55" level=info msg="  ... 已写入 1930000/14821251 行 (13.0%) (bas_sn_type_relation_new) | 速度 31970 row/s | 预计剩余 6m43s" app=godbsync
```

## 安全

godbsync 把表名 / 列名 / schema / 库名等**标识符**直接拼进 SQL（经方言 `QuoteIdentifier` 包裹）。SQL 标识符不能用 `?`、`:n`、`$n` 等占位符绑定，因此无法像值那样走参数化查询；而各方言的 `QuoteIdentifier` 只做裸引号包裹、不转义内部引号字符。为此，工具在**拼 SQL 之前、于 `config.Validate()` 阶段**对全部标识符类字段做**白名单校验**，从源头阻断注入。

### 标识符白名单

仅放行安全字符，其余一律拒绝（空字段跳过，视为“未指定”合法）：

| 字段 | 允许字符 | 校验函数 |
|---|---|---|
| 库名 / 表名 / 列名 / schema / watermark / 列映射 | `[A-Za-z0-9_]`，长度 1..63 | `validIdentifier` |
| 表的 `target`（如 `owner.table`） | 单级点号分隔，每段为一个标识符，禁止 `..` 路径穿越 | `validQualifiedName` |
| `source/target/state.mysql` 的 host | `[A-Za-z0-9.:\-_/]`，含 IPv6 方括号 `[]`，长度 ≤253 | `validHost` |
| Oracle 的 `service`（service name / SID） | `[A-Za-z0-9._$#-]` | `validServiceName` |

覆盖位置：`source` / `target` 的 host、database、schema、service；`state.mysql` 的 host、database；每张表的 `name`、`target`、`schema`、`watermark`、`column_mapping` 的键与值。

### 为什么有效

白名单锁死字符集后，引号（`"` `` ` `` `[`）、空格、分号 `;`、注释符（`--` / `#`）等全部被拒，恶意标识符在 `Validate()` 阶段就返回错误、程序无法启动，根本到不了拼 SQL 那一步。例如表名写成 `users"; DROP TABLE secrets;--`，会被 `validIdentifier` 拦截并提示：

```
tables[0].name 含非法字符（仅允许字母、数字、下划线，长度 1..63）
```

### 设计取舍

- **仅接受 ASCII 标识符**：含中文或 `$`（Oracle 普通表名）的名称会被拒绝。这是有意的纵深防御——若业务确有中文表名需求，应在方言的 `QuoteIdentifier` 中**转义内部引号字符**，而非放宽本白名单。
- **白名单优于黑名单**：新增的攻击字符天然被拒，无需逐个补转义规则。
- 当 `config` 来源为不可信输入（如用户自助填表名）时，本校验是必要防线；若 `config` 永远由运维手改 yaml，本层不影响正常功能，仅作额外保险。

## 注意事项

- 不处理源端删除（无 CDC）
- 增量依赖单调水位列；MySQL 目标库需提前 `CREATE DATABASE`，Oracle/PostgreSQL 目标库需确保目标用户/schema 有建表权限，SQL Server 目标库需确保用户有建表权限
- 无主键表同步会产生重复行（MySQL 目标靠唯一键去重，Oracle/SQL Server 目标无主键时退化为普通 INSERT，PostgreSQL 目标无主键时退化为普通 INSERT），校验仅比行数、跳过内容比对
- 一批数据驻留内存，`batch_size` 过大可能超目标库单批语句长度/包大小限制（如 MySQL `max_allowed_packet`、Oracle 占位符上限、SQL Server 2100 参数上限，工具已自动分块）
- 非 truncate 全量路径串行提交；大表首跑追求速度可用 `truncate: true`（并发写入，且同样支持断点续传）
- SQLite 后端多实例勿并发跑同一目标库
- **目标库为 Oracle、SQL Server 或非 MySQL 方言时进度后端可用 `sqlite` 或「独立 MySQL」**（在 `state.mysql` 段配置任意 MySQL 连接，与目标库解耦）
