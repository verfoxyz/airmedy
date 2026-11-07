# 修复：tx（QQ 音乐）搜索返回 0 行

> 状态：待开工
> 前置：`docs/specs/cloud-search-sources/spec.md` 已实现
> 发现方式：本地联调（2026-11-07 22:26），非人工图形化验证

---

## 1. 现象

新增的 5 个源里，`kw`、`mg` 正常，**`tx` 搜任何关键词都返回 0 行**，且 `has_more=true`。
用插件直接调用（`plugins/wy`，`node -e`）复现：

```
kw rows: 5 | covers: 5 | hasMore: true | first: "晴天"
tx rows: 0 | covers: 0 | hasMore: true | first: undefined      ← 问题
mg rows: 5 | covers: 5 | hasMore: true | first: "圣诞星（feat. 杨瑞代）"
```

## 2. 根因

`mapTXPage()` 从错误层级读列表：

```js
const data = body.req?.data;
const list = Array.isArray(data.song?.list) ? data.song.list : [];   // ← data.song 不存在
const total = Number(data.meta?.sum);                                // ← meta 不在 data 下
```

真实响应（2026-11-07 抓取，HTTP 200、`code=0`）：

```json
{ "code": 0,
  "req": { "code": 0,
           "data": { "body": { "song": { "list": [ …30 条… ] }, "album": …, "singer": … },
                     "meta": { "sum": 1000, "perpage": 30, … } } } }
```

即：列表在 `req.data.body.song.list`，总数在 `req.data.meta.sum`。
参考实现里 `body` / `meta` 是请求封装从 `req.data` 里拆出来的**别名**，不是 JSON 的真实字段名——照抄字段名就会读到 `undefined`。

现有测试用的是按错误路径伪造的 fixture，所以全绿。

## 3. 修法

`plugins/wy/src/search.js` 的 `mapTXPage()`：

```js
const data = body.req?.data;
if (!data || typeof data !== 'object') {
  throw new SearchError('search answered without data', KIND.UNAVAILABLE);
}
const page = data.body && typeof data.body === 'object' ? data.body : null;
const list = Array.isArray(page?.song?.list) ? page.song.list : [];
const total = Number(data.meta?.sum);
```

其余（`mapTXRow`、签名、翻页、失败分类）不动。

## 4. 测试

`plugins/wy/test/sources.test.js`（或 tx 对应用例）：

- **fixture 改成真实形状**：`{ code: 0, req: { code: 0, data: { body: { song: { list: [...] } }, meta: { sum } } } }`；
- 断言 `rows.length === limit`、标题/艺人/时长映射正确；
- 断言 `hasMore` 来自 `meta.sum`；
- 断言封面由 album mid 拼出；
- 失败用例保持：`req.code != 0` → `auth`；
- 回归：`cd plugins/wy && node --test`（当前 70 passed，修好后应 ≥70）。

联调验证（不依赖 GUI）：

```bash
cd plugins/wy && node -e "
const { createSearcher } = require('./src/search.js');
createSearcher({ timeoutMs: 15000 })({ source: 'tx', query: '周杰伦', limit: 5, offset: 0, include_cover: true })
  .then(r => console.log('rows', r.rows.length, 'covers', r.rows.filter(x => x.cover_url).length, 'hasMore', r.hasMore));
"
```

期望：`rows 5 covers 5 hasMore true`。

## 5. 验收

自动：§4 全绿。

人工：搜索页选 `tx` 单源 → 出 5 行、有封面；全选 5 源时 tx 结果并入合并列表。

## 6. 风险

- 这个错误不会抛异常（空列表被当成"源没有结果"），宿主不会报失败，界面上只表现为"tx 没结果"——排查时先看返回行数，别先怀疑签名。
- 其它源的响应路径已按真实抓包核对过（kw 正常、mg 正常），只有 tx 有这个问题。
