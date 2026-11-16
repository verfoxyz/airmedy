# Bug：切歌导致缓存中断，任务页却显示「报错」

> 状态：✅ 已完成（新增 `canceled` 状态：`stopSession`/退出走 `Cancel`（error 为空），仅 `fail()` 保留 `failed`；迁移 000108 把历史 `failed/interrupted` 改为 `canceled`；任务页把缓存 `canceled` 归入「已完成」，请求的 `canceled` 仍归「出错」；清空记录包含 canceled。测试：Go + `frontend/src/stores/tasks.spec.ts`）
> 现象：「下载/任务」里缓存任务显示 `interrupted`，被归入「出错」
> 期望：切歌、暂停关闭等**主动中断**不算错误，不应出现在「出错」标签页
> 关联：`cache-task-history.md`

---

## 1. 根因

- 缓存会话在切歌/退出时被主动停止；
- 当前实现把所有未完成就被停止的行标记为 `failed`，`error='interrupted'`；
- 任务页按 `state==failed` 归入「出错」，用户看到红色报错。

主动中断 ≠ 失败：没有下载错误，只是用户切换了歌曲。

## 2. 修法

### 2.1 新增状态 `canceled`

`stream_cache_tasks.state` 增加 `canceled`：

| 状态 | 含义 | 任务页 |
|---|---|---|
| `active` | 下载中 | 进行中 |
| `done` | 完成 | 已完成 |
| `failed` | 真实失败（网络/解析/磁盘） | 出错 |
| `canceled` | 主动中断（切歌、退出、用户停止） | 已完成（或单列「已取消」） |

### 2.2 标记规则

- `stopSession`（切歌/Stop/Close）→ `canceled`；
- 应用退出时的活动行 → `canceled`（错误信息留空）；
- 只有 `fail()` 路径 → `failed` + 错误信息；
- 已经 `done` 的行不受影响。

### 2.3 任务页

- `canceled` 不计入「出错」计数与红色标记；
- 「已完成」标签包含 `done` 与 `canceled`（或单独显示灰色「已取消」，二选一，推荐并入已完成）；
- 「清空记录」同样删除 `canceled` 行。

### 2.4 保留诊断

- 行内可保留 `canceled_reason`（switch/exit/user），仅用于详情，不显示为错误。

## 3. 测试

**Go**

- 切歌 → 该缓存行 `state=canceled`、`error` 为空；
- 应用退出 → 活动行 `canceled`；
- 网络失败 → 仍为 `failed` 且带错误；
- `ListTasks` 分类：`canceled` 不进入出错列表；
- 清空记录删除 `canceled`。

**前端**

- 「出错」标签不显示切歌产生的缓存行；
- 「已完成」显示灰色「已取消」；
- 计数一致。

**人工**

1. 播放未缓存曲目 → 2 秒内切歌 → 任务页不出现红色报错；
2. 同一场景在「已完成」里能看到已取消；
3. 断网失败仍出现在「出错」。

## 4. 验收

- 主动中断不再显示为错误；
- 真实失败仍保留错误信息；
- 计数与标签一致。

## 5. 风险

- 历史数据里的 `failed(interrupted)` 行可在迁移中改为 `canceled`（`error='interrupted'`）；
- 单列「已取消」会多一个标签，若不想增加标签就并入已完成。
