# Bug：已入库的曲目仍有一条「进行中」任务（重试永不成功）

> 状态：✅ 已完成（worker 按 identity 复解析 → 本地 title+artist 兜底 → done/canceled，不再重试；合并与删除按 `MediaRequestMatchKey` 收敛同曲其它 identity；任务页新增 `waiting` 状态（等待重试），达到 20 次显示失败；重试清零 attempts/next_attempt_at；启动扫描用 `ListOrphans` 忽略退避立即收敛 `track_id` 失效的 failed 请求；Go/Vue 测试覆盖）
> 现象：任务页一直有一条「新·与天齐 / 下载中…」
> 数据：请求 `kg/91a6ce…` 仍 `failed`、`attempts=5`、`next_attempt_at` 在 6 小时后
> 关联：`media-request-state-machine.md`、`fix-delete-resets-request.md`、`fix-stale-task-row.md`

---

## 1. 证据

```
media_requests:
  907ba194 | kg/91a6ce… | promote | failed
    error="track row for kg/91a6ce… no longer exists"
    attempts=5  next_attempt_at=2026-11-18T01:24:41

  37efc370 | tx/004KaogL1rk4jT | promote | done  (2026-11-16 20:59)
```

- 同曲（`新·与天齐` / `指尖笑`）已通过 tx 提升入库；
- kg 的 remote 行已被合并/删除，但 kg 请求仍存在；
- worker 每次执行都找不到 track 行 → 失败 → 指数退避（本次 6 小时后）；
- 任务页把「failed + 有 next_attempt_at」显示为 active，于是看起来永远在下载中。

## 2. 根因

1. 合并/提升只收敛了**当前 identity** 的请求，同曲其它 identity 的请求被遗留；
2. worker 找不到 track 行时直接判失败，没有按 identity / 本地命中做自愈；
3. 失败退避到 6 小时后，UI 仍展示为进行中，用户无法判断。

## 3. 修法

### 3.1 worker 自愈（核心）

执行前若 `track_id` 指向的行不存在：

1. 按 `provider + external_id` 再查一次（合并后行 id 可能已变）；
2. 仍没有 → 用统一 `match_key`（归一化 artist+title）查本地库：
   - 本地已有可播行 → 请求置 `done`（记录事件 `resolved: local row`）；
   - 没有本地行 → 置 `canceled`（原因 `track row removed`），不重试；
3. 永远不要因为“行不存在”反复重试。

### 3.1.1 启动扫描：失效行不等退避

`track_id` 已失效属于**不可恢复错误**，不能按退避时间挂着：

- worker 启动时的扫描，除“到期的 failed”外，还要包括
  **`track_id` 指向的行不存在的所有 failed 请求**（忽略 `next_attempt_at`）；
- 对它们立即按 §3.1 解析：有本地行 → `done`，否则 `canceled`；
- 目的：避免用户看到一条“等待重试”挂到几小时后才自动消失；
- 实现建议：`ListActive` 之外新增 `ListOrphans`（`state='failed' AND track_id NOT IN (SELECT id FROM tracks)`），
  或把该条件并入启动扫描查询。

### 3.2 合并/删除联动

- 合并（`track:merged`）或删除文件时：把**同一 match_key** 的其它活动请求一起收敛：
  - 已有本地行 → `done`；
  - 否则 `canceled`；
- 保留历史行（不物理删除），便于审计。

### 3.3 UI 语义

- `failed + next_attempt_at` → 显示「等待重试」而不是「下载中」；
- 达到最大尝试次数（20）→ 显示「失败」并进入出错页；
- 「重试」按钮清零 attempts 与 next_attempt_at。

## 4. 测试

**Go**

- `track_id` 失效、identity 能查到新行 → 请求 done；
- identity 查不到、match_key 有本地行 → done；
- 都查不到 → canceled，不再调度重试；
- 合并后同 match_key 的活动请求被收敛；
- 达到最大尝试次数后不再自动重试（显示失败）；
- 启动扫描会立即收敛 `track_id` 失效的 failed 请求（不等 `next_attempt_at`）。

**前端**

- 等待重试的行显示「等待重试」；
- 失败终态在「出错」，可手动重试。

**人工**

1. 复现：tx 已入库、kg 请求残留 → 启动后该行自动变「已完成」，不再进行中；
2. 任务页不再出现“永远下载中”的行；
3. 手动重试可用。

## 5. 验收

- 自动：§4 全绿；
- 人工：§5 三条成立；
- 同曲多 identity 不再留下永久进行中的请求。

## 6. 风险

- match_key 归一化不一致会漏收敛，必须与匹配缓存同一规则；
- 自动置 done 需要在事件里记录来源，便于排查；
- 收敛历史请求不应影响用户主动重新下载（显式 upsert 仍可重开）。
