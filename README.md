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

### `GET /release-comparisons?left=<env>&left_version=<v>&right=<env>&right_version=<v>`

跨环境发布差异比对：为两侧各指定一个目标环境和该环境上的一个版本（四个参数全部必填），系统按统一口径直接返回逐条差异。环境名与版本标识按既有发布记录的**原值精确识别**，不做大小写、空白或模糊匹配改写。两侧可以是同一个环境（用于同一环境内两个历史版本的比较）。

返回 HTTP 200：

```json
{
  "left_environment": "staging",
  "left_version": "1.0.0",
  "right_environment": "prod",
  "right_version": "1.0.0",
  "left_release":  { "id": 7, "environment": "staging", "version": "1.0.0", "changes": ["login", "beta"], "gate_status": "allowed", "rollback_point": "0.8.0", "registered_at": "..." },
  "right_release": { "id": 8, "environment": "prod",    "version": "1.0.0", "changes": ["login", "gamma"], "gate_status": "blocked", "rollback_point": "0.8.0", "registered_at": "..." },
  "version":       { "left": "1.0.0", "right": "1.0.0", "consistent": true },
  "gate_status":   { "left": "allowed", "right": "blocked", "consistent": false },
  "rollback_point": {
    "left":  { "identifier": "0.8.0", "target_release": { "...": "左侧回滚点指向的发布记录" } },
    "right": { "identifier": "0.8.0", "target_release": { "...": "右侧回滚点指向的发布记录" } },
    "consistent": true
  },
  "change_diffs": [
    {
      "change_id": "beta",
      "summary": "beta",
      "kind": "missing",
      "present_on_left": true,
      "present_on_right": false,
      "content_status": "absent",
      "left": "beta"
    },
    {
      "change_id": "gamma",
      "summary": "gamma",
      "kind": "added",
      "present_on_left": false,
      "present_on_right": true,
      "content_status": "absent",
      "right": "gamma"
    }
  ],
  "consistent": false
}
```

约定：

- 变更以其稳定标识（记录里的变更条目原值）识别，方向固定为 left → right：`kind` 为 `added`（右侧有、左侧无）、`missing`（左侧有、右侧无）或 `changed`（两侧都有但内容不同）。每条都给出 `change_id`、`summary`、`present_on_left`/`present_on_right` 两侧存在状态与 `content_status`（`same`/`different`/`absent`）。
- 变更条目按 `change_id` 字典序稳定输出；内容完全相同时 `change_diffs` 为确定的空数组 `[]`，此时各标量结论也一致则顶层 `consistent` 为 `true`。
- `version`、`gate_status`、`rollback_point` 各自给出两侧记录值与 `consistent` 结论。门禁状态按**发布时记录的原值**比较，不根据当前状态重新推断。
- 回滚点同时比较标识与所指向的发布对象：回滚点标识是该环境上被指向的历史发布版本（与 `GET /environments/{environment}/history` 的回溯口径一致，即同环境下版本等于回滚点标识的发布）；两侧 `target_release` 完整回溯到原始发布记录。只有标识相同且目标发布的门禁状态、变更集合、目标自身回滚点标识也全部相同时 `rollback_point.consistent` 才为 `true`（比较不递归）。
- 响应不含服务端生成时刻；相同输入连续查询时字段、条目顺序和结论逐字节一致。查询不写任何数据。

错误码（错误体仍只有 `error` 一个顶层键）：

| 场景 | 状态码 | code |
|---|---|---|
| 缺少四个必填参数中的任何一个 | 400 | `invalid_request` |
| 请求的环境在既有发布记录中不存在（按固定 left、right 顺序先校验环境，再校验版本） | 404 | `ReleaseComparisonEnvironmentNotFound` |
| 指定版本在该环境的发布记录中不存在（精确值匹配，`1.2` 不会匹配 `1.2.0`） | 404 | `ReleaseComparisonVersionNotFound` |
| 发布（或其回滚点指向的目标发布）缺少变更、门禁状态或回滚点等比较必需数据，或回滚点不指向任何已记录发布 | 422 | `ReleaseComparisonDataIncomplete` |

### `GET /environments/{environment}/release-history[?limit=<n>&cursor=<token>]`

按环境查询发布时间由近到远（`registered_at` 倒序，同一时刻按写入顺序倒序）的发布历史。每条完整保留版本、变更条目、门禁状态和回滚点，并带 `id`，可继续通过 `GET /releases/{environment}/{version}` 回溯到该次发布。

返回 HTTP 200：

```json
{
  "environment": "prod",
  "limit": 20,
  "next_cursor": "N...",
  "releases": [ { "...": "与 POST /releases 相同的发布记录形状" } ]
}
```

- `limit` 可选，默认 20，最大 100；非法（非正整数、超过上限或非数字）返回 HTTP 400 `invalid_request`。
- 翻页使用上一页响应中的不透明 `cursor`（URL-safe，调用方只原样回传）；分页基于 `(发布时间, 登记 id)` 的 keyset 定位，翻页期间即使有更新的发布写入，也不会重复或遗漏任何已经记录的发布；没有下一页时 `next_cursor` 省略。
- 环境不存在返回 HTTP 404 `environment_not_found`；游标无法解码返回 HTTP 400 `invalid_request`。
- 该入口为只读查询，不改变 `GET /environments/{environment}/history`（从旧到新、无分页）的既有行为。

### 旧记录的读取约定

本次新增字段（`changes`、`gate_status`、`rollback_point`）只对新登记和差异比较生效。早于这些字段写入的记录保持原值可读：输出中跳过这三个字段，不判为无效，也不补写。

当上述新的比对/历史入口遇到这种缺少 `changes`、`gate_status` 或 `rollback_point` 的旧记录，或回滚点不指向任何已记录发布时，返回确定的 HTTP 422 `ReleaseComparisonDataIncomplete`，而不是把缺失折叠成空结果。

## 错误返回约定

所有错误响应使用同一个 JSON 形状，并且只包含 `error` 一个顶层键：

```json
{"error":{"code":"<机器可读的短标识>","message":"<一句话说明>"}}
```

`code` 使用小写下划线形式，`message` 不包含堆栈、文件路径或 SQL。

当前使用的 `code`：`invalid_request`、`not_found`、`conflict`、`storage_unavailable`、`invalid_release_input`、`release_not_found`、`environment_not_found`、`release_conflict`、`comparison_conflict`、`ReleaseComparisonEnvironmentNotFound`、`ReleaseComparisonVersionNotFound`、`ReleaseComparisonDataIncomplete`。

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

- `environment`、`version`、`changes`、`gate_status`、`rollback_point` 均为必填；`changes` 为数组（允许空数组）。`batch_id` 为可选：非空白字符串，最长 200 个字符，用于发布晋级链路（见下文）；空白但非省略时按校验失败处理。
- 每个变更条目的 `category`、`title`、`description` 为必填非空字符串；`sequence` 为可选正整数。要么所有条目都带 `sequence`（且互不相同），要么全部省略——全省略时服务端按数组顺序从 1 编号。
- `gate_status` 只接受 `allowed`、`blocked`、`pending`。
- `rollback_point` 是公开入口中的稳定标识，服务端只原样保存，不推断、不读取或生成任何文件内容。
- 成功返回 201 与 `{"release_record": {...}}`，记录包含：
  - `id`：稳定记录标识，形如 `rel_` 加 32 个十六进制字符（不暴露数据库主键）；
  - `recorded_at`：服务端生成的记录时间，UTC，`YYYY-MM-DDTHH:MM:SSZ`；
  - 完整的 `environment`、`version`、`changes`、`gate_status`、`rollback_point`；写入时提供了 `batch_id` 时还包含该字段。
- 写入成功后立即可由下面的查询入口与差异比对读取。

错误码（错误体仍只有 `error` 一个顶层键）：

| 场景 | 状态码 | code |
|---|---|---|
| 目标环境未登记 | 404 | `ENVIRONMENT_NOT_FOUND` |
| 同一环境同一版本重复提交 | 409 | `RELEASE_ALREADY_EXISTS` |
| 必填字段缺失、类型不符、`sequence` 非法或重复、门禁枚举非法、回滚点为空白、`batch_id` 为空白或超长 | 422 | `RELEASE_VALIDATION_FAILED` |
| 请求体不是合法 JSON | 400 | `invalid_request` |

### `GET /api/v1/release-records`

查询参数全部可选，按 AND 组合：

| 参数 | 含义 |
|---|---|
| `environment` | 目标环境；未登记时 404 `ENVIRONMENT_NOT_FOUND` |
| `version` | 精确版本 |
| `gate_status` | 门禁状态；非枚举值返回 422 `RELEASE_VALIDATION_FAILED` |
| `recorded_from` / `recorded_to` | 记录时间闭区间，支持 RFC3339 时刻或 `YYYY-MM-DD` 日期（日期分别取当日 00:00:00 / 23:59:59 UTC）；无法解析返回 422 `RELEASE_VALIDATION_FAILED` |
| `batch_id` | 精确匹配发布批次标识（仅写入时携带该字段的记录） |

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

## 发布批次与晋级链路 API（/api/v1）

发布记录可以携带可选的 `batch_id`，用于把同一发布批次在多个环境中的发布事实关联成一条晋级链路。它只是发布记录上的稳定标识：不改变写入、单记录查询和环境差异比对的既有语义。

### 发布记录上的批次标识

- `POST /api/v1/release-records` 的请求体新增**可选**字段 `batch_id`：非空白字符串，最长 200 个字符。省略或为空字符串（不提供该字段）时记录为无批次记录，响应形状与以前完全一致（不出现 `batch_id` 键）。
- 无批次记录仍要求同一 `environment` + `version` 唯一；带批次记录按 `(environment, version, batch_id)` 唯一：**不同批次复用同一环境同一版本互不冲突**，但同一批次在同一环境重复提交同一版本仍返回 409 `RELEASE_ALREADY_EXISTS`；带批次与无批次记录占用同一个 `(environment, version)` 时也返回 409，二者不能并存。
- 成功响应中的记录在带批次时额外包含 `"batch_id": "..."`；`GET /api/v1/release-records` 新增可选过滤参数 `batch_id`（精确匹配，与其它过滤条件 AND 组合）。
- 批次隔离是硬边界：链路、差异与追溯只读取同一 `batch_id` 的发布事实，多个批次复用同一版本也不会被合并成别的链路。

### `GET /api/v1/release-batches/{batch_id}/promotion-chain`

查询参数 `environments` 为逗号分隔的**有序**环境序列，例如 `dev,test,staging,prod`。返回该批次按晋级顺序排列的完整链路：

```json
{
  "batch_id": "b-2026-10-01",
  "environments": ["dev", "test", "staging", "prod"],
  "nodes": [
    {
      "environment": "dev",
      "releases": [
        {"id": "rel_…", "environment": "dev", "version": "1.0.0",
         "gate_status": "allowed", "rollback_point": "snap:0.9.0",
         "changes": [{"sequence": 1, "category": "feature", "title": "alpha", "description": "…"}],
         "recorded_at": "2026-10-01T08:00:00Z"}
      ],
      "effective_release": { "...": "该节点登记顺序最后的发布事实" }
    }
  ],
  "segment_diffs": [
    {
      "from_environment": "dev",
      "to_environment": "test",
      "added_changes": [ { "…变更条目…": "下游有、上游无（按 title 排序）" } ],
      "missing_changes": [ { "…变更条目…": "上游有、下游无（按 title 排序）" } ],
      "inconsistent_changes": [
        {"left": {"…变更条目…": "上游"}, "right": {"…变更条目…": "下游（title 相同但 sequence/category/description 不同）"}}
      ],
      "version": {"left": "1.0.0", "right": "1.0.1", "changed": true},
      "gate_status": {"left": "allowed", "right": "blocked", "changed": true},
      "rollback_point": {"left": "snap:0.9.0", "right": "snap:0.8.0", "changed": true}
    }
  ],
  "consistent": false
}
```

约定：

- `nodes` 顺序就是请求的环境顺序；同一节点在批次内有多次发布时，`releases` 按发布时间从旧到新**完整列出**，绝不只保留最后一次；同一秒写入的多次发布按写入顺序稳定排列。
- `effective_release` 是该节点登记顺序最后的发布事实，逐段差异基于相邻节点的有效事实计算。
- `segment_diffs` 只包含存在确定差异的相邻段，按链路顺序排列；无任何差异时为确定的空数组 `[]`，此时 `consistent` 为 `true`。只要任一段存在变更条目的新增、缺失、不一致，或版本分歧、门禁事实冲突、回滚点不一致，`consistent` 即为 `false`，并分别给出确定的差异类别，不合并成笼统结果。
- 变更条目以稳定标识 `title` 识别（与既有环境差异比对一致）；差异中的变更列表按 `title` 字典序排列；节点发布事实中的变更保留记录里的原始顺序。
- 时间（`recorded_at`）与版本字符串沿用服务既有格式；响应不含服务端生成时刻，相同输入重复查询结果逐字节一致。
- 查询不写任何数据，不改写历史记录。

### `GET /api/v1/release-batches/{batch_id}/promotion-diff?from=<env>&to=<env>`

从任意两个环境节点生成**局部晋级差异**（不要求相邻）。响应：

```json
{
  "batch_id": "b-2026-10-01",
  "from_environment": "dev",
  "to_environment": "staging",
  "from_release": { "…发布事实…": "from 节点的有效发布" },
  "to_release": { "…发布事实…": "to 节点的有效发布" },
  "diff": { "…与链路中 segment_diffs 完全相同的逐段差异形状…": "" },
  "consistent": false
}
```

`diff` 内各类别字段始终存在（无差异时为空数组或 `"changed": false`），`consistent` 语义与链路查询一致。

### `GET /api/v1/release-batches/{batch_id}/changes/{title}/trace?environments=dev,test,staging,prod`

按变更条目稳定标识 `title` 追溯它在链路中的走向：

```json
{
  "batch_id": "b-2026-10-01",
  "change_title": "beta",
  "first_environment": "dev",
  "first_entered_at": "2026-10-01T08:00:00Z",
  "environments": ["dev", "test", "staging", "prod"],
  "passed_environments": ["dev", "test"],
  "first_missing_environment": "staging"
}
```

- `first_environment` 是序列中首个有效发布包含该条目的节点；`first_entered_at` 是该节点最早一次包含该条目的发布时间。
- `passed_environments` 从首次进入节点起、沿晋级方向连续包含该条目的节点；`first_missing_environment` 是其后首个不再包含该条目的节点；一直存在到序列末尾时为空字符串 `""`。
- 该条目在批次的所有请求节点中都不存在时返回 404 `PROMOTION_CHANGE_NOT_FOUND`。

### 晋级链路错误码

三个查询入口共用统一的校验顺序与错误形状（仍只有 `error` 一个顶层键）：

| 场景 | 状态码 | code |
|---|---|---|
| 批次在所有发布记录中都不存在（唯一可观察的批次 NotFound） | 404 | `RELEASE_BATCH_NOT_FOUND` |
| `environments` 缺失、为空、含空段或含重复环境（消息中指出冲突环境） | 400 | `INVALID_PROMOTION_SEQUENCE` |
| 序列包含未登记环境（消息中指出未知环境） | 400 | `INVALID_PROMOTION_SEQUENCE` |
| 请求节点在该批次内没有发布事实，无法确定先后顺序（消息中指出缺顺序信息的节点） | 409 | `PROMOTION_ORDER_CONFLICT` |
| 局部差异缺少 `from`/`to` | 400 | `INVALID_PROMOTION_SEQUENCE` |
| 局部差异 `from` 与 `to` 相同 | 400 | `SAME_PROMOTION_NODE` |
| 追溯的变更条目不属于该批次 | 404 | `PROMOTION_CHANGE_NOT_FOUND` |

单元素环境序列是合法的：链路返回一个节点、空 `segment_diffs` 与 `consistent: true`。序列校验先于批次存在性检查，因此未知批次配重复/未知环境时仍返回 400；批次存在性先于节点顺序检查。
