# 需求：删除曲目后清理孤儿专辑/艺术家/流派

> 状态：待开工
> 范围：`internal/app/library` 的 `DeleteTrackFile`
> 硬约束：AI 不做图形化测试；界面由人工确认。

---

## 1. 现象

右键 →「删除文件」删掉某首歌后，**专辑仍然留在库里的专辑页**，点进去是空的。
艺术家、流派、作曲者同理。

---

## 2. 根因（已核对代码）

`DeleteTrackFile` 只做了三件事：

```
os.Remove(file)
→ delete tracks row（级联歌单、收藏、歌词、分析数据）
→ delete download_jobs row
→ 从搜索索引删 track 文档
```

**它没有调用 `cleanupOrphanedEntities`**——而扫描路径里有：

```go
// RemoveWatchedFolder 里
s.cleanupOrphanedEntities(ctx)      // 删 albums/artists/composers/genres 里没有曲目的行
s.CleanupOrphanedArtworks(ctx)      // 删没人引用的封面文件
```

所以"删文件"和"删文件夹"两条路径行为不一致：前者留下空的专辑/艺术家行。

---

## 3. 修法

在 `DeleteTrackFile` 的删除事务和索引清理之后补两步（与 `RemoveWatchedFolder` 一致）：

```go
s.cleanupOrphanedEntities(ctx)
if err := s.CleanupOrphanedArtworks(ctx); err != nil {
    s.logger.Warn("Failed to cleanup orphaned artworks", "error", err)
}
```

注意：

- 两者都是**尽力而为**：内部已 `logger.Warn`，不返回错误，不应让"删除文件"因此失败；
- 顺序：先删 track 行 → 再清理孤儿 → 最后发事件，否则清理时专辑还被引用；
- 事件：现有只发 `library:track-deleted`。专辑页需要刷新，确认前端是否已监听
  `library:*` 的通用刷新；若没有，补一个 `library:album-deleted` 或让专辑视图回落刷新。

---

## 4. 验收

**自动**

- Go 测试：删除某专辑最后一首歌 → 该专辑行不存在；删中间一首 → 专辑仍在；
- Go 测试：删除后 `artwork` 缓存中该专辑的图被清理（或至少调用过清理函数）；
- Go 测试：清理失败（例如 repo 报错）时 `DeleteTrackFile` 仍返回成功（只是记 warn）。

**人工（由人做，AI 不做）**

1. 右键删除某专辑里**唯一**一首歌 → 专辑页不再出现该专辑；
2. 删除某专辑里**其中一首** → 专辑仍在，曲目数减一；
3. 删除后艺术家页没有空的艺术家条目；
4. 该曲目原本的歌单成员关系、收藏、歌词一并消失。

---

## 5. 边界

- 只清理"没有任何曲目引用"的实体；共享同一艺术家的其它专辑不受影响；
- 正在播放的曲目被删除：播放器状态不动（现有行为），不属本轮；
- 智能歌单引用的是查询条件，不因为实体行消失而变化。

---

## 6. 估时

**0.5 天**（两行调用 + 一个测试）。
