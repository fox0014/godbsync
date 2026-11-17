-- ============================================================================
-- godbsync CI 种子数据：SQL Server 源端（2019 / 2022）
--
-- 与 mysql.sql / postgresql.sql / oracle.sql 保持一致：同名、同列序、同语义类型。
-- 注意事项：
--   * 中文一律用 N'' 前缀，否则按数据库排序规则降级为 VARCHAR 会丢字符。
--   * 保留字列名用 [] 包裹；T-SQL 里 ORDER / GROUP / DESC / KEY 均为保留字。
--   * 本文件由 sqlcmd -i 读取，执行时必须带 -f 65001（UTF-8），否则中文乱码。
-- ============================================================================

IF OBJECT_ID('dbo.T_ANCHOR', 'U') IS NOT NULL DROP TABLE dbo.T_ANCHOR;
IF OBJECT_ID('dbo.T_TYPES', 'U') IS NOT NULL DROP TABLE dbo.T_TYPES;
IF OBJECT_ID('dbo.T_RESERVED', 'U') IS NOT NULL DROP TABLE dbo.T_RESERVED;
IF OBJECT_ID('dbo.T_NOPK', 'U') IS NOT NULL DROP TABLE dbo.T_NOPK;
IF OBJECT_ID('dbo.T_EMPTY', 'U') IS NOT NULL DROP TABLE dbo.T_EMPTY;
GO

-- ---------------------------------------------------------------------------
-- T_ANCHOR：1200 行，单列主键（IDENTITY）+ 单调时间列
-- ---------------------------------------------------------------------------
CREATE TABLE dbo.T_ANCHOR (
  ID BIGINT IDENTITY(1,1) NOT NULL CONSTRAINT PK_T_ANCHOR PRIMARY KEY,
  NAME NVARCHAR(64) NOT NULL,
  AMOUNT DECIMAL(18,4) NULL,
  UPDATED_AT DATETIME2(6) NOT NULL
);
GO

EXEC sys.sp_addextendedproperty @name=N'MS_Description', @value=N'断点续传与水位探针',
  @level0type=N'SCHEMA', @level0name=N'dbo', @level1type=N'TABLE', @level1name=N'T_ANCHOR';
GO

;WITH N AS (
  SELECT TOP (1200) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS I
  FROM sys.all_objects a CROSS JOIN sys.all_objects b
)
INSERT INTO dbo.T_ANCHOR (NAME, AMOUNT, UPDATED_AT)
SELECT N'row-' + RIGHT('00000' + CAST(I AS NVARCHAR(10)), 5) + N'-中文',
       CAST(I * 1.2345 AS DECIMAL(18,4)),
       DATEADD(SECOND, I, CAST('2024-01-01T00:00:00' AS DATETIME2(6)))
FROM N;
GO

-- ---------------------------------------------------------------------------
-- T_TYPES：类型边界探针
-- ---------------------------------------------------------------------------
CREATE TABLE dbo.T_TYPES (
  ID INT NOT NULL CONSTRAINT PK_T_TYPES PRIMARY KEY,
  C_DEC DECIMAL(38,10) NULL,
  C_BIG BIGINT NULL,
  C_FLOAT FLOAT(53) NULL,
  C_VC NVARCHAR(255) NULL,
  C_TXT NVARCHAR(MAX) NULL,
  C_DATE DATE NULL,
  C_TS DATETIME2(6) NULL,
  C_BOOL BIT NULL,
  C_BIN VARBINARY(MAX) NULL
);
GO

INSERT INTO dbo.T_TYPES (ID, C_DEC, C_BIG, C_FLOAT, C_VC, C_TXT, C_DATE, C_TS, C_BOOL, C_BIN) VALUES
(1, 1234567890123456789012345678.0123456789, 9223372036854775807, 3.141592653589793,
 N'普通中文与 ASCII 混排 abcXYZ', REPLICATE(N'长文本测试', 1500),
 '2024-03-05', '2024-03-05T12:34:56.123456', 1, 0x000102030405060708090A0B0C0D0E0F),
(2, -1234567890123456789012345678.0123456789, -9223372036854775808, -2.718281828459045,
 N'负数与极小值', N'短文本',
 '1970-01-02', '1970-01-02T00:00:00.000001', 0, 0xFFEEDDCCBBAA99887766554433221100),
(3, 0.0000000001, 0, 0,
 N'', N'',
 '2024-12-31', '2024-12-31T23:59:59.999999', 0, 0x00),
(4, 9999999999999999999999999999.9999999999, 1, 1.5,
 N'特殊字符: 单引号 '' 反斜杠 \ 制表 ' + NCHAR(9) + N' 换行 ' + NCHAR(10) + N' 表情 😀🎉 结束',
 N'含 emoji 的文本 😀', '2000-02-29', '2000-02-29T00:00:00.000000', 1, 0xDEADBEEF),
(5, -0.0000000001, -1, -1.5,
 N'边界长度: 这是一个用于验证 255 字符上限的填充值', REPLICATE(N'x', 100),
 '2038-01-18', '2038-01-19T03:14:07.000000', 1, NULL),
(6, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL);
GO

-- ---------------------------------------------------------------------------
-- T_RESERVED：保留字列名
-- ---------------------------------------------------------------------------
CREATE TABLE dbo.T_RESERVED (
  ID INT NOT NULL CONSTRAINT PK_T_RESERVED PRIMARY KEY,
  [ORDER] INT NULL,
  [GROUP] NVARCHAR(32) NULL,
  [DESC] NVARCHAR(64) NULL,
  [KEY] NVARCHAR(32) NULL
);
GO

INSERT INTO dbo.T_RESERVED (ID, [ORDER], [GROUP], [DESC], [KEY]) VALUES
(1, 100, N'g-alpha', N'第一行描述', N'k1'),
(2, -100, N'g-beta', N'第二行描述', N'k2'),
(3, 0, N'', N'第三行描述', N'k3');
GO

-- ---------------------------------------------------------------------------
-- T_NOPK：无主键表（SQL Server 源退化为无序全量）
-- ---------------------------------------------------------------------------
CREATE TABLE dbo.T_NOPK (
  K1 INT NULL,
  K2 NVARCHAR(32) NULL,
  K3 DECIMAL(10,2) NULL
);
GO

INSERT INTO dbo.T_NOPK (K1, K2, K3)
SELECT TOP (50) CAST(ID AS INT), N'np-' + CAST(ID AS NVARCHAR(10)), CAST(AMOUNT AS DECIMAL(10,2))
FROM dbo.T_ANCHOR ORDER BY ID;
GO

-- ---------------------------------------------------------------------------
-- T_EMPTY：0 行表
-- ---------------------------------------------------------------------------
CREATE TABLE dbo.T_EMPTY (
  ID INT NOT NULL CONSTRAINT PK_T_EMPTY PRIMARY KEY,
  V NVARCHAR(10) NULL
);
GO

-- ---------------------------------------------------------------------------
-- 自检：各表预期行数
-- ---------------------------------------------------------------------------
SELECT 'T_ANCHOR' AS TAB, COUNT(*) AS ROWS_ FROM dbo.T_ANCHOR
UNION ALL SELECT 'T_TYPES', COUNT(*) FROM dbo.T_TYPES
UNION ALL SELECT 'T_RESERVED', COUNT(*) FROM dbo.T_RESERVED
UNION ALL SELECT 'T_NOPK', COUNT(*) FROM dbo.T_NOPK
UNION ALL SELECT 'T_EMPTY', COUNT(*) FROM dbo.T_EMPTY;
GO
