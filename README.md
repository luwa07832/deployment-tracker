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

## 可追溯发布记录 API（/api/v1）

第二套发布记录能力与基线接口并存，全部位于 `/api/v1` 前缀下。它不改变上面任何入口的请求、响应、状态码或排序；新记录写入独立的表，基线入口读不到它们，反之亦然。错误响应仍是 `{"error":{"code":...,"message":...}}` 这一形状。

### 环境登记

目标环境必须先登记，发布记录才能写入。

- `POST /api/v1/environments`：请求体 `{"environment":"prod","display_name":"Production"}`，`display_name` 可选。首次登记返回 201；同一 `environment` 重复登记是幂等的，返回 200 与已有记录（不会覆盖显示名）。`environment` 缺失或为空白时返回 422 `ENVIRONMENT_VALIDATION_FAILED`。
- `GET /api/v1/environments`：返回 `{"environments":[...]}`，按环境标识排序；每项含 `environment`、`display_name`、`registered_at`。
- `GET /api/v1/environments/{environment}`：返回 `{"environment":{...}}`；不存在返回 404 `ENVIRONMENT_NOT_FOUND`。

### `POST /api/v1/release-records`

登记一条可追溯发布记录。请求体：

```json
{
  "environment": "prod",
  "version": "1.2.0",
  "changes": [
    {"sequence": 1, "category": "feature", "title": "add login", "description": "users can sign in"},
    {"sequence": 2, "category": "fix", "title": "fix logout", "description": "logout clears the session"}
  ],
  "gate_status": "allowed",
  "rollback_point": "snapshot:1.1.0@sha256:abcdef"
}
```

- `environment`、`version`、`changes`、`gate_status`、`rollback_point` 均为必填；`changes` 为数组（允许空数组）。
- `batch_id` 为可选的发布批次标识（非空白字符串，最长 128 字符）。省略或为空白时记录不属于任何批次，并保持既有的“同一环境同一版本唯一”约束；携带批次时按 `(batch_id, environment, version)` 唯一，因此不同批次可以复用同一版本，同一批次在同一环境也可登记多个不同版本（同一批次内同一版本重复提交仍返回 409）。批次标识是不透明稳定标识，服务端只原样保存，不推断其内容。
- 每个变更条目的 `category`、`title`、`description` 为必填非空字符串；`sequence` 为可选正整数。要么所有条目都带 `sequence`（且互不相同），要么全部省略——全省略时服务端按数组顺序从 1 编号。
- `gate_status` 只接受 `allowed`、`blocked`、`pending`。
- `rollback_point` 是公开入口中的稳定标识，服务端只原样保存，不推断、不读取或生成任何文件内容。
- 成功返回 201 与 `{"release_record": {...}}`，记录包含：
  - `id`：稳定记录标识，形如 `rel_` 加 32 个十六进制字符（不暴露数据库主键）；
  - `recorded_at`：服务端生成的记录时间，UTC，`YYYY-MM-DDTHH:MM:SSZ`；
  - 完整的 `environment`、`version`、`changes`、`gate_status`、`rollback_point`；登记时携带了批次时还包含 `batch_id`（无批次记录不输出该字段）。
- 写入成功后立即可由下面的查询入口与差异比对读取。

错误码（错误体仍只有 `error` 一个顶层键）：

| 场景 | 状态码 | code |
|---|---|---|
| 目标环境未登记 | 404 | `ENVIRONMENT_NOT_FOUND` |
| 同一环境同一版本重复提交 | 409 | `RELEASE_ALREADY_EXISTS` |
| 必填字段缺失、类型不符、`sequence` 非法或重复、门禁枚举非法、回滚点为空白 | 422 | `RELEASE_VALIDATION_FAILED` |
| 请求体不是合法 JSON | 400 | `invalid_request` |

### `GET /api/v1/release-records`

查询参数全部可选，按 AND 组合：

| 参数 | 含义 |
|---|---|
| `environment` | 目标环境；未登记时 404 `ENVIRONMENT_NOT_FOUND` |
| `version` | 精确版本 |
| `gate_status` | 门禁状态；非枚举值返回 422 `RELEASE_VALIDATION_FAILED` |
| `batch_id` | 精确发布批次标识；只返回该批次的记录，不影响其它过滤条件 |
| `recorded_from` / `recorded_to` | 记录时间闭区间，支持 RFC3339 时刻或 `YYYY-MM-DD` 日期（日期分别取当日 00:00:00 / 23:59:59 UTC）；无法解析返回 422 `RELEASE_VALIDATION_FAILED` |

返回 200 `{"release_records":[...]}`，按 `recorded_at` 倒序；同一时刻再按写入顺序倒序，保证结果确定。空结果为 `[]`。

### `GET /api/v1/release-records/{id}`

按稳定标识取得单条完整记录（含全部变更条目与回滚点）；不存在返回 404 `RELEASE_RECORD_NOT_FOUND`。

### `GET /api/v1/compare?left=<env>&right=<env>[&version=<v> | &as_of=<time>]`

比较两个已登记环境，读取语义与上面的查询入口完全一致。范围口径二选一：

- 都不给：比较两侧全部可追溯记录；
- `as_of=<time>`：截止时间口径（含），只比较 `recorded_at` 不晚于该时刻的记录；取值为 RFC3339 时刻或 `YYYY-MM-DD` 日期（日期取当日 23:59:59 UTC）。这是一个时间范围，范围为空（如过去的日期）是合法的 200 空结果；无法解析返回 400 `INVALID_COMPARE_RANGE`；
- `version=<version>`：精确基线版本口径，只比较该版本；该版本在两侧都不存在时返回 400 `RELEASE_VERSION_NOT_FOUND`（只存在于一侧是合法的漂移场景，不报错）。

成功返回 200：

```json
{
  "left": "staging",
  "right": "prod",
  "as_of": "2026-09-30T23:59:59Z",
  "compared_at": "2026-10-01T08:30:00Z",
  "left_versions": ["1.0.0"],
  "right_versions": ["1.0.0", "1.2.0"],
  "common_versions": ["1.0.0"],
  "only_left_versions": [],
  "only_right_versions": ["1.2.0"],
  "version_diffs": [
    {
      "version": "1.0.0",
      "field_diffs": [
        {"field": "rollback_point", "left": "snap:0.9.0", "right": "snap:0.8.0"}
      ],
      "gate_status": {"left": "allowed", "right": "blocked", "changed": true},
      "added_changes": [{"sequence": 2, "category": "feature", "title": "c", "description": "..."}],
      "removed_changes": [{"sequence": 2, "category": "fix", "title": "b", "description": "..."}],
      "changed_changes": [
        {"left": {"sequence": 1, "category": "feature", "title": "a", "description": "old"},
         "right": {"sequence": 1, "category": "feature", "title": "a", "description": "new"}}
      ]
    }
  ]
}
```

- 方向固定为 left → right：`added_changes` 是右侧有、左侧无；`removed_changes` 相反；变更条目按 `title` 识别为同一条，序号、分类或描述不同进入 `changed_changes`。
- `field_diffs` 列出共有版本上其它标量字段的差异（目前包含 `rollback_point`）。
- 所有版本列表按点分段版本序排列，空结果是确定的空数组；`baseline_version` 仅在版本口径下出现，`as_of` 仅在截止时间口径下出现（无范围口径时二者均省略）；`compared_at` 为服务端比较时刻（UTC 秒精度），用于解释结果。除该时间戳外，同一输入的结果逐字节稳定。

错误码：

| 场景 | 状态码 | code |
|---|---|---|
| `left` 与 `right` 相同 | 400 | `SAME_ENVIRONMENT_COMPARE` |
| 缺少 `left`/`right`、同时给了 `version` 与 `as_of`，或 `as_of` 无法解析 | 400 | `INVALID_COMPARE_RANGE` |
| 精确版本在两侧都不存在 | 400 | `RELEASE_VERSION_NOT_FOUND` |
| 任一环境未登记 | 404 | `ENVIRONMENT_NOT_FOUND` |

这些问题不会被静默折叠成空结果。

### 发布晋级链路（只读）

链路入口把同一发布批次（`batch_id`）在多个已登记环境中的发布事实关联起来。它们只读取既有发布记录，绝不写入或改写历史；相同输入的响应逐字节稳定（无服务端时间戳）。变更条目沿用既有结构，其稳定标识为 `title`（与差异比对的识别口径一致）；时间沿用 `recorded_at`，版本沿用点分段版本序。

#### `GET /api/v1/release-batches/{batch_id}/chain?environments=dev,test,staging,prod`

返回整条晋级链路。`environments` 为逗号分隔的有序环境序列。节点内的发布事实按 `recorded_at` 从旧到新完整列出（同一时刻按写入顺序），同一环境在该批次内的多次发布全部保留，不折叠为最后一次；不同批次即使版本相同也严格隔离。

成功返回 200：

```json
{
  "batch_id": "b1",
  "environment_order": ["dev", "test", "staging", "prod"],
  "nodes": [
    {
      "environment": "dev",
      "releases": [
        {
          "id": "rel_...",
          "environment": "dev",
          "version": "1.0.0",
          "gate_status": "allowed",
          "rollback_point": "snapshot:1.0.0",
          "changes": [{"sequence": 1, "category": "feature", "title": "a", "description": "..."}],
          "recorded_at": "2026-09-01T10:00:00Z"
        }
      ]
    }
  ],
  "segment_diffs": [
    {
      "from_environment": "dev",
      "to_environment": "test",
      "consistent": false,
      "categories": ["missing_changes", "version_divergence"],
      "version_divergences": [
        {"version": "0.9.0", "presence": "from", "only_in_from": true, "only_in_to": false},
        {"version": "1.0.1", "presence": "to", "only_in_from": false, "only_in_to": true}
      ],
      "gate_status_conflicts": [{"version": "1.0.0", "from_gate_status": "allowed", "to_gate_status": "blocked"}],
      "rollback_point_conflicts": [{"version": "1.0.0", "field": "rollback_point", "from": "snap:a", "to": "snap:b"}],
      "added_changes": [{"sequence": 2, "category": "feature", "title": "c", "description": "..."}],
      "missing_changes": [{"sequence": 3, "category": "fix", "title": "b", "description": "..."}],
      "inconsistent_changes": [
        {"left": {"sequence": 1, "category": "feature", "title": "a", "description": "old"},
         "right": {"sequence": 1, "category": "feature", "title": "a", "description": "new"}}
      ]
    }
  ],
  "consistent": false,
  "inconsistency_categories": ["gate_status_conflict", "missing_changes", "version_divergence"]
}
```

- 逐段差异方向固定为 `from_environment` → `to_environment`：`added_changes` 是下游新出现的条目，`missing_changes` 是下游缺失的上游条目，`inconsistent_changes` 是两侧都有但序号、分类或描述不一致的条目（按 `title` 识别）。各列表按稳定标识（`title`）排序，空结果为确定的空数组。
- `version_divergences` 逐版本列出只存在于一侧的版本；共有版本分别比较门禁状态（`gate_status_conflicts`）与回滚点（`rollback_point_conflicts`）。
- 相邻节点没有任何差异时，该段所有差异列表为空数组、`consistent` 为 `true`；全部相邻段一致时顶层 `consistent` 为 `true`、`inconsistency_categories` 为空数组。仅存在“新增变更”不会使链路不一致；出现缺失变更、不一致变更、版本分歧、门禁冲突或回滚点不一致时，顶层与对应段的 `consistent` 均为 `false`，`categories` / `inconsistency_categories` 分别给出确定的类别（`missing_changes`、`inconsistent_changes`、`version_divergence`、`gate_status_conflict`、`rollback_point_conflict`），不合并为笼统结果。
- 顺序可判定性：相邻节点之间必须能由发布事实确定先后。共有版本上，下游该版本最后一次发布必须晚于上游；无共有版本时，下游首个发布必须晚于上游最后一个发布。节点在该批次内没有任何发布事实，或事实证明顺序相反/重叠时，返回 409 `PROMOTION_CHAIN_ORDER_CONFLICT`，消息中逐段列出缺少顺序信息的节点（形如 `dev -> test`）。

#### `GET /api/v1/release-batches/{batch_id}/promotion-diff?from=<env>&to=<env>`

在任意两个环境节点之间生成局部晋级差异，计算口径与链路中的逐段差异完全一致（同样先做顺序可判定性检查）。返回 200：

```json
{
  "batch_id": "b1",
  "from_environment": "staging",
  "to_environment": "prod",
  "segment": { "from_environment": "staging", "to_environment": "prod", "consistent": true, "categories": [], "...": "与 chain 的 segment_diffs 元素相同" }
}
```

`from` 与 `to` 相同返回 400 `SAME_ENVIRONMENT_COMPARE`；缺少任一参数返回 400 `invalid_request`。

#### `GET /api/v1/release-batches/{batch_id}/changes/{change_id}/trace?environments=dev,test,staging,prod`

按变更条目标识（`title`）追溯它在链路中的传播：首次进入的节点、经过的全部节点，以及首次缺失的节点。`appearances` 按发布时间顺序列出该条目出现的每次发布（保留版本、门禁、回滚点、记录时间）。返回 200：

```json
{
  "batch_id": "b1",
  "change_id": "fix logout",
  "environment_order": ["dev", "test", "staging", "prod"],
  "first_present_environment": "dev",
  "first_missing_environment": "staging",
  "present_environments": ["dev", "test"],
  "appearances": [
    {"environment": "dev", "release_id": "rel_...", "version": "1.0.0",
     "recorded_at": "2026-09-01T10:00:00Z", "gate_status": "allowed", "rollback_point": "snap:1"}
  ]
}
```

- 条目在整条链路从未出现时不是错误：返回 200，`first_present_environment` 与 `first_missing_environment` 为 `null`，两个列表为空数组。
- 一个节点的任意一次发布包含该条目即视为“经过”该节点；`first_missing_environment` 取首次出现节点之后第一个不包含该条目的节点，之后再次出现不改变该结果。

#### 链路入口共同的错误约定

| 场景 | 状态码 | code |
|---|---|---|
| 批次不存在（三个入口相同的唯一 NotFound 观察） | 404 | `RELEASE_BATCH_NOT_FOUND` |
| `environments` 序列包含重复环境（消息列出冲突环境） | 400 | `DUPLICATE_ENVIRONMENT_IN_SEQUENCE` |
| 序列包含未登记环境（消息列出未知环境） | 400 | `UNKNOWN_ENVIRONMENT_IN_SEQUENCE` |
| 缺少或无法解析环境序列 | 400 | `invalid_request` |
| 发布事实不足以确定相邻节点先后（消息列出节点段） | 409 | `PROMOTION_CHAIN_ORDER_CONFLICT` |

校验顺序为：请求参数 → 重复/未知环境 → 批次存在性 → 顺序可判定性。局部差异入口的 `from`/`to` 相同使用既有的 `SAME_ENVIRONMENT_COMPARE`。这些查询不改变 `POST /api/v1/release-records`、单记录查询、`/api/v1/compare` 及全部基线入口的既有语义。
