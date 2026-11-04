# PoC：搜索 → ID → 自定义源 → 直链 → 下载

> 结论先行：**链路成立并已实测跑通**（真实下载 10.33 MB / 320 kbps / 4:31，ffprobe 验证通过）。
> VIP 曲目按预期明确失败，错误被如实透出。

---

## 1. 它验证了什么

```
① 公开搜索（无 cookie）        music.163.com/api/search/get/web   → songmid + fee
② 自定义源协议（lx musicUrl）  globalThis.lx.request 走 lx-runtime → 直链
③ 下载                        http(s) 流式落盘 .part → 校验 → rename
④ 音频校验                    magic bytes + ftpobe（外部验证）
```

关键点：**②走的是真正的 lx 自定义源协议**（`p0-fixture/lx-runtime.js` 是它的参考实现），
不是普通函数调用。所以这一步跑通，意味着 Airmedy 侧的 Go/WebView 宿主照同一套语义实现即可。

---

## 2. 实测结果

### 正例（免费曲）

```
✓ 公开搜索（无 cookie）   "周杰伦 晴天" → 10 条
✓ 选择曲目              2668397359 · 晴天 (原唱 周杰伦) · fee=8
✓ 自定义源解析 musicUrl  http://m801.music.126.net/2026.../xxx.mp3
✓ 下载完成              10.33 MB · audio/mpeg · sha1 8ea9973bdb19…
✓ 音频校验              识别为 mp3，文件头 49443304 ("ID3")
✓ 文件                  ffprobe: mp3 / 270.74s / 320 kbps
```

### 负例（VIP 曲）

```
✓ 公开搜索（无 cookie）   → 10 条
✓ 指定曲目              id=509781655
⚠ 自定义源解析          预期内失败：无可用直链（需要会员或受版权限制）
```
→ 退出码 0，链路正常，错误语义正确。**这正是宿主该有的行为**：失败要明确，不能静默返回坏数据。

---

## 3. 怎么跑

```bash
cd poc
node e2e.js                        # 默认：搜"周杰伦 晴天"，取第一首免费曲
node e2e.js --query="陈奕迅 十年"
node e2e.js --id=509781655         # 指定 VIP 曲，验证失败路径
node e2e.js --out=./tmp            # 自定义输出目录
```

输出目录默认 `poc/out/`（已 gitignore），**测试音频不会入库**。

---

## 4. 踩到的坑（Airmedy 实现时要照抄）

| 坑 | 说明 |
|---|---|
| **直链常是 `http://`** | 第一次就栽在这：只支持 https 会直接报 `Protocol "http:" not supported`。下载器必须双协议。 |
| **必须带 `Referer: https://music.163.com/`** | 少了会 403。UA 同理。 |
| **`Content-Length` 可能缺失** | 要按实际字节数校验，不能只信响应头。 |
| **VIP 返回 `code=-110` + `url=null`** | 不是 HTTP 错误，是业务码；必须单独识别并给出可读提示。 |
| **搜索结果是 `fee` 决定能否播** | `fee=1` VIP / `fee=8` 免费低音质 / `fee=0` 免费。UI 上应提前标"VIP"。 |
| **QQ 侧会限流**（`code 2001`） | 需节流 + 重试；网易云实测连续 5 次无异常。 |

---

## 5. 已知限制（下一步要补的）

1. **只有免费曲**：VIP 需要会员 cookie 或自定义源后端。
2. **下载器是最小版**：无断点续传、无并发分块、无重试退避、无限速。
3. **没有写标签**：Airmedy 侧用 taglib 写 title/artist/album/cover（现成能力）。
4. **没有入库**：`ImportFile` 在 Go 侧，本 PoC 只到文件落盘。
5. **没有接 `download_jobs`**：S3 方案的表结构还没建。

---

## 6. 这证明了什么、没证明什么

**证明了**：公开搜索拿 ID → 自定义源换直链 → 下载可播放文件，这条链路在真实网络上成立。

**没证明**：VIP 曲能拿到直链（需要会员账号或第三方音源）；也不能证明所有 lx 脚本都能用
（如 `ikun` 那份需要它自己的付费后端）。

> 合规说明：本 PoC 只用公开接口、不登录、不携带任何账号 cookie，仅作技术验证。
