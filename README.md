# deployment-tracker

部署与发布记录服务。它跟踪每次发布的目标环境、版本、变更条目和上线门禁状态，并提供环境之间的差异查询与回滚点查询。

## 运行要求

- Go 1.22 或以上
- SQLite（本服务自带存储，不需要外部数据库）

## 构建、测试与启动

```bash
go build ./...
go test ./...
go run .
```

服务默认监听 `127.0.0.1:8080`。可用环境变量覆盖：

| 变量 | 默认值 | 用途 |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | HTTP 监听地址 |
| `DB_PATH` | `deployment-tracker.db` | SQLite 数据库文件路径 |

## 已公开的入口

### `GET /healthz`

返回服务与存储状态。正常时 HTTP 200：

```json
{"status":"ok","database":"ok"}
```

存储不可用时 HTTP 503：

```json
{"error":{"code":"storage_unavailable","message":"database is not available"}}
```

### `POST /releases`

登记一条发布记录。记录保存后不可修改。请求体：

```json
{
  "environment": "prod",
  "version": "1.2.0",
  "changes": ["add login", "fix logout"],
  "gate_status": "allowed",
  "rollback_point": "1.1.0"
}
```

- `environment`、`version`、`changes`、`gate_status`、`rollback_point` 为必填；`name` 可选。
- `gate_status` 只接受 `allowed`、`blocked`、`pending`。
- 字段缺失、类型不符或门禁状态非法时返回 HTTP 400 `invalid_release_input`；请求体不是合法 JSON 时返回 HTTP 400 `invalid_request`。

新建成功返回 HTTP 201，响应体为 `{"release": {...}}`，包含 `id`、`environment`、`version`、`changes`、`gate_status`、`rollback_point`、`registered_at`。

相同 `environment` 与 `version` 重复登记时：

- 内容完全相同（变更条目按集合比较，与顺序无关）返回 HTTP 200 与已有记录，不新建。
- 内容不同返回 HTTP 409 `release_conflict`。

### `GET /releases`

按条件查询发布记录。查询参数都可省略，组合时按 AND 生效：

| 参数 | 匹配字段 |
|---|---|
| `environment` | 目标环境 |
| `version` | 版本 |
| `change` | 单个变更条目（包含即匹配） |
| `gate_status` | 门禁状态 |
| `rollback_point` | 回滚点 |

返回 HTTP 200 `{"releases": [...]}`，按登记顺序（从旧到新）排列；无匹配时 `releases` 为空数组。查询不存在的环境返回 HTTP 404 `environment_not_found`。

### `GET /releases/{environment}/{version}`

返回指定环境与版本的有效记录（同一版本有多条历史时取登记顺序靠后的那条）。环境不存在返回 HTTP 404 `environment_not_found`；环境存在但版本无记录返回 HTTP 404 `release_not_found`。

### `GET /environments/{environment}/history`

历史追溯：按登记顺序返回该环境从旧到新的全部记录，每条保留版本、变更条目、门禁状态和回滚点。返回 HTTP 200 `{"environment": "...", "releases": [...]}`。环境不存在返回 HTTP 404 `environment_not_found`。

### `GET /compare?left=<env>&right=<env>[&until=<version>]`

比较两个不同目标环境在截止版本（含）范围内可追溯的记录。`until` 可省略，表示不限制。返回 HTTP 200：

```json
{
  "left": "staging",
  "right": "prod",
  "until": "1.5.0",
  "left_versions": ["1.0.0"],
  "right_versions": ["1.0.0", "1.2.0"],
  "common_versions": ["1.0.0"],
  "only_left_versions": [],
  "only_right_versions": ["1.2.0"],
  "version_diffs": [
    {
      "version": "1.0.0",
      "added_changes": ["d"],
      "removed_changes": ["b"],
      "left_gate_status": "allowed",
      "right_gate_status": "blocked",
      "gate_status_changed": true
    }
  ],
  "left_rollback_points": ["0.9.0"],
  "right_rollback_points": ["0.8.0", "1.0.0"]
}
```

- 所有版本列表按版本排序（点分段：数字段按数值，其余按字典序）；同一版本有多条历史时采用登记顺序靠后的有效记录。
- `added_changes` / `removed_changes` 按 left → right 方向解读：right 有而 left 没有为新增，反之为删除；按字典序排列。
- 没有共同版本时 `common_versions` 与 `version_diffs` 为确定的空数组。
- `left` 与 `right` 相同返回 HTTP 409 `comparison_conflict`；任一环境不存在返回 HTTP 404 `environment_not_found`；缺少 `left` 或 `right` 返回 HTTP 400 `invalid_request`。

### 旧记录的读取约定

本次新增字段（`changes`、`gate_status`、`rollback_point`）只对新登记和差异比较生效。早于这些字段写入的记录保持原值可读：输出中跳过这三个字段，不判为无效，也不补写。

## 错误返回约定

所有错误响应使用同一个 JSON 形状，并且只包含 `error` 一个顶层键：

```json
{"error":{"code":"<机器可读的短标识>","message":"<一句话说明>"}}
```

`code` 使用小写下划线形式，`message` 不包含堆栈、文件路径或 SQL。

当前使用的 `code`：`invalid_request`、`not_found`、`conflict`、`storage_unavailable`、`invalid_release_input`、`release_not_found`、`environment_not_found`、`release_conflict`、`comparison_conflict`。
