# Bug：同一首歌因艺术家写法不同产生 3 条库记录（且远程行没有封面）

> 状态：待开工
> 现象：库里 `Synthetic Fracture` 有三条：一条本地文件（有封面）、两条远程行（wy/kw，无封面）
> 范围：艺术家归一化 + 远程行注册去重；不改播放

---

## 1. 数据

| 行 | 来源 | raw_artist_names | path | artwork |
|---|---|---|---|---|
| 1 | 本地导入 | `Sad Keyboard Guy;Gardens` | 本地 flac | 有 |
| 2 | wy / 1941192069 | `Sad Keyboard Guy; Gardens` | remote | 无 |
| 3 | kw / 217807740 | `Sad Keyboard Guy; Gardens` | remote | 无 |

（本地行的实体艺人是 `Gardens` + `Sad Keyboard Guy`，顺序与分隔符都与远程行不同。）

## 2. 根因

### 2.1 艺术家比较是“整串”比较

当前跨源匹配（`catalog.MatchTrack`、stream cache 跨源匹配、缓存去重分组）都用：

```go
domain.NormalizationKey(row.Artists[0]) == domain.NormalizationKey(targetArtist)
```

- 只比较**第一个**艺人；
- 不处理分隔符（`;`、`,`、`、`、`&`、`feat.`）和**顺序**；
- `"Sad Keyboard Guy;Gardens"` 与 `"Gardens, Sad Keyboard Guy"` 永远不相等。

### 2.2 远程行注册只看 identity

搜索/推荐点播放会 `EnsureRemoteTrack`，它按 `provider + external_id` 建行，**不会**检查本地是否已有同一首歌（本地行没有 provider identity），于是同一首歌出现“本地 + 多个远程”并存。

### 2.3 远程行没有封面

封面是在**播放时**按 identity 物化到 `artwork_key` 的；没播放过的远程行在列表里就是无封面（与艺术家写法无关，但用户看到的“两条没封面”是这个原因）。

## 3. 修法

### 3.1 艺人集合归一化（核心）

新增：

```go
// ArtistSetKey turns a credit list into a comparable key: every artist string
// is split on common delimiters, each part is normalized, duplicates removed,
// the set is sorted and joined. Order and separators no longer matter.
func ArtistSetKey(names []string) string
```

- 分隔符：`;` `,` `、` `/` `&` `feat.` `ft.` `with`（大小写不敏感）；
- 每段 `NormalizationKey` 后去重、排序、用 `\x1f` 连接；
- 替换以下比较点：
  - `catalog.MatchTrack` 的艺人判断（不再只看 `Artists[0]`）；
  - stream cache 跨源匹配（`title_key + artist_key`）；
  - 缓存去重分组；
  - 推荐匹配。

> 标题仍严格相等；时长仍 ±5s。

### 3.2 注册远程行前先找本地同曲

`EnsureRemoteTrack` / `EnsureStreamableTrack`：

1. 先按 `title + ArtistSetKey`（时长 ±5s）查**本地非 remote** 行；
2. 找到 → 不新建远程行，直接返回该行，并把 `provider/external_id` 合并到它上面（复用现有 merge/`carryIdentity` 逻辑，identity 只保留一个，其余忽略）；
3. 没找到 → 按现在的逻辑新建远程行。

这样同一首歌不会再出现“本地 + 远程”并存。

### 3.3 已有重复的清理

- 一次性迁移/工具（可选，本期可只做新数据）：对“标题相同 + 艺人集合相同 + 时长接近”的本地行与远程行执行 merge；
- 不做自动删除，避免误合并；可以在文档后续单列任务。

### 3.4 封面

- 远程行列表无封面是“未播放”的结果，按既有封面任务处理（播放时物化 / 预热封面缓存）；
- 本任务的 3.1、3.2 完成后，这类重复不再产生，封面自然只剩一份。

## 4. 测试

**Go**

- `ArtistSetKey`：`"Sad Keyboard Guy;Gardens"`、`"Gardens, Sad Keyboard Guy"`、`"Gardens & Sad Keyboard Guy"` 得到同一 key；
- 顺序、重复、多余空格、`feat.` 处理正确；
- `MatchTrack`：行艺人顺序不同也能命中；
- 注册远程行：本地已有同曲 → 返回本地行、不新建远程行、identity 合并；
- 时长超出容差 → 仍新建远程行；
- 缓存去重：艺术家写法不同的两份被识别为同一组。

**人工**

1. 搜索这首 `Synthetic Fracture` 并播放 → 库里不再多出远程行，直接命中本地文件；
2. 封面正常显示（本地文件自带）；
3. 其它艺术家顺序不同的场景不再重复。

## 5. 验收

- 自动：§4 全绿；
- 人工：§4 三条成立；
- 艺术家写法差异不再导致重复记录。

## 6. 风险

- 艺人集合归一化可能把“不同艺人的同名合作”合并（如 A & B 与 B & A 视为同一首），标题+时长仍需一致，风险可控；
- 远程行合并到本地行后，该行只保留一个 identity，之后再从另一个源播放会走跨源匹配而不是新行；
- 历史重复不会自动消失（§3.3 只处理新数据），需要时再做清理任务。
