# Bug：提升后的本地行没有封面

> 状态：待开工
> 现象：远端曲目提升（保存/收藏）后，封面消失（本地行 `artwork_key` 为空）
> 关联：`promote.go`、`fix-recommendation-covers.md`、本地导入流程

---

## 1. 数据证据

```
tracks: 9b02afc0… | Here It Comes | wy/28667025 | …\Here It Comes - Pegboard Nerds.flac | ready | artwork_key=''
```

提升前该曲目在界面上有封面（封面缓存里存在），提升后本地行的 `artwork_key` 为空。

## 2. 根因

1. 提升会把 remote 行替换成“本地导入”行；
2. 本地导入的封面**只从文件内嵌图提取**：
   `metadataExtractor.ExtractArtwork(path)` → 有图才写 `artwork_key`；
3. 如果提升时没有嵌入封面（下载内容设置未开启封面，或封装失败），文件里就没有图；
4. 旧 remote 行/封面缓存里的图**没有迁移**到新行，于是封面丢失。

## 3. 修法

### 3.1 合并时保留封面键

在提升/合并（替换 remote 行）时：

- `newTrack.ArtworkKey == "" && oldTrack.ArtworkKey != ""` → 继承旧键；
- 同步写 `album.ArtworkKey`（若有）。

### 3.2 缓存兜底

- 旧行为空或没有键，但 `coverAssets.BytesForIdentity(provider, externalID)` 有数据
  → `artworkCache.Save(bytes, mime)` 得到键，写入新行；
- 与 `promoteCover` 已有的字节查找逻辑复用。

### 3.3 优先级

```
1. 文件内嵌图（导入时已提取）
2. 旧行 artwork_key
3. 封面缓存字节（identity）
4. 保持为空（占位图）
```

## 4. 测试

**Go**

- 提升：旧行有 `artwork_key`、新文件无内嵌图 → 新行继承该键；
- 旧行无键但封面缓存有字节 → 保存后新行有键；
- 文件内嵌图存在 → 使用内嵌图的键（优先）；
- 三者都没有 → 新行键为空，不报错；
- 合并（`track:merged`）路径同样保留键。

**人工**

1. 提升一首有封面的远端曲目 → 左下角/详情/列表中封面仍在；
2. 提升一首无封面的曲目 → 显示占位图，不报错；
3. 重启后封面仍在。

## 5. 验收

- 自动：§4 全绿；
- 人工：§4 三条成立。

## 6. 风险

- 继承旧键时注意旧键可能已从 artwork cache 淘汰 → 显示失败时回退占位图；
- 不要覆盖“文件内嵌图”的键（它更准确）。
