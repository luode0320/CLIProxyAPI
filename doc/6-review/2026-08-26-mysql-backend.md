# 6-review: MySQL storage backend (2026-08-26)

## 结论

`STYLE: PASS`

## 检查范围

- 新增：`internal/store/mysqlstore.go`、`internal/store/mysql_cooldown_store.go`、`internal/store/mysql_cooldown_store_test.go`
- 修改：`internal/store/postgres_cooldown_store_test.go`（mock driver 参数判据兼容 7-arg）、`sdk/cliproxy/auth/classification.go`（`AuthSourceMySQL`）、`cmd/server/main.go`（MYSQL_DSN 接线）、`go.mod`/`go.sum`（go-sql-driver/mysql v1.10.0）

## 来源对象

用户需求：为 CLIProxyAPI 新增 MySQL 存储后端，与现有 PostgreSQL 后端并存。

## 跟随依据

- 全部新代码以 `internal/store/postgresstore.go` 与 `postgres_cooldown_store.go` 为局部风格基线（结构、方法顺序、错误前缀、日志风格逐一对齐），未引入个人偏好写法。
- 注释遵循仓库 AGENTS.md「Comments in English only」约定，与既有文件一致。
- 测试复用同包既有 mock driver（`cooldownTestDriver` 等），仅扩展参数判据，避免重复 mock。
- 命名与既有术语系统对称：`MySQLStore` ↔ `PostgresStore`、`mysqlCooldownStateStore` ↔ `postgresCooldownStateStore`、`AuthSourceMySQL` ↔ `AuthSourcePostgres`。

## 发现与处理

| 检查项 | 结论 | 说明 |
|---|---|---|
| gofmt / 换行 / 尾随空白 | PASS | `gofmt -w` 已执行 |
| 命名一致性 | PASS | 与 PG 版命名族对称，无歧义 |
| 注释 | PASS | 英文注释、函数头齐备；改动位点（mock 判据）已补说明注释 |
| 文件目录归位 | PASS | 存储实现均位于 `internal/store/` |
| 依赖方向 | PASS | store 包仅依赖 sdk/auth，无反向引用 |
| 测试资产归位 | PASS | 与既有 store 测试同目录同包（跟随项目现状） |
| 公共工具复用 | PASS | 复用 `jsonEqual`、`valueAsString`、`labelFor`、`normalizeAuthID`、`normalizeLineEndings` |
| 局部风格跳变 | PASS | 无 |

## 刻意不处理项（code-quality 排除记录）

1. `mysqlstore.go` 约 610 行，超过 500 行阈值，但为 `postgresstore.go`（700+ 行）的等价移植，保持两后端文件结构对称更利于维护；拆分将破坏对称性，记为刻意不处理。
2. 未抽象共享 Store 接口：`PostgresStore`/`MySQLStore` 均为具体类型，main.go 以分支选择。现仅两个实现且差异集中在 SQL 方言，接口抽象收益低，符合「默认不抽象接口」原则。
3. 测试资产放源码目录同包：与仓库既有全部 store 测试（`gitstore_test.go`、`postgres_cooldown_store_test.go`）模式一致，且需访问包内未导出 mock 类型；迁移到根 `test/` 镜像会破坏白盒复用，记为刻意不处理（与 test-program-rules 的差异另行记录）。

## 备注

- gitstore 的 corruption recovery 测试在本机 Windows 环境失败（`.git` 目录 rename `Access is denied`），已在无改动基线验证为预置环境问题，与本次改动无关。
