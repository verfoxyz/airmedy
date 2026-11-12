# 任务：Last.fm 登录改为本地回环回调

> 状态：✅ 已完成（代码已合入 wip/stream-playback）
> 背景：Last.fm 应用表单不接受 `airmedy://auth`（自定义 scheme 被判为无效 URL），已注册回调 `http://127.0.0.1:53682/auth`
> 范围：只改 Last.fm 登录流程；不改 scrobble/推荐

---

## 1. 目标

用**本地 HTTP 监听**接收 Last.fm 授权回调，替代当前的深链接方案：

1. `Connect()` 时在 `127.0.0.1:53682` 启动临时 HTTP 监听；
2. 打开 `https://www.last.fm/api/auth/?api_key=...&cb=http://127.0.0.1:53682/auth`；
3. 用户在浏览器授权后，Last.fm 重定向到回调，`/auth` 收到 `token`；
4. 用 token 调 `auth.getSession`，保存用户名/session（现有逻辑不变）；
5. 关闭监听、返回成功。

## 2. 约束

- **端口固定为 `53682`**：Last.fm 后台注册的回调是固定字符串，端口不能随机；
- 监听只绑定 `127.0.0.1`（不要 `0.0.0.0`）；
- 只接受 `GET /auth?token=...`，其它路径返回 404；
- 收到 token 后立即响应一个简单 HTML（“授权完成，可以关闭此页面”），然后关闭服务；
- 超时（默认 5 分钟）未收到回调 → 关闭监听并返回错误；
- `Connect()` 被取消/重复调用要能安全退出（关闭监听）。

## 3. 代码改动

### 3.1 `internal/app/lastfm/service.go`

- 新增常量：

```go
const lastFmCallbackPort = 53682
const lastFmCallbackPath = "/auth"
```

- `Connect(ctx)`：

```go
ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", lastFmCallbackPort))
// 端口被占用：返回可读错误（"端口 53682 被占用，Last.fm 登录需要它"）
```

- 启动 `http.Server{Handler: mux}`，`mux` 只处理 `lastFmCallbackPath`；
- token 通过 channel 传回 `Connect`；成功/失败都 `shutdown`；
- 使用 `http://127.0.0.1:53682/auth` 作为 `cb` 参数（与已注册地址完全一致）。
- 删除/保留深链接分支？**保留** `airmedy://` 处理（main.go 通用逻辑不动），但 Last.fm 流程不再依赖它；`Connect` 里不再使用 `airmedy://auth`。

### 3.2 错误处理

| 情况 | 行为 |
|---|---|
| 端口被占用 | 返回明确错误，不打开浏览器 |
| 浏览器打开失败 | 关闭监听、返回错误 |
| 用户取消/超时 | 关闭监听、返回 `context deadline exceeded` 类错误 |
| Last.fm 回调带 error 参数 | 关闭监听、把错误码转成用户可读信息 |
| 回调 token 为空 | 视为失败，继续等待或超时 |

### 3.3 安全

- 只监听回环地址；
- 不接受来自非 GET 的请求；
- 不把 api_secret 写进任何响应；
- 回调响应不包含 token。

## 4. 测试

**Go（`httptest` 可覆盖 handler，端口用注入值便于测试）**

- 把端口/路径抽成可注入参数，测试里用随机端口；
- 正常：请求 `/auth?token=abc` → channel 收到 token，响应 200；
- 错误路径、错误 method、未知路径 → 404 / 不产生 token；
- 超时：不回调 → 5 秒（测试用短超时）后返回错误且端口释放；
- 端口占用：先占住端口，`Connect` 返回可读错误；
- 取消：`ctx` 取消后监听关闭。

**人工**

1. 设置 → 集成 → 连接 Last.fm → 浏览器授权 → 回到应用显示已连接；
2. 断开后重连正常；
3. 端口被占用时提示明确；
4. 连接后推荐页不再报“请求失败”（前提是 secrets 里填了有效 key）。

## 5. 验收

- 自动：§4 全绿；
- 人工：§4 四条成立；
- 不再依赖 `airmedy://` 深链接完成 Last.fm 登录。

## 6. 风险

- 端口固定：若被其它程序占用，登录失败（给出明确提示）；
- Last.fm 要求回调 URL 与注册值完全一致（含端口与路径），改端口必须同步改后台；
- 浏览器可能提示“打开本地应用”之类的安全提示，属正常；
- 旧的 `airmedy://auth` 深链接仍可能在系统注册，保留无害。
