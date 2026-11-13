# vi 性能优化报告（2026-11-13）

目标：**Web 端响应速度** + **识别 / 采集效率**。全部改动都基于实测数据定位瓶颈，下面每一条都有对应的前后对比或明确说明"未能量化"。

数据基线：`events.jsonl` 3850 条事件、Qdrant 本地、macOS（`/usr/local/opt/opencv`）。

---

## 一、Web 端响应速度

| 接口 | 优化前 | 优化后 | 说明 |
|---|---|---|---|
| `GET /api/health` | **130 ~ 175 ms** | **1.26 ms** | 原来每次健康检查都跑 `QDEnsure()`：2 次集合查询 + 2 次建索引 PUT |
| `GET /api/labels` | 5.5 ~ 7.5 ms，19,982 B | **3.6 ms，4,041 B** | gzip 压缩（-80%），标签索引替代全表扫描 |
| `GET /api/stats` | 1.7 ~ 2.9 ms | **1.2 ms** | 零拷贝事件视图 |
| `GET /api/identities` | ~5 ms，6,875 B | 5.6 ms，**1,542 B** | 体积 -78%，延迟持平（聚合本身仍是 O(n)） |
| `GET /api/search/date` | ~1 ms | 1.1 ms | 持平 |
| `POST /api/capture`（新坐标，无前端地址） | **110 ~ 580 ms** | **2.1 ~ 2.8 ms** | 逆地理编码移出请求路径 |
| `POST /api/capture`（带前端地址） | — | **2.1 ~ 2.8 ms** | 前端地址直接入库，零网络请求 |
| `POST /api/capture`（同坐标重复） | 3.6 ms | 1.7 ms | — |

### 1. 健康检查不再每次都建索引
`internal/qdrant/qdrant.go`
- `Ensure()` 结果加 10 分钟 TTL，健康检查改用新增的 `Ping()`（单请求、300ms 超时）。
- 这是全站最先被调用的接口（页面加载即打），原来 130+ms 是纯浪费。

### 2. 逆地理编码从请求路径里摘出去
`internal/geo/geo.go`、`internal/store/store.go`
- 采集时 `ReverseTimeout(lat, lon, 200ms)` 替代原来的阻塞式 `Reverse()`（最多等 6 秒的网络往返）。
- 同坐标 **singleflight**：并发采集同一位置只发一次外部请求。
- 逻辑顺序：精确缓存 → 邻近缓存（≤2km，GPS 抖动/新街区）→ 全新坐标直接返回空、后台解析。
  **宁可比对精度晚一拍，也不让请求卡在网络往返上。**
- 前端 `/api/geocode` 已解析的地址随 capture 表单带上来（`address` 字段），命中即入库并回写缓存 → 常规路径完全不发外部请求。
- 缓存落盘从"每次未命中重写整份 geo_cache.json"改为 5 秒防抖写入。

### 3. 文本响应 gzip + 弱 ETag
`internal/api/compress.go`（新增）
- 只对 `/api/*` 与 html/js/css/json 等文本路径生效，图片/模型/wasm 直接透传不缓冲。
- GET 200 响应带 `W/"sha1"`，浏览器重复请求命中 304；`Vary: Accept-Encoding` 正确设置。
- 206（Range 分段）不压缩，避免破坏 `Content-Range`。
- 静态资源策略：`/js/`、`/css/` 缓存 1 天，`/` 改为 `no-cache`（原来是 60s 必须回源校验）。

### 4. 事件列表零拷贝 + 分页早停
`internal/store/store.go`、`internal/api/api.go`
- 新增 `EventSlice()`：只返回底层切片头（追加只会写 len 之后的槽位，读者持有定长切片头是安全的），不再每个请求复制整份事件表。
- `list()` 从新到旧扫描，只构造当前页的 view，不再为全表生成对象再切片。
- `LabelsFor()` 从"遍历全部标签"改为 `labelIdx["kind|target"]` 索引查找（O(1)），并在写入/删除/重载时维护索引。列表、身份、统计每次请求都要对每条事件查一次标签，原来这是最热的内层循环。

### 5. HTTP 服务参数
`main.go`：显式 `ReadHeaderTimeout / IdleTimeout / MaxHeaderBytes`，避免慢客户端长期占住 goroutine。

### 6. 顺手修掉的真 bug：命令行参数被吞
`flag.Parse()` 遇到第一个非 flag 参数就停止，而 `os.Args[1]` 恰好是子命令 `serve`，所以 `start0.sh` 里的 `serve "$@"` 参数**从来没有生效过**。现在启动前把子命令摘掉，`bin/vi serve -addr 127.0.0.1:8699` 才真正可用。

---

## 二、识别 / 采集效率（前端实时链路）

这部分收益**无法在当前环境量化**（需要真实浏览器跑摄像头）。以下说明都是结构性的、可解释的开销消除，不是调参。

`web/js/face.js`
- `detect()` 不再**每帧每张脸**做：新建 112×112 `OffscreenCanvas` + 全帧 `drawImage` + 相似变换。
  对齐图改由 `align()` 在**真正推理时**生成（每 track 约 3s 一次）。逐帧从 3 次 canvas 操作降到 0 次。
- 肤色检测（误检过滤）原来每张脸每帧 `new OffscreenCanvas(w,h)` + 全框 `getImageData`；改为复用一张 64×64 画布 + 缩采样，像素量降 1~2 个数量级。
- `crop()` 改为 2 张画布轮转复用（`toBlob` 在调用瞬间快照位图，轮转可避免两次上传互相覆盖）。

`web/js/capture.js`
- 人脸/车牌上传从 `await uploadFace(...)`（**阻塞检测循环**）改为入队：最多 2 个在途，一次网络往返不再停掉整帧检测。
- 推理 worker 不再等上传返回；`track.uploading` 防止同一 track 重复入队。
- 车牌上传失败会回滚去重占位，2 分钟内可重试。

`web/js/plate.js`
- 640×640 检测张量（每帧 4.9MB Float32Array）改为复用缓冲：按 `byteLength` 校验，ORT proxy 若 transfer 掉缓冲则自动重新分配，不做任何假设。
- 识别预处理复用一张 160×48 画布 + 复用识别张量缓冲。
- 同位置 3s 内复用识别结果（停车、红灯等静止场景），跳过 rec 前向。

---

## 三、批量导入 / 采集效率

| 项目 | 结果 |
|---|---|
| 12MP 大图解码 | **52 ms/张 → 33 ms/张**（长边 > 2400 用 1/2 降采样解码） |
| YuNet 输出解码 | 512 次 `GetFloatAt` cgo → 一次 `DataPtrFloat32` 连续拷贝；`YuNet` 阶段 8.7 → 7.8 ms/张 |
| arc 特征提取 | `GetFloatAt` ×512 → 一次连续拷贝（每张人脸固定开销） |
| 大批量日志 | >50 张时按每 10 张刷新进度、仅在有过滤/异常时打明细，避免日志 I/O |
| 端到端 453 张导入 | 波动 ±10s，**未观察到稳定提升** |

`cmd/benchface` 是新增的阶段耗时基准工具（可复跑）：

```
./bin/benchface -dir data/images/import -n 40 -workers 1
  图片 40 张 / 人脸 40 张（墙钟 5.75s，吞吐 6.96 人/秒）
    解码 0.71 ms    YuNet 7.8 ms    arc特征 143 ms
```

**结论：导入的瓶颈是 `arc.onnx` 前向本身（143 ~ 224 ms/张，随机器负载波动），占单张总耗时 90% 以上。** 其余优化都是几个 ms 级别的边角料，所以端到端测不出来。

### 为什么没有做多 worker 并行导入（实测否决）
每个 worker 要独立 `gocv.Net` 实例（OpenCV DNN 的 `Net` 不线程安全），`arc.onnx` 是 **130MB**，4 个 worker 就是 520MB 常驻内存。实测：

| worker | 吞吐 |
|---|---|
| 1 | 4.82 人/秒 |
| 2 | 5.86 人/秒（+22%） |
| 4 | 7.27 人/秒（+51%），内存 ×4 |

每个 worker 的单张耗时从 257ms 涨到 630ms（CPU/内存带宽争抢，OpenCV 内部已有并行）。**1.5x 吞吐换 4x 内存不划算**，故未实现。

---

## 四、顺带修复的数据正确性 bug

`internal/store/store.go`：`geo.WGS84ToGCJ02(lng, lat)` 返回 `(lng, lat)`，但代码写成 `gcjLat, gcjLon = &g, &l` → **每条事件的经纬度都是反的**（采集与导入两条路径都错）。已修正为 `&l, &g`。

示例：传入 `lat=30.66, lon=104.03`，修复前返回 `lat=104.03, lon=30.66`。

⚠️ 历史 `events.jsonl` 里已存的坐标仍是反的，需要一次性回写脚本才能修正（未执行，等确认）。

---

## 五、建议的下一步（未实施，按收益排序）

1. **arc.onnx 换执行后端**：导入提速的最大杠杆。OpenCV DNN 在 macOS 上只能吃 CPU；换 ONNX Runtime（CoreML/CUDA EP）或 TensorRT，单张 143ms 有机会降到 20~40ms。需要引入 ORT 的 C API 或换推理框架，改动较大。
2. **历史事件经纬度回写**：一次性脚本，按 `lat>90 || lon>90` 之类的规则识别并交换，先备份 `events.jsonl`。
3. **Qdrant 侧**：当前每次 capture 都同步 upsert（`wait=false`），已足够；若事件量到十万级，再考虑把 upsert 改为异步批量。
4. 前端可加 Web Worker 承载 640×640 检测预处理（当前在主线程做 409k 像素的平面重排），进一步压主线程抖动。

---

## 六、运维注意：重启请用 ./start.sh

`start0.sh` 是把 **Qdrant 作为 `vi` tmux 会话的子进程**一起拉起的（`./bin/qdrant &` + `exec ./bin/vi serve`）。
所以 `tmux kill-session -t vi` 会连 Qdrant 一起带走，之后只单独起 `./bin/vi serve` 就会出现
"Qdrant 未连接"——这不是误报，是 Qdrant 真的没在跑。

```bash
cd /Users/51pwn/MyWork/vi && ./start.sh     # 正确：缺 Qdrant 会自动拉起
```

健康检查侧也做了两处防误判：探测超时 300ms → 1s（Qdrant 批量 upsert 时可能几百 ms 不响应）；
异常结果只缓存 5s（正常 15s），恢复后页面能立刻显示已连接。

## 七、改动清单

```
internal/api/api.go               +86/-   健康检查缓存、零拷贝扫描、地址直通、静态缓存策略
internal/api/compress.go          新增    gzip + ETag 中间件
internal/geo/geo.go               +195/-  singleflight、邻近缓存、防抖落盘、非阻塞 ReverseTimeout
internal/store/store.go           +117/-  标签索引、EventSlice、采集地址优先级、经纬度修正
internal/qdrant/qdrant.go         +39     Ensure TTL + Ping
internal/importface/importface.go +123/-  降采样解码、embedding 零拷贝、日志节流
internal/importface/yunet.go      +58/-  YuNet 输出向量化解码
main.go                           +15/-  子命令 flag 修复、HTTP 超时
web/js/face.js                    +53/-  对齐图延后生成、复用画布
web/js/capture.js                 +58/-  上传非阻塞入队、地址直传
web/js/plate.js                   +64/-  张量/画布复用、识别结果缓存
cmd/benchface                     新增    导入链路阶段基准工具
```
