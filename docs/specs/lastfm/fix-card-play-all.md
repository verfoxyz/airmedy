# Bug：Last.fm 歌单「播放全部」没有反应（单首可播）

> 状态：待开工
> 现象：Last.fm 页「为你推荐」卡片点「播放全部」没有任何效果；单首播放正常
> 页面：`LastFmView.vue` 卡片、`LastFmListView.vue` 详情页

---

## 1. 根因

两处 `playAll` 都是串行解析、没有逐行容错：

```ts
for (const row of items) {
  if (!isPlayableLastFmRow(row)) continue          // 未匹配/无源的行被静默跳过
  const id = await catalogStore.preparePlayback(...) // 任何一行抛错 → 整个循环中断
  if (id) ids.push(id)
}
if (ids.length === 0) return                        // 静默返回，没有任何提示
```

因此：

1. 中间一行解析失败（mg/kg 无源、身份失效、插件报错、被删除的 remote 行）→ 异常跳到外层 catch，**全部失败**；
2. 列表还在匹配中（`matching=true`）→ 所有行被跳过 → `ids` 为空 → 静默无反应；
3. 单首播放不经过这段循环，所以正常。

## 2. 修法

### 2.1 逐行容错

```ts
const ids: string[] = []
let skipped = 0
for (const row of playableRows) {
  try {
    const id = await catalogStore.preparePlayback(lastFmRowToCatalogRow(row))
    if (id) ids.push(id)
    else skipped++
  } catch (e) {
    skipped++
    console.warn('lastfm: skip a row that could not be prepared', row.match_key, e)
  }
}
```

- 单行失败不再中断整批；
- 记录跳过数量，播完/无结果时提示。

### 2.2 空结果与部分结果

- `ids.length === 0`：
  - 若列表仍在匹配（存在 `matching=true` 行）→ 显示「正在匹配，请稍后」；
  - 否则显示「没有可播放的曲目」；
- 有部分结果：正常播放，并提示「已跳过 N 首不可播放的曲目」。

### 2.3 匹配中的播放

- 卡片操作按钮在「全部行都还在匹配」时禁用（`title` 提示“匹配中”）；
- 或允许播放已匹配子集，但要有上面 2.2 的提示，不能静默。

### 2.4 解析顺序与节流

- 保持串行（简单、不放大源请求）；
- 若行数多（50），串行可能较慢：解析期间按钮显示 loading，禁止重复点击；
- 解析/播放失败已有 `playError` 提示通道，复用。

## 3. 测试

**前端**

- 中间一行 `preparePlayback` reject → 其余行仍被解析并播放；
- 全部行 `matching=true` → 不发播放请求并显示“匹配中”提示；
- 全部行不可播放 → 显示“无可用曲目”；
- 部分可播 → 播放可播的部分，并显示跳过数量；
- 详情页 `playAll` 与卡片行为一致；
- 双击/重复点击在解析中不会重复发起。

**人工**

1. 「为你推荐」匹配完成后点播放全部 → 正常开始播放；
2. 有不可播行时 → 其余照播，提示跳过数量；
3. 刚刷新（全部匹配中）时点播放全部 → 提示“匹配中”，不静默；
4. 单首播放行为不变。

## 4. 验收

- 自动：§3 全绿；
- 人工：§4 四条成立。

## 5. 风险

- 逐行容错后可能有“部分播放”，需要提示清楚，避免用户以为全部成功；
- 串行解析 50 行较慢，若体验差再改并用并发上限 3（不要无限并发）。
