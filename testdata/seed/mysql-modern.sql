-- ============================================================================
-- godbsync CI 种子数据：MySQL 8.0+ 专属特性探针
--
-- 只被「MySQL 目标版本降级」这个 job 使用，不并入 mysql.sql，
-- 否则用 5.5/5.6 当源时会建表失败。
--
-- 覆盖 AGENTS.md 里记录的几条降级规则（目标版本 <8.0 时必须自动降级）：
--   JSON                    → <5.7 降为 LONGTEXT
--   utf8mb4_0900_ai_ci      → <8.0 降为 utf8mb4_general_ci
--   DATETIME(6)             → <5.6.4 去掉微秒参数
--   表级 COLLATE=0900       → 同上，随字符集一起降级
-- ============================================================================

DROP TABLE IF EXISTS T_MODERN;

CREATE TABLE T_MODERN (
  ID INT NOT NULL,
  J JSON DEFAULT NULL,
  V VARCHAR(200) COLLATE utf8mb4_0900_ai_ci DEFAULT NULL COMMENT '8.0 专属排序规则',
  D DATETIME(6) DEFAULT NULL COMMENT '微秒精度',
  T TEXT,
  PRIMARY KEY (ID)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='DDL 降级探针';

INSERT INTO T_MODERN (ID, J, V, D, T) VALUES
(1, '{"a": 1, "b": "中文"}', 'collation 0900 探针', '2024-05-06 07:08:09.123456', '普通文本'),
(2, '[1, 2, 3]', '第二行', '2024-05-06 07:08:09.000000', NULL),
(3, NULL, NULL, NULL, NULL);

SELECT 'T_MODERN' AS TAB, COUNT(*) AS ROWS_ FROM T_MODERN;
