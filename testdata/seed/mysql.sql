-- ============================================================================
-- godbsync CI 种子数据：MySQL 源端
--
-- 版本要求：本文件用 DATETIME(6) / LONGTEXT，需 MySQL 5.7+ 作为【源端】。
-- 5.5 / 5.6 只作为【目标端】参与测试（见 workflow 的 mysql-versions job），
-- 它们的建表由 godbsync 按版本自动降级完成，不经由本文件。
--
-- 设计约束（改动前请先读）：
--   1. 所有标识符统一【大写】。Oracle 目标端 GenerateCreate 用 "..." 双引号原样
--      建表，而 EnsureTable 查的是 all_tables 的大写名；小写表名会导致「表已存在」
--      被误判为不存在，二次同步直接 ORA-00955。
--   2. 五张表跨四个方言【同构】：同名、同列序、同语义类型。任何方言的 seed 改动
--      都必须同步到另外三份，否则 CI 矩阵里的方向会测到不同的东西。
--   3. 表名/列名不要加引号，让各方言按自身默认规则处理（MySQL 反引号由 Godbsync
--      的 QuoteIdentifier 在同步时补，seed 里不需要）。
-- ============================================================================

SET NAMES utf8mb4;

DROP TABLE IF EXISTS T_ANCHOR;
DROP TABLE IF EXISTS T_TYPES;
DROP TABLE IF EXISTS T_RESERVED;
DROP TABLE IF EXISTS T_NOPK;
DROP TABLE IF EXISTS T_EMPTY;

-- ---------------------------------------------------------------------------
-- T_ANCHOR：单列主键 + 单调时间列，验证「有序断点续传」与「增量水位」。
-- 1200 行 / 默认 batch_size=500 → 至少 3 个批次，足够覆盖并发有序窗口。
-- ---------------------------------------------------------------------------
CREATE TABLE T_ANCHOR (
  ID BIGINT NOT NULL AUTO_INCREMENT,
  NAME VARCHAR(64) NOT NULL COMMENT '业务名称（含中文，验证字符集透传）',
  AMOUNT DECIMAL(18,4) DEFAULT NULL,
  UPDATED_AT DATETIME(6) NOT NULL COMMENT '增量水位列，按行号单调递增',
  PRIMARY KEY (ID)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='断点续传与水位探针';

DROP PROCEDURE IF EXISTS SP_SEED_ANCHOR;
DELIMITER $$
CREATE PROCEDURE SP_SEED_ANCHOR()
BEGIN
  DECLARE i INT DEFAULT 1;
  WHILE i <= 1200 DO
    INSERT INTO T_ANCHOR (NAME, AMOUNT, UPDATED_AT)
    VALUES (
      CONCAT('row-', LPAD(i, 5, '0'), '-中文'),
      ROUND(i * 1.2345, 4),
      TIMESTAMPADD(SECOND, i, '2024-01-01 00:00:00')
    );
    SET i = i + 1;
  END WHILE;
END$$
DELIMITER ;

-- 1200 次单行 INSERT 放进一个事务，避免 autocommit 逐条 fsync 拖慢 seed。
START TRANSACTION;
CALL SP_SEED_ANCHOR();
COMMIT;

DROP PROCEDURE SP_SEED_ANCHOR;

-- ---------------------------------------------------------------------------
-- T_TYPES：类型边界探针。精度 / 长文本 / 微秒 / 二进制 / 全 NULL 行。
-- 第 6 行除主键外全部 NULL —— Oracle 的 '' 等价于 NULL，这是跨库最容易出偏差的一行。
-- ---------------------------------------------------------------------------
CREATE TABLE T_TYPES (
  ID INT NOT NULL,
  C_DEC DECIMAL(38,10) DEFAULT NULL,
  C_BIG BIGINT DEFAULT NULL,
  C_FLOAT DOUBLE DEFAULT NULL,
  C_VC VARCHAR(255) DEFAULT NULL,
  C_TXT LONGTEXT,
  C_DATE DATE DEFAULT NULL,
  C_TS DATETIME(6) DEFAULT NULL,
  C_BOOL TINYINT(1) DEFAULT NULL,
  C_BIN LONGBLOB,
  PRIMARY KEY (ID)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='类型映射边界探针';

INSERT INTO T_TYPES (ID, C_DEC, C_BIG, C_FLOAT, C_VC, C_TXT, C_DATE, C_TS, C_BOOL, C_BIN) VALUES
(1, 1234567890123456789012345678.0123456789, 9223372036854775807, 3.141592653589793,
 '普通中文与 ASCII 混排 abcXYZ', REPEAT('长文本测试', 1500),
 '2024-03-05', '2024-03-05 12:34:56.123456', 1, UNHEX('000102030405060708090A0B0C0D0E0F')),
(2, -1234567890123456789012345678.0123456789, -9223372036854775808, -2.718281828459045,
 '负数与极小值', '短文本',
 '1970-01-02', '1970-01-02 00:00:00.000001', 0, UNHEX('FFEEDDCCBBAA99887766554433221100')),
(3, 0.0000000001, 0, 0,
 '', '', 
 '2024-12-31', '2024-12-31 23:59:59.999999', 0, UNHEX('00')),
(4, 9999999999999999999999999999.9999999999, 1, 1.5,
 CONCAT('特殊字符: 单引号 '' 反斜杠 \\ 制表 ', CHAR(9), ' 换行 ', CHAR(10), ' 表情 😀🎉 结束'),
 '含 emoji 的文本 😀', '2000-02-29', '2000-02-29 00:00:00.000000', 1, UNHEX('DEADBEEF')),
(5, -0.0000000001, -1, -1.5,
 '边界长度: 这是一个用于验证 255 字符上限的填充值', REPEAT('x', 100),
 '2038-01-18', '2038-01-19 03:14:07.000000', 1, NULL),
(6, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL);

-- ---------------------------------------------------------------------------
-- T_RESERVED：保留字作为列名，验证 QuoteIdentifier 在四个方言下的正确性。
-- ORDER / GROUP / DESC / KEY 在 Oracle 与 MySQL 都是硬保留字。
-- ---------------------------------------------------------------------------
CREATE TABLE T_RESERVED (
  ID INT NOT NULL,
  `ORDER` INT DEFAULT NULL,
  `GROUP` VARCHAR(32) DEFAULT NULL,
  `DESC` VARCHAR(64) DEFAULT NULL,
  `KEY` VARCHAR(32) DEFAULT NULL,
  PRIMARY KEY (ID)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='保留字列名探针';

INSERT INTO T_RESERVED (ID, `ORDER`, `GROUP`, `DESC`, `KEY`) VALUES
(1, 100, 'g-alpha', '第一行描述', 'k1'),
(2, -100, 'g-beta', '第二行描述', 'k2'),
(3, 0, '', '第三行描述', 'k3');

-- ---------------------------------------------------------------------------
-- T_NOPK：无主键表。MySQL 源会退化为「无序全量」（不续传、不校验边界）；
-- Oracle 源则走 ROWID 断点 —— 同一张表在矩阵里测到的是两条不同的代码路径。
-- ---------------------------------------------------------------------------
CREATE TABLE T_NOPK (
  K1 INT DEFAULT NULL,
  K2 VARCHAR(32) DEFAULT NULL,
  K3 DECIMAL(10,2) DEFAULT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='无主键表';

INSERT INTO T_NOPK (K1, K2, K3)
SELECT ID, CONCAT('np-', ID), AMOUNT FROM T_ANCHOR WHERE ID <= 50;

-- ---------------------------------------------------------------------------
-- T_EMPTY：0 行表。验证建表成功且全量同步不因空结果集报错。
-- ---------------------------------------------------------------------------
CREATE TABLE T_EMPTY (
  ID INT NOT NULL,
  V VARCHAR(10) DEFAULT NULL,
  PRIMARY KEY (ID)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='空表';

-- ---------------------------------------------------------------------------
-- 自检：各表预期行数（CI 里会在同步前打印一次，便于定位 seed 是否生效）
-- ---------------------------------------------------------------------------
SELECT 'T_ANCHOR' AS TAB, COUNT(*) AS ROWS_ FROM T_ANCHOR
UNION ALL SELECT 'T_TYPES', COUNT(*) FROM T_TYPES
UNION ALL SELECT 'T_RESERVED', COUNT(*) FROM T_RESERVED
UNION ALL SELECT 'T_NOPK', COUNT(*) FROM T_NOPK
UNION ALL SELECT 'T_EMPTY', COUNT(*) FROM T_EMPTY;
