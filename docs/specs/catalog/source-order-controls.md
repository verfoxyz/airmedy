# 任务：搜索源开关 + 拖拽排序

> 状态：待开工
> 目的：在「设置 → 搜索源」里可以单独停用某个源（如 kg 的 K 歌版本），并用拖拽快速调整顺序
> 关联：`source-order.md`（顺序本身已实现）、`catalog/search`

---

## 1. 范围

在当前「搜索源」设置页增加两件事：

1. **启停开关**：每个搜索源一个开关；
2. **拖拽排序**：拖动手柄直接调整顺序（保留现有上移/下移按钮作为无障碍替代）。

## 2. 数据

### 2.1 停用列表

新增迁移 `000112_catalog_disabled_sources`：

```sql
ALTER TABLE app_settings
    ADD COLUMN catalog_disabled_sources TEXT NOT NULL DEFAULT '[]';
```

- 值：JSON 数组，如 `["kg"]`；
- 表示“用户停用的源”，不是“启用的源”——新增源默认启用；
- 与插件的启用/禁用无关：插件仍启用，只是不参与搜索。

### 2.2 顺序

沿用现有 `catalog_source_order`；停用源**保留在顺序里**，重新启用后回到原位置。

## 3. 后端

- `SourceOrderRepository` 扩展：
  - `Get()` → `{order []string, disabled []string}`；
  - `Save(order)`、`SetDisabled(provider, disabled)`；
- `selectSources` / `Search` / `Sources`：
  - 先过滤 `disabled`，再按 order；
  - 全部被停用时返回明确错误（`ErrCatalogNoSources`），前端提示「所有搜索源已停用」；
- 匹配（`MatchTrack`）同样不选停用源；
- 停用时如果该源正在下载/播放，不中断已有任务，只影响新搜索；
- 事件：复用 `SourceOrderRepository` 内存缓存失效（写入即失效），无需新事件。

## 4. 前端

### 4.1 列表项

```
[≡ 拖拽手柄]  [源名称]  [上移][下移]      [启用/停用开关]
```

- 停用项整行降低不透明度，但**保留在原位置**（可拖拽、可重新启用）；
- 开关切换立即保存（乐观更新，失败回滚 + 提示）。

### 4.2 拖拽

- 使用 HTML5 drag 或 pointer 事件实现，带占位高亮；
- 拖拽结束只调用一次保存（`SaveSourceOrder`）；
- 键盘：保留上移/下移按钮，Tab 可聚焦；
- 拖拽中不触发行的点击事件。

### 4.3 文案

12 语言新增：

- `settings.source_enabled` / `settings.source_disabled`
- `settings.source_drag_hint`
- `settings.all_sources_disabled`
- `settings.source_save_failed`

## 5. 搜索页联动

- 源筛选按钮只显示启用源；
- 全部停用时搜索页显示空状态提示，而不是报错弹窗。

## 6. 测试

**Go**

- 停用后 `Search` 不再调用该源（fake 断言调用次数）；
- 恢复启用后重新参与；
- 停用源保留在顺序中；
- 全部停用返回 `ErrCatalogNoSources`；
- 停用/顺序写入互不覆盖；
- 迁移 up/down。

**前端**

- 开关切换调用后端并更新 UI，失败回滚；
- 拖拽重排后保存一次（spy 调用次数）；
- 上移/下移仍可用；
- 停用项保留显示；
- 搜索页按钮与空状态。

**人工**

1. 停用 kg → 搜索不再出现 kg 结果，按钮消失；
2. 重新启用 → 结果恢复、位置保留；
3. 拖拽把 kw 放到第一位 → 刷新后保持；
4. 全部停用 → 搜索页给出提示，不崩溃。

## 7. 验收

- 自动：§6 全绿；
- 人工：§7 四条成立。

## 8. 风险

- 停用源若仍被队列/播放引用，不影响播放，只影响新搜索；
- 拖拽与点击卡片冲突需 `pointerdown` 判定阈值；
- 顺序与停用是两个字段，保存时必须分别写入，避免互相覆盖。
