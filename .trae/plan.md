# vi 系统 4 项功能优化计划

## 背景

vi 系统（人脸+车牌识别）需要 4 项改进：
1. **批量标签** — 当前每个头像单独打标签，效率低
2. **批量删除** — 误检数据无法清理，只能逐条删除（无 API）
3. **分页跳转** — 只有"上一页/下一页"，无法快速跳页
4. **Web 采集误检** — 前端 MediaPipe 检测和 importfaces 一样有误检问题

---

## 功能 1：批量标签分配

### 现状
- [labels.html](file:///Users/51pwn/MyWork/vi/web/labels.html) 每行一个头像 + "命名"按钮 → prompt 输入 → 调 `/api/label/add`
- API: `POST /api/label/add` 单条 `{name, event_id}`

### 方案
**不改后端 API**，前端加多选模式：
1. 每行头像加 checkbox
2. 底部工具栏：全选/反选 + 标签输入框 + "批量打标"按钮
3. 选中多个后，循环调现有 `/api/label/add`（N 次请求）
4. 完成后刷新列表

**改动文件：**
- `web/labels.html` — 加多选 checkbox + 底部批量操作栏
- `web/js/labels.js` — 加多选逻辑 + 批量提交函数

---

## 功能 2：批量删除事件

### 现状
- 无删除 API，无删除 UI
- 事件存在 `data/events.jsonl`（JSONL 追加写），图片在 `data/images/`
- Qdrant 向量用 `event.ID` 作为 point_id

### 方案
**后端新增 2 个 API：**
1. `DELETE /api/events/batch` — 批量删除 `{ids: ["uuid1", "uuid2"]}`
   - 从 `events.jsonl` 移除对应行（重写文件）
   - 删除本地图片文件
   - 从 Qdrant 删除 point（`DELETE /collections/{name}/points`）
2. `POST /api/events/batch-delete` — 同上（兼容旧浏览器）

**前端改动：**
- `web/search.html` / `web/labels.html` — 加"删除"按钮（单条）+ "批量删除"（多选模式）
- 删除前确认对话框

**改动文件：**
- `internal/api/api.go` — 新增 `handleDeleteEventsBatch` 路由
- `internal/store/store.go` — 新增 `DeleteEvents(ids []string) error` 方法
- `internal/qdrant/qdrant.go` — 新增 `DeletePoints(ids []string) error` 方法
- `web/js/search.js` / `web/js/labels.js` — 删除按钮 + 批量删除逻辑

---

## 功能 3：分页数字跳转

### 现状
- [search.js](file:///Users/51pwn/MyWork/vi/web/js/search.js) 只有 `prev`/`next` 按钮
- API 返回 `{page, page_size, total, has_more, items}`

### 方案
在 `prev`/`next` 之间插入数字页码：
```
[上一页] 1 2 3 ... 7 8 9 ... 15 16 [下一页]
```
- 当前页高亮
- 超过 10 页时省略中间（显示前后 2 页 + 省略号）
- 点击数字直接跳页

**改动文件：**
- `web/search.html` — 分页区域加 `<div id="pageNumbers">` 容器
- `web/js/search.js` — `search()` 函数渲染数字页码

---

## 功能 4：Web 采集误检过滤

### 现状
- [face.js](file:///Users/51pwn/MyWork/vi/web/js/face.js) 用 MediaPipe FaceLandmarker 检测（`minFaceDetectionConfidence: 0.4`）
- 检测到人脸后直接上传，没有二次验证
- 误检来源：MediaPipe 本身 + 低置信度

### 方案
**两层过滤（在 `capture.js` 的 `faceLoop` 中实现）：**

1. **置信度提升**：`minFaceDetectionConfidence` 从 0.4 → 0.6（减少低质量检测）
2. **embedding norm 过滤**（复用 importfaces 逻辑）：
   - `embed()` 返回 512 维向量后，计算 L2 norm
   - norm < 1.5 或 > 8.0 → 丢弃（非人脸区域）
3. **肤色检测**（复用 importfaces 逻辑）：
   - 裁剪区域转 HSV，检查肤色像素比例
   - < 15% → 丢弃

**改动文件：**
- `web/js/face.js` — `FaceEngine` 加 `hasSkinTone()` 和 `isValidFace()` 方法
- `web/js/capture.js` — `faceLoop` 和 `ensureInferWorker` 里加过滤逻辑

---

## 实施顺序

| 优先级 | 功能 | 改动量 | 依赖 |
|---|---|---|---|
| P0 | 批量删除 | 中 | 无 |
| P1 | 批量标签 | 小 | 无 |
| P2 | 分页跳转 | 小 | 无 |
| P3 | Web 误检过滤 | 中 | 无 |

P0-P2 是纯前端/后端 CRUD，可并行。P3 需要调参验证。

---

## 验证方案

1. **批量删除**：导入 10 张测试图 → 删除 5 张 → 确认 events.jsonl 减少 + Qdrant count 减少
2. **批量标签**：选中 3 个头像 → 输入"测试" → 确认 3 条事件都关联标签
3. **分页**：制造 50 条数据 → 确认页码 1/2/3 显示正确，点击跳转正常
4. **Web 误检**：用婚礼照片在摄像头前展示 → 确认领带/花朵不再触发上传
