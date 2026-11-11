# Spec：宿主网络代理（不使用 / 系统代理 / 自定义按域名）

> 状态：待开工
> 范围：**只覆盖宿主进程的出站 HTTP 请求**；插件（子进程）的网络不在此列，见 §7
> 前置：无（独立于流播放）

---

## 1. 目标

设置里提供三档代理模式：

| 模式 | 行为 |
|---|---|
| `off`（默认） | 全部直连 |
| `system` | 读取操作系统代理设置 |
| `custom` | 按规则表：域名 / IP / CIDR → 指定代理，未命中直连 |

自定义规则支持 `http://`、`https://`、`socks5://` 代理地址（可带用户名密码）。

## 2. 非目标

- PAC 脚本（`*.pac`）解析；
- 插件/子进程的代理（§7）；
- 按进程、按请求头的规则；
- 代理链路（多级代理）。

---

## 3. 设置与存储

迁移 `000094` 给 `app_settings` 增加：

```sql
ALTER TABLE app_settings ADD COLUMN proxy_mode TEXT NOT NULL DEFAULT 'off';
ALTER TABLE app_settings ADD COLUMN proxy_rules TEXT NOT NULL DEFAULT '[]';
```

- `proxy_mode` ∈ `off|system|custom`；
- `proxy_rules` 是 JSON 数组，按顺序匹配，**第一条命中生效**：

```json
[
  { "match": "api.deezer.com",        "proxy": "http://127.0.0.1:7890" },
  { "match": "*.last.fm",             "proxy": "socks5://127.0.0.1:1080" },
  { "match": "10.0.0.0/8",            "proxy": "http://127.0.0.1:8080" },
  { "match": "203.0.113.7",           "proxy": "http://user:pass@127.0.0.1:8080" }
]
```

规则语法：

- 精确主机名：`api.deezer.com`；
- 通配子域：`*.deezer.com`（不含根域本身）；
- IPv4/IPv6 字面量、IPv4/IPv6 CIDR；
- 不支持端口/路径匹配；匹配对象是 `req.URL.Hostname()`。

默认：

- 无规则命中 → 直连；
- `localhost` / `127.0.0.0/8` / `::1` 永远直连；
- 规则非法（解析失败）→ 跳过该条并记 warning，不影响其它规则。

---

## 4. 系统代理

- **Windows**：读注册表
  `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`
  的 `ProxyEnable`、`ProxyServer`、`ProxyOverride`；`ProxyServer` 形如 `host:port` 或按协议的 `http=host:port;https=host:port`；
- **macOS**：`scutil --proxy`；
- **Linux**：`HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY` 环境变量；
- 读取失败或未启用 → 直连，不报错；
- `ProxyOverride` 中的地址（如 `<local>`）按直连处理。

> 注意：Go 的 `http.ProxyFromEnvironment` **不读** Windows 的“Internet 选项”，所以必须自己读注册表。

---

## 5. 实现

新增 `internal/infra/netproxy`：

```go
type Options struct {
    Mode  string        // off | system | custom
    Rules []Rule        // custom 模式使用
}

// Transport returns a transport that applies the mode/rules per request.
// Transports are cached per proxy URL so connections are reused.
func (p *Provider) Transport() *http.Transport

// Client returns an http.Client with the proxy transport and a timeout.
func (p *Provider) Client(timeout time.Duration) *http.Client
```

- `Provider` 从 settings 读取配置，设置变更后刷新（重设 transport 缓存）；
- `Transport.Proxy = func(req *http.Request) (*url.URL, error)`：按 §3 规则匹配；
- 同一代理 URL 复用一个 `*http.Transport`（连接池不因每次请求重建）。

### 5.1 改造清单（必须全部替换）

| 位置 | 现状 |
|---|---|
| `internal/infra/artwork/fetcher.go` | 自建 `http.Client` |
| `internal/infra/covercache/cache.go` | 自建 client（可注入） |
| `internal/infra/lyrics/lrclib.go` | 自建 client |
| `internal/infra/lyrics/kugou.go` | 自建 client |
| `internal/infra/downloader/downloader.go` | 自建 client（下载音频） |
| `internal/app/stream/cache.go` | 自建 client（**流缓存上游**，被墙的源也靠它） |
| `internal/app/library/artist_artwork.go` | 每次新建 client（Deezer 头像） |
| `internal/app/lastfm/service.go` | `http.Get` / `http.Post`（默认 client） |
| `internal/app/updater/service.go` | `http.DefaultClient` |

替换方式：这些组件通过构造函数/Setter 接收 `*http.Client`（或 transport），由 FX 注入 `netproxy.Provider`。

---

## 6. 设置 UI

设置新增「网络」分类（或并入现有「集成/下载」）：

- 模式选择：不使用 / 使用系统代理 / 自定义；
- 自定义模式显示规则表：`匹配` + `代理地址`，可增删、上移下移（顺序即优先级）；
- 代理地址输入框对密码部分打码显示；保存前做 URL 解析校验；
- 修改后立即生效（Provider 刷新 transport），无需重启。

新增 i18n key（12 个语言）：`settings.network.*`。

---

## 7. 插件（明确不做）

- 插件是独立子进程，自己发网络请求，**不受本设置影响**；
- 宿主不向插件注入代理环境变量，也不在插件 API 里提供代理；
- 插件如需代理，由插件自己的设置实现（属于插件作者的责任）；
- 文档与设置页需注明这一点，避免用户误以为“开了代理插件就会走代理”。

---

## 8. 测试

- 匹配器：精确域名、`*.` 通配、IPv4/IPv6、CIDR、非法规则跳过、顺序优先；
- 本机地址永远直连；
- 传输层：用 `httptest` 起一个记录请求的代理，断言命中规则的目标走代理、未命中的直连；
- 系统代理：Windows 注册表读取用 fake（或跳过平台测试），`ProxyServer` 两种格式；
- 设置：迁移 up/down、默认值、非法 JSON 容错；
- 改造回归：给上述 9 处客户端注入 provider 后，现有测试仍通过。

## 9. 验收

自动：§8 全绿。

人工：

1. `off`：Deezer 直连（失败）；
2. `custom`：只给 `api.deezer.com` 配代理 → 艺人头像能拉到，其它请求仍直连；
3. `system`：系统代理开启/关闭分别生效；
4. 代理地址填错 → 只影响命中规则的请求，其它功能正常；
5. 插件搜索不受影响（不经过宿主代理）。

## 10. 风险

- Windows `ProxyServer` 的格式（按协议分段）解析要容错，解析失败按直连；
- 代理认证信息存在 `app_settings` 明文里（与现有设置一致）；UI 打码，日志不得输出完整 URL；
- 只支持 CONNECT 的 HTTPS 代理与 socks5；代理不支持 CONNECT 时 HTTPS 会失败；
- 修改 transport 会影响进行中的下载（新连接用新配置，旧连接保持），可接受。
