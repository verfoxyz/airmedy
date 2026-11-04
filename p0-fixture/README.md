# P0 夹具：lx 自定义源兼容层验收套件

> 目的：在动 Airmedy 一行代码之前，先把「lx 自定义源 → Airmedy」这条链路要遵守的
> **契约**钉死，并给出一份可执行的**参考实现（golden reference）**。
>
> 将来 Airmedy 的 Go + WebView 宿主，必须跑通**同一张场景表**。两边同时全绿，才算兼容层达标。

---

## 1. 为什么需要它

用户提供的真实音源 [`../ikun-music-source.js`](../ikun-music-source.js) 暴露了几个关键事实：

1. 只声明 `['musicUrl']`，**没有 `local`** → 本地库直接调它必然失败
2. 强依赖平台 ID（`musicInfo.hash ?? musicInfo.songmid`）
3. 声明了官方不认识的 `git` 源和 `hires/atmos/master` 音质 → **说明生态里有魔改客户端**
4. 服务端化：真正的解析在 `https://c.wwwweb.top`，脚本只是 API key 的壳
5. 一堆容易被忽略的细节：对象 body、`follow_max`、`updateAlert`、`429` 语义、`console.groupEnd()`

这些细节只要漏一个，用户看到的就是"装上了但一直失败"。所以先用夹具锁住它们。

---

## 2. 文件清单

| 文件 | 作用 |
|---|---|
| `lx-runtime.js` | **参考实现**：`globalThis.lx` 的 request / on / send / utils / 校验 / 源过滤 |
| `lx-source-fixture.js` | 与真实音源**同构**的测试脚本（指向本地 mock，含超纲 source/音质） |
| `mock-server.js` | 零依赖 mock API：`/music/url`、`/script/lxmusic`、`/audio/*`、场景切换 |
| `example-source.js` | 一份「与 ikun 同构」的示例自定义源；用户导入的 `.js` 就是这个形态 |
| `harness.js` | 26 条验收场景，`node harness.js` 一条命令出结果 |
| `shim-selftest.js` | `lx.utils` 的**已知向量**测试（md5/AES/RSA/zlib/buffer） |

---

## 3. 跑起来

```bash
cd p0-fixture

node shim-selftest.js   # 10/10 通过 —— 验证 crypto/zlib 语义
node harness.js         # 26/26 通过 —— 验证完整的源协议
node harness.js --verbose   # 附带 mock 侧收到的请求记录
```

想手动折腾：

```bash
node mock-server.js 18787
curl "http://127.0.0.1:18787/__scenario?set=busy"   # 切换场景
curl -X POST http://127.0.0.1:18787/music/url \
     -H 'X-Api-Key: local-test-key' -H 'Content-Type: application/json' \
     -d '{"source":"wy","musicId":"123","quality":"320k"}'
```

场景：`ok | deny(403) | busy(429) | fail(500) | slow | redirect | badurl | longurl | html | http500 | noapikey`

---

## 4. 验收表（Airmedy 侧要一一对应）

| # | 场景 | 关键点 | Airmedy 侧对应 |
|---|---|---|---|
| A1 | 宽容模式保留 `git` 源 | 非标准 source 不能被丢 | `filterSources(tolerant=true)` |
| A2 | 保留 `hires/atmos/master` | 超纲音质不能裁 | 同上 |
| A3 | 严格模式丢弃 `git`、裁到 4 档 | 与官方 lx 对齐（可选开关） | `filterSources(tolerant=false)` |
| A4 | 不声明 `local` 时不得报错 | 源缺失是正常情况 | 空源表要能正常渲染 |
| B1 | `checkUpdate` 异步触发 `updateAlert` | 加载完成 ≠ 初始化完成 | 事件桥 + 一次性语义 |
| B2 | `updateAlert` 只能一次 | 第二次必须 reject | 宿主计数 |
| C1 | 返回 http URL | 正常路径 | `resolve()` |
| C2 | 请求头/体符合约定 | `X-Api-Key`、`User-Agent: lx-music-desktop/2.0.0`、JSON body | 宿主注入 env/version |
| C3 | `hash` 优先、`songmid` 回退 | 真实脚本的 ID 选择 | 插件输出必须带 `source`+`id` |
| C4 | 对象 body 自动 JSON 化 | 不能出现 `[object Object]` | request 实现 |
| C5 | `follow_max` 跟随 302 | needle 语义 | 重定向上限 |
| D1 | 403 → 鉴权失败 | 不可重试 | 错误分类 |
| D2 | 429 → 请求过速 | **可重试**，要退避 | 重试策略 |
| D3 | 500 → 透出 message | 可重试 | — |
| D4 | HTTP 500 仍按业务 code | 不能只看 HTTP status | — |
| D5 | 非 JSON 响应 | 不能崩，报 unknow error | — |
| D6 | 不支持的动作 → reject | `action not support` | 能力声明校验 |
| E1 | 非 http(s) URL 被拒 | 宿主兜底 | `validateResult` |
| E2 | URL > 2048 被拒 | 同上 | — |
| F1 | 请求超时（默认值） | 脚本不传 timeout 也要有上限 | 20s 默认 |
| F2 | 外层超时 | handler 卡死也要能收口 | resolve 超时 |
| F3 | `cancel()` 中断在途请求 | 切歌要能取消 | AbortController |
| F4 | 未注册 handler 的明确报错 | 便于排查 | — |

---

## 5. 与真实音源的差异（有意为之）

| 项 | 真实 `ikun-music-source.js` | 本夹具 |
|---|---|---|
| API 地址 | `https://c.wwwweb.top`（付费、限速） | 本地 mock，可编排各种错误 |
| API key | 真实密钥 | `local-test-key` |
| 版本号/注释头 | `v26` | `v0` |
| 其余结构 | — | **刻意保持一致**（对象 body、`follow_max`、错误码、`console.groupEnd`） |

⚠️ 夹具里的 key 是假的，**不要**拿去打真实接口。

---

## 6. 参考实现的已知限制（Airmedy 实现时需要补齐）

| 限制 | 说明 | 补齐建议 |
|---|---|---|
| `formData` 未实现 | 直接返回 `ENOTSUP` 错误 | Go 侧用 `mime/multipart` |
| 无 cookie jar | 每次请求独立 | 宿主统一 cookie jar（供登录态使用） |
| 无代理支持 | 直连 | 复用 Airmedy 新增的网络代理设置 |
| 无证书/UA 策略 | 用 Node 默认 | 跟系统一致 |
| 无域名白名单 | 任意目标可达 | 端口层加 allowlist（见方案文档 §9） |
| `follow_max` 不计入跨协议降级 | http→https 等按标准库处理 | — |

---

## 7. 下一步

1. **把这 26 条搬进 Go 测试**（`internal/infra/pluginhost` 的 `_test.go`），用 `httptest` 起同样的 mock。
2. Go 宿主实现后，跑同一张表；两边都绿 → 兼容层可以对外宣布。
3. 再补一个「Airmedy 端到端」夹具：`resolve → 下载到临时目录 → ImportFile → playlist.AddTrack`，
   验证"搜到 → 换 URL → 落盘 → 入库 → 进歌单"的完整闭环。
