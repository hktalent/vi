# vi · 见到你

> 一个全栈自研的**视频监控 + 人脸 / 车牌记忆系统**。摄像头画面只在浏览器本地处理（ONNX Runtime Web + MediaPipe），特征矩阵与裁剪后的小图上传到 Go 后端，Qdrant 存向量 + payload，支持标签、多维度检索、统计、每日 Markdown 总结、命中标签自动鸣叫。手机端友好，零构建。

---

## ✨ 功能一览

| 能力 | 说明 |
|---|---|
| 🔍 **实时人脸检测** | MediaPipe FaceLandmarker，最多同时检测 **10 张人脸**，每 2 帧跑一次，中间帧由跟踪器衔接 |
| 🧬 **人脸特征提取** | ArcFace R100 FP32 → **512 维余弦向量**，5 点相似变换对齐 112×112 |
| 👤 **人脸属性识别** | FairFace 性别（男/女）+ 年龄段（9 档 0-2 … 70+）+ ViT 眼镜二分类 |
| 🚗 **车牌检测 + 识别** | HyperLPR YOLO 检测（最多 **5 张**）+ CRNN CTC 字符识别（78 字符表） |
| 🧠 **身份归一化** | 新入库人脸自动在 Qdrant 查最近邻（阈值 0.45），同一人 → 同一 `person_id` |
| 🏷️ **标签体系** | 任何人脸 / 车牌可打标签，命中时前端鸣叫 + 生成 Alert |
| 🔎 **多维度检索** | 按日期 / 车牌 / 标签名 / 地名分页（每页 20 条） |
| 📊 **统计面板** | 区间内人脸次数、不同人员数、车牌次数、时段分布、标签命中率 |
| 📅 **每日 Markdown 总结** | 自动（可手动触发）生成当日概览 + 标签命中 + 时段 + 高频地点 + 最近记录 |
| 🗺️ **定位 + 地址 + 海拔 + 方向** | 浏览器 Geolocation 提供经纬度/海拔，DeviceOrientation 提供相机方向角度，后端高德/Open-Meteo 逆地理编码，本地缓存；方向入库角度，前端显示中文方位（东北/西/南…） |
| 📸 **批量图片导入** | `vi importfaces -dir /path` 递归扫描本地图片 → Haar/YuNet 检测 → ArcFace 提特征 → upsert Qdrant；**自动解析 EXIF**：GPS 坐标 → 逆地理得中文地址；GPSImgDirection → 相机方向；Altitude → 海拔；DateTimeOriginal 作 captured_at |
| 📱 **手机端** | 响应式布局 + 竖长视频预览 + 设备枚举切换摄像头（前后置）；iOS 13+ 方向传感器需用户授权 |
| 🔒 **隐私友好** | 推理全在本地浏览器完成；后端只收裁剪图 + 向量 + GPS |

---

## 🏗️ 架构图

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                             浏览器（前端，无构建）                            │
│  ┌──────────────────────────────────────────────────────────────────────┐   │
│  │                         video element (1280×720)                       │   │
│  │                              │                                        │   │
│  ├──────────────────────────────┼───────────────────────────────────────┤   │
│  │  FaceLandmarker (每 2 帧)    │    YOLO 车牌检测 (每 5 帧+帧差跳过)     │   │
│  │  numFaces = 10               │    最多 5 张车牌                        │   │
│  │       │                      │         │                              │   │
│  │  IoU 跟踪器                  │    CRNN CTC 识别                       │   │
│  │  最多 12 个 track            │    最多 5 张                            │   │
│  │       │                      │         │                              │   │
│  │  ┌────┴──── 推理队列 worker（串行，ORT WASM 限制） ────┐                 │   │
│  │  │  ArcFace embed (512d)                               │                 │   │
│  │  │  FairFace gender / age / glasses                    │                 │   │
│  │  │  track 入队 → 推理 → 立即上传，不阻塞下一个           │                 │   │
│  │  └────────────────────────────────────────────────────┘                 │   │
│  │                                                                      │   │
│  │  浏览器侧防抖：同一 track ≥ 3s 推理间隔 + ≥ 6s 上传间隔                  │   │
│  └──────────────────────────────────┬─────────────────────────────────────┘   │
│                                     │ multipart (小图 + 向量 + attrs)         │
└─────────────────────────────────────┼───────────────────────────────────────┘
                                      │
                                      ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                         Go 后端（单二进制，net/http）                         │
│  ┌──────────┐   ┌──────────┐   ┌──────────┐   ┌──────────────┐              │
│  │ API 路由  │──▶│ Store    │──▶│ Qdrant   │   │ Geocoder     │              │
│  │ capture  │   │ cooldown  │   │ 512 Cos. │   │ BigDataCloud │              │
│  │ search   │   │ events    │   │ sparse   │   │ 本地缓存      │              │
│  │ labels   │   │ alerts    │   │ payload  │   └──────────────┘              │
│  │ stats    │   │ labels    │   └──────────┘                                │
│  │ summary  │   └──────────┘                                                │
│  └────┬─────┘                                                               │
│       │ 每天 23:50 自动触发                                                   │
│       ▼                                                                     │
│  Markdown 总结 → data/reports/summary-YYYY-MM-DD.md                          │
│                                                                             │
│  COOP/COEP 头 → 使 SharedArrayBuffer 可用 → 4 线程 WASM                      │
│  Cache-Control → /models/ /vendor/ immutable 永久缓存                        │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 前端并行架构

```
视频帧
  │
  ├── FaceLandmarker 检测（一次性返回所有脸）
  │     ├── 最多 10 张人脸
  │     └── IoU 跟踪 → 最多维护 12 个 track
  │
  ├── 推理队列 ensureInferWorker() （串行，受 ORT WASM 限制）
  │     ├── track_A.embed + attrs → 推理完立即上传
  │     ├── track_B.embed + attrs → 紧接着
  │     └── track_C ... → 排队等待，下一轮 rAF 补入
  │
  ├── YOLO 检测 + CRNN 识别（每 5 帧 + 帧差跳过）
  │     └── 最多 5 张车牌
  │
  └── Go 后端（天然并发）每请求一个 goroutine
```

---

## 🎯 技术栈

| 层 | 技术 | 版本 |
|---|---|---|
| 后端 | Go | 1.22+ |
| 后端 HTTP | `net/http`（标准库，无框架） | — |
| 后端 CGO | OpenCV 4（gocv v0.38.0） | Homebrew / apt |
| EXIF 解析 | `github.com/rwcarlsen/goexif` | 最新 |
| 向量存储 | Qdrant | **下载见下方**（已内置 `bin/qdrant`） |
| 前端运行 | vanilla JS（无构建、无打包） | — |
| 人脸检测 | MediaPipe FaceLandmarker | 最新 WASM |
| 人脸特征 | ONNX Runtime Web · ArcFace R100 FP32 | — |
| 属性识别 | FairFace (gender + age) · ViT (glasses) | — |
| 车牌检测 | HyperLPR YOLO · ONNX | — |
| 车牌识别 | CRNN · CTC · ONNX | — |
| 逆地理编码 | 高德 Web Service Key（可选，国内坐标准） / Open-Meteo 免 key 备用 | — |

---

## 📂 项目结构

```
vi/
├── main.go                    # 入口 + 子命令路由（serve / importfaces / reset / version / help）
├── internal/
│   ├── api/api.go             # HTTP 路由 + API handler（capture / search / labels / stats …）
│   ├── auth/auth.go           # Token 签发 + 媒体 URL 签名 + 密码哈希
│   ├── config/config.go       # 配置加载（flag + env + 默认值）
│   ├── geo/geo.go             # 逆地理编码（高德优先 / Open-Meteo 备用）+ WGS-84 ↔ GCJ-02 + 本地缓存
│   ├── importface/importface.go  # 批量图片导入：检测 → 裁剪 → ArcFace → upsert
│   ├── importface/yunet.go       # YuNet 人脸检测器（ONNX DNN，5 点关键点）
│   ├── exif/exif.go              # 图片 EXIF 解析：GPS/海拔/方向/创建时间
│   ├── qdrant/qdrant.go       # Qdrant REST 客户端（dense + sparse）
│   ├── store/store.go         # Event / Label / Alert / Cooldown / 落盘 / JSONL
│   └── report/report.go      # 每日 Markdown 总结 + 定时调度
├── go.mod
├── start.sh                   # 一键启动（首次自动 patch gocv + 编译 + 拉起 Qdrant）
├── impt.sh                    # 批量导入脚本：./impt.sh <图片目录> [标签]
├── stop.sh                    # 停止
│
├── bin/
│   ├── vi                     # 编译后的后端二进制（全功能，含 importfaces）
│   └── qdrant                 # Qdrant 可执行文件（见下方下载说明）
│
├── .build/gocv/               # gocv patch 目录（解决 OpenCV 4.11+ videoio 兼容）
├── scripts/
│   ├── patch-gocv.sh          # 自动 patch gocv（幂等，go.mod replace）
│   ├── gocv-videoio-stub.cpp  # OpenCV 4.11+ videoio 兼容 stub
│   ├── prepare_models.py      # 模型下载 / 导出脚本
│   └── export_glasses.py      # 眼镜分类模型导出
│
├── models/                    # 前端推理模型（~400MB）
│   ├── face_landmarker.task   # 3.7MB · MediaPipe 人脸关键点
│   ├── arc.onnx               # 130MB · ArcFace R100 FP32
│   ├── face_gender.onnx       # 83MB · FairFace 性别
│   ├── face_age.onnx          # 83MB · FairFace 年龄（9 档）
│   ├── face_glasses.onnx      # 85MB · ViT 眼镜二分类
│   ├── plate_det.onnx         # 3.7MB · HyperLPR YOLOv5-face
│   ├── plate_rec.onnx         # 9.8MB · CRNN CTC 车牌识别
│   └── chars.json             # 78 字符表
│
├── web/                       # 前端（直接 FileServer，无构建）
│   ├── index.html             # 实时监测（主页面）
│   ├── labels.html            # 标签管理
│   ├── search.html            # 多维检索
│   ├── stats.html             # 统计
│   ├── css/style.css
│   ├── js/
│   │   ├── capture.js         # 采集编排 + 推理队列 + 上传
│   │   ├── face.js            # 人脸引擎（检测 + 对齐 + embed + attrs）
│   │   ├── plate.js           # 车牌引擎（检测 + 识别）
│   │   └── common.js          # 通用工具（API / 定位含海拔 + DeviceOrientation 方向传感器 / 鸣叫 / 方位转换）
│   └── vendor/
│       ├── ort/               # ONNX Runtime Web WASM（4 线程 SIMD）
│       └── mp/                # MediaPipe vision bundle + wasm
│
└── data/                      # 运行时数据（自动创建）
    ├── images/YYYY/MM/DD/     # 人脸 / 车牌裁剪图 + import/ 导入抠图
    ├── qdrant/                # Qdrant 向量库数据
    ├── events.jsonl           # 事件日志（含 altitude / heading）
    ├── labels.json            # 标签表
    ├── alerts.jsonl           # 告警日志
    ├── geo_cache.json         # 地址缓存
    ├── qdrant.log
    └── reports/               # 每日总结 Markdown
```

---

## 🚀 快速开始

### 1. 前置条件

| 依赖 | 版本 | 说明 |
|---|---|---|
| **Go** | ≥ 1.22 | **编译需要 CGO 开启**（gocv 依赖） |
| **OpenCV 4** | ≥ 4.8 | macOS: `brew install opencv`；Ubuntu: `apt install libopencv-dev` |
| **pkg-config** | — | macOS Homebrew 自带；Ubuntu: `apt install pkg-config` |
| **Qdrant 二进制** | **见下方下载** | 裸进程放到 `bin/qdrant`，start.sh 自动拉起 |
| 现代浏览器 | Chrome / Edge / Safari | 需要支持 WebAssembly + SharedArrayBuffer |
| HTTPS 或 `127.0.0.1` | — | 浏览器摄像头权限要求安全上下文 |

#### Qdrant 下载地址

Qdrant 官方 releases：[https://github.com/qdrant/qdrant/releases](https://github.com/qdrant/qdrant/releases)

**vi 项目用裸二进制进程启动**（`start.sh` 会自动拉起 `./bin/qdrant`），只需下载对应架构的可执行文件放到 `bin/` 目录：

```bash
# macOS arm64 (Apple Silicon)
curl -L -o bin/qdrant https://github.com/qdrant/qdrant/releases/download/v1.12.4/qdrant-aarch64-apple-darwin
chmod +x bin/qdrant

# macOS amd64 (Intel)
curl -L -o bin/qdrant https://github.com/qdrant/qdrant/releases/download/v1.12.4/qdrant-x86_64-apple-darwin
chmod +x bin/qdrant

# Linux amd64
curl -L -o bin/qdrant https://github.com/qdrant/qdrant/releases/download/v1.12.4/qdrant-x86_64-unknown-linux-gnu.tar.gz
tar xzf qdrant-x86_64-unknown-linux-gnu.tar.gz -C bin/
chmod +x bin/qdrant

# Linux arm64 (树莓派 / ARM 服务器)
curl -L -o bin/qdrant https://github.com/qdrant/qdrant/releases/download/v1.12.4/qdrant-aarch64-unknown-linux-gnu.tar.gz
tar xzf qdrant-aarch64-unknown-linux-gnu.tar.gz -C bin/
chmod +x bin/qdrant
```

> 版本号 `v1.12.4` 替换为最新版即可：[查看所有 release](https://github.com/qdrant/qdrant/releases/latest)

**Docker 备选（可选，与 start.sh 互斥）**：如果你的环境只能跑容器，可以手动用 Docker 启动 Qdrant——但 `start.sh` 里已经带了"检测 6333 端口再决定是否拉起 qdrant"的逻辑，所以两种方式不会冲突：

```bash
# 手动 Docker 启动 Qdrant（之后再跑 ./start.sh，它会检测到 6333 已就绪，只起 vi）
docker run -d --name vi-qdrant \
  -p 6333:6333 -p 6334:6334 \
  -v $(pwd)/data/qdrant:/qdrant/storage \
  qdrant/qdrant:v1.12.4
```

### 2. 启动

```bash
cd vi/
./start.sh
# → 首次运行自动 patch gocv（OpenCV 4.11+ 兼容）→ 编译 Go 源码 → 启动 Qdrant (6333) → 启动 vi (8688)
# → 浏览器打开 http://127.0.0.1:8688
```

### 3. 首次运行

1. 打开浏览器访问 `http://127.0.0.1:8688`
2. 允许摄像头 + 定位权限
3. **iOS 13+**：首次点击"启动"时弹出方向传感器授权弹窗，点允许即可启用相机方向
4. 页面一打开即后台**异步预加载所有模型**（~6s），不用等点"启动"
5. 点击「启动摄像头」开始采集
6. 看到目标进入画面 → 自动对齐、推理、上传、入库
7. 采集卡片底部会显示**海拔**和**方位**（如 `海拔 45m · 方向 东北`）

### 4. 批量导入本地照片（一次性建人脸库）

```bash
# 扫描 ~/Photos/婚礼 目录递归所有照片
# 自动：检测人脸 → 提 ArcFace 特征 → 解析 EXIF GPS/海拔/方向/时间 → 逆地理得中文地址 → 入库
./impt.sh "/Users/${USER}/Documents/M_T_X/婚礼" "婚纱"
./impt.sh "/Volumes/Home/MyPhoto/0205婚礼摄影" "婚礼现场"
./impt.sh "/Users/${USER}/Pictures/Photos Library.photoslibrary/" "手机时光"
./impt.sh "/Volumes/Home/Photos Library.photoslibrary/" "时光旅客"
./impt.sh "/Volumes/Home/MyPhoto" "婚礼宾客"
# 只要人脸不要车牌
VI_VIDEO=1 VI_VPLATES=0 ./impt.sh "/Volumes/Home/MyPhoto" "婚礼宾"

# 等价于：./bin/vi importfaces -dir ~/Photos/婚礼 -label 婚礼

# dry-run：只扫描+检测，不上传 Qdrant
./bin/vi importfaces -dir ~/Photos/婚礼 -dry-run

# 强制重新导入（忽略已有 person_id）
./bin/vi importfaces -dir ~/Photos/婚礼 -force
```

### 5. 打标签 + 鸣叫

- 在「实时监测」或「检索」页面，点图片查看详情 → 「打标签」
- 勾选「命中后鸣叫」→ 下次同一人 / 车牌出现时浏览器蜂鸣 2 次
- 也可在「标签」页面直接管理所有标签

### 6. 停止

```bash
./stop.sh
```

---

## ⚙️ 配置

所有参数通过命令行 flag 或环境变量覆盖：

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `VI_ADDR` | `127.0.0.1:8688` | HTTP 监听地址 |
| `VI_DATA` | `data` | 数据目录（图片 / 日志 / 向量） |
| `VI_MODELS` | `models` | 模型目录 |
| `VI_WEB` | `web` | 前端静态文件目录 |
| `VI_QDRANT` | `http://127.0.0.1:6333` | Qdrant 地址 |
| `VI_FACE_COLLECTION` | `vi_faces` | 人脸向量集合名 |
| `VI_PLATE_COLLECTION` | `vi_plates` | 车牌向量集合名 |
| `VI_FACE_THRESHOLD` | `0.45` | 同一人脸余弦相似度阈值 |
| `VI_COOLDOWN_SECONDS` | `60` | 同一身份重复入库冷却时间 |
| `VI_GEOCODE_URL` | 高德 API 或 Open-Meteo 备用 | 逆地理编码接口 |
| `QuietShield` | 空 | 高德 Web Service Key（国内坐标准确）；留空则用 Open-Meteo 免 key |
| `VI_SUMMARY_AT` | `23:50` | 每日总结生成时间 HH:MM |
| `VI_TZ` | `Asia/Shanghai` | 时区 |

命令行 flag 优先级高于环境变量：

```bash
./bin/vi -addr 0.0.0.0:8688 -cooldown 120 -face-threshold 0.5
```

### 启动参数示例

```bash
# 生产环境：监听所有网卡 + 2 分钟冷却 + HTTPS 前置反代
./bin/vi -addr 0.0.0.0:8688 -cooldown 120

# 开发环境：严格判定 + 较短冷却便于测试
./bin/vi -face-threshold 0.5 -cooldown 30
```

---

## 📡 API 参考

### 采集

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/api/capture` | 前端上传一次检测结果（multipart） |

**multipart 字段：**

| 字段 | 类型 | 说明 |
|---|---|---|
| `kind` | string | `face` 或 `plate` |
| `captured_at` | string | ISO 时间戳 |
| `lat`, `lon` | string | 可选，GPS 坐标（WGS-84 浏览器坐标，后端转 GCJ-02 + 逆地理） |
| `altitude` | string | 可选，海拔米数（来自 `navigator.geolocation.coords.altitude`） |
| `heading` | string | 可选，相机方向角度（0=北，90=东；来自 `DeviceOrientationEvent.webkitCompassHeading`） |
| `vector` | string | 人脸时必填，`[float, ...]` JSON（512 维） |
| `gender` / `age_range` / `glasses` | string | 人脸属性（可选） |
| `plate_text` / `plate_conf` | string | 车牌时必填 |
| `image` | file | 裁剪后的小图（≤ 8MB） |

### 健康检查

```
GET /api/health
→ { ok, qdrant, models: {...}, face_threshold, cooldown_seconds, events, labels }
```

### 标签

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/labels` | 列出所有标签 + 计数 + 样例图 |
| `POST` | `/api/labels` | 新建标签（`{name, event_id, beep}`） |
| `PATCH` | `/api/labels/{id}` | 修改鸣叫开关（`{beep}`） |
| `DELETE` | `/api/labels/{id}` | 删除标签 |

### 身份聚合

```
GET /api/identities?kind=face|plate&page=1&q=关键词
→ 同一 person_id 或车牌号的聚合视图（计数 / 首次 / 最近 / 标签 / 样例图）
```

### 检索

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/search/date?date=YYYY-MM-DD` | 按日期分页 |
| `GET` | `/api/search/plate?plate=京A` | 车牌模糊匹配 |
| `GET` | `/api/search/label?name=张三` | 按标签名 |
| `GET` | `/api/search/place?q=中关村` | 按地名 / 街道 |

**所有检索统一返回**：`{ page, page_size: 20, total, has_more, items: [...] }`

### 统计

```
GET /api/stats?from=YYYY-MM-DD&to=YYYY-MM-DD
→ { faces, plates, persons, unique_plates, labeled_events, daily: [{date, faces, plates}] }
```

### 告警

```
GET /api/alerts?since=RFC3339&limit=20
→ 命中过「鸣叫标签」的告警记录
```

### 每日总结

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/api/summary/run?date=YYYY-MM-DD` | 手动触发生成（不传 date 默认今天） |
| `GET` | `/api/summary/list` | 列出所有已生成的 .md |

### 管理

```
POST /api/admin/reset  # 清空所有事件 / 标签 / 告警 / 向量 / 图片
```

---

## 🖥️ 前端页面

| 页面 | 路径 | 功能 |
|---|---|---|
| 实时监测 | `/` | 主视频区 + 状态栏 + 标签 feed + 实时诊断 |
| 标签管理 | `/labels.html` | 所有标签的 CRUD + 计数 + 鸣叫开关 |
| 多维检索 | `/search.html` | 日期 / 车牌 / 标签 / 地名 四个 tab |
| 统计 | `/stats.html` | 区间筛选 + 日度柱状图 + 身份聚合 |

**实时监测页面核心特性：**

- **视频叠加层**：绿色框 = 人脸（带性别/年龄/眼镜标签），蓝色框 = 车牌
- **状态栏**：芯片显示摄像头 / 人脸引擎 / 车牌引擎 / Qdrant / 定位状态
- **诊断行**：实时显示人脸数 / 跟踪数 / 推理队列长度
- **Feed 面板**：最近 30 条采集事件滚动展示
- **状态栏可收起**：桌面端 46px 竖条 / 手机端 42px 横条，localStorage 记忆偏好

---

## 📦 模型说明

### 人脸链路

| 模型 | 文件 | 体积 | 输入 | 输出 |
|---|---|---|---|---|
| FaceLandmarker | `face_landmarker.task` | 3.7 MB | video | 468 关键点 + 检测框 |
| ArcFace R100 FP32 | `arc.onnx` | 130 MB | `[1,112,112,3]` NHWC RGB `(x-127.5)/128` | `[1,512]` 余弦向量 |
| FairFace Gender | `face_gender.onnx` | 83 MB | `[1,3,224,224]` NCHW `(x/127.5-1)` | `[1,2]` male / female |
| FairFace Age | `face_age.onnx` | 83 MB | 同上 | `[1,9]` 0-2…70+ |
| ViT Glasses | `face_glasses.onnx` | 85 MB | 同上 | `[1,2]` yes / no |

### 车牌链路

| 模型 | 文件 | 体积 | 输入 | 输出 |
|---|---|---|---|---|
| HyperLPR 检测 | `plate_det.onnx` | 3.7 MB | `[1,3,640,640]` RGB/255 letterbox | `[1,25200,15]` cxcywh+conf+5关键点 |
| CRNN CTC | `plate_rec.onnx` | 9.8 MB | `[1,3,48,160]` RGB `(x-127.5)/127.5` | `[1,T,78]` CTC logits |
| 字符表 | `chars.json` | — | — | 78 字符（含省份简称 + 字母 + 数字） |

### 推理策略

- **所有模型统一 ORT WASM EP**（proxy 模式，4 线程，COOP/COEP 启用）
- ArcFace(FP32) 在 WASM 上稳定；属性模型 FairFace 有过 WebGPU 输出错误（已禁用）
- **proxy 模式下 Tensor ArrayBuffer 会被 detach** → 每个属性模型独立 `cloneTensor()`
- MediaPipe 默认 **CPU(XNNPACK)**；GPU 委托在 macOS ANGLE/Metal 上会导致 `GL_CONTEXT_LOST_KHR` → 系统崩溃，默认关闭；`?mpgpu=1` 显式开启

### 模型总大小

~400 MB，首次浏览器加载 ~6s。后端通过 `Cache-Control: public, max-age=31536000, immutable` 让浏览器永久缓存。

---

## 🏛️ Qdrant 集合

| 集合 | 类型 | 向量 | 说明 |
|---|---|---|---|
| `vi_faces` | dense | 512 维 Cosine | 每个人脸 → 最近邻匹配 |
| `vi_plates` | sparse | `text`（字符 n-gram） | 同一车牌 → 模糊匹配 |

### Payload 字段

**vi_faces:**
```json
{
  "kind": "face",
  "person_id": "face-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx",
  "gender": "male",           // 可选
  "age_range": "30-39",       // 可选
  "glasses": "yes",           // 可选
  "captured_at": "2026-11-05T10:30:00+08:00",
  "address": "北京市 朝阳区 ...",
  "image": "2026/11/05/face-xxx.jpg",
  "lat": 39.9042,             // GCJ-02 火星坐标（WGS-84 转换后）
  "lon": 116.4074,
  "altitude": 45.2,           // 可选，海拔（米）
  "heading": 45.0             // 可选，相机方向角度（0=北, 90=东）
}
```

**vi_plates:**
```json
{
  "kind": "plate",
  "plate": "京A12345",
  "captured_at": "2026-11-05T10:30:00+08:00",
  "address": "北京市 朝阳区 ...",
  "image": "2026/11/05/plate-xxx.jpg",
  "lat": 39.9042,
  "lon": 116.4074,
  "altitude": 45.2,
  "heading": 45.0
}
```

---

## 🚢 部署

### GitHub Release（推荐的分发方式）

项目内置 `.github/workflows/release.yml`，推送版本 tag 即可自动构建 6 个平台的二进制并发布到 GitHub Release：

```bash
git tag v0.1.0
git push origin v0.1.0
# → Actions 自动跑 6 路交叉编译 + 下载对应 Qdrant + 打包 + 创建 Release
```

**构建矩阵（6 个目标）：**

| GOOS | GOARCH | 产物名 | 体积（strip+trimpath） |
|---|---|---|---|
| linux | amd64 | `vi-linux-amd64.tar.gz` | ~7.2 MB |
| linux | arm64 | `vi-linux-arm64.tar.gz` | ~6.7 MB |
| darwin | amd64 | `vi-darwin-amd64.tar.gz` | ~7.4 MB |
| darwin | arm64 | `vi-darwin-arm64.tar.gz` | ~6.8 MB |
| windows | amd64 | `vi-windows-amd64.zip` | ~7.4 MB |
| windows | arm64 | `vi-windows-arm64.zip` | ~6.7 MB |

**每个平台包内容：**
```
vi-{os}-{arch}/
├── bin/
│   ├── vi[.exe]          # Go 交叉编译产物（CGO_ENABLED=0）
│   └── qdrant[.exe]      # Qdrant v1.12.4 官方 release 对应架构
├── web/                  # 前端静态文件（无构建）
├── scripts/              # 辅助脚本
├── start.sh              # 一键启动
├── stop.sh               # 停止
├── README.md
└── .gitattributes        # 跨平台换行统一
```

**模型包（单独 asset）：** `vi-models-{tag}.tar.gz`（~400 MB），包含 `models/` 全部 ONNX + MediaPipe `.task` 文件。模型文件较大，Release 里单独提供，解压到项目根目录即可。

**构建细节：**
- `CGO_ENABLED=0` 纯 Go 交叉编译（项目零 CGO 依赖，全标准库）
- `-ldflags "-s -w"` 剥离 debug info，二进制体积砍半
- `-trimpath` 从符号表移除构建路径，安全 + 可复现
- `-X main.Version=$TAG` 注入版本号（二进制启动时 `-h` 可见）
- Qdrant 从 GitHub Release 自动下载对应架构（Linux / macOS arm64 实测有官方包）
- Windows arm64 Qdrant 官方暂缺，构建会跳过下载（手动放置即可）

**手动触发：** GitHub Actions → Release workflow → Run workflow（输入 tag 名）

### 本机 / 开发

```bash
./start.sh
# → http://127.0.0.1:8688
```

### 局域网 HTTPS（手机端摄像头必需）

手机浏览器在 HTTP 下属于**非安全上下文**，`getUserMedia()` 会直接被禁用。两种方案：

#### 方案 A：OpenSSL 自签证书 + Go TLS

```bash
cd vi/
openssl req -x509 -newkey rsa:4096 -keyout server.key -out server.crt \
  -days 3650 -nodes -subj "/CN=*" -addext "subjectAltName=DNS:*"
```

修改 `main.go` 的 `ListenAndServe` 为 `ListenAndServeTLS("server.crt", "server.key")`，重新编译。

#### 方案 B：nginx / Caddy 前置反代（推荐）

```nginx
# /etc/nginx/conf.d/vi.conf
server {
    listen 443 ssl http2;
    server_name vi.local;           # 改成你的域名

    ssl_certificate     /path/to/fullchain.pem;
    ssl_certificate_key /path/to/privkey.pem;

    # WebSocket 如果后续扩展需要
    location / {
        proxy_pass http://127.0.0.1:8688;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }
}
```

### systemd 服务（Linux 生产）

```ini
# /etc/systemd/system/vi.service
[Unit]
Description=vi video surveillance
After=network.target

[Service]
Type=simple
WorkingDirectory=/opt/vi
ExecStart=/opt/vi/bin/vi -addr 0.0.0.0:8688 -cooldown 120
Restart=on-failure
RestartSec=5
User=vi

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable vi
sudo systemctl start vi
```

### Docker（vi 本身**不推荐** Docker 化，Qdrant 可独立容器）

vi 后端依赖 gocv（CGO + OpenCV 4），Docker 镜像需额外安装 libopencv-dev，体积大且构建慢。**推荐组合**：

- **vi** → 裸进程 `./start.sh` 或 systemd（见上方）
- **Qdrant** → 如需隔离环境，单独用 Docker 起（vi 检测到 6333 已占用就不会重复拉起）

```bash
# 仅 Qdrant 容器化，vi 裸进程
docker run -d --name vi-qdrant \
  --restart unless-stopped \
  -p 6333:6333 -p 6334:6334 \
  -v /opt/vi/data/qdrant:/qdrant/storage \
  qdrant/qdrant:v1.12.4

# vi 仍用 start.sh / systemd 跑
./start.sh   # → 检测 6333 已就绪，只起 vi
```

> 如果一定要把 vi 也塞进 Docker，需要在基础镜像里装 OpenCV：
> ```dockerfile
> FROM golang:1.22-bookworm AS build
> RUN apt-get update && apt-get install -y libopencv-dev pkg-config
> WORKDIR /src
> COPY go.mod go.sum ./ && go mod download
> COPY . .
> RUN go build -o /out/vi .
> 
> FROM debian:bookworm-slim
> RUN apt-get update && apt-get install -y libopencv-dev ca-certificates tzdata && rm -rf /var/lib/apt/lists/*
> COPY --from=build /out/vi /usr/local/bin/vi
> COPY web /app/web
> COPY models /app/models
> WORKDIR /app
> EXPOSE 8688
> CMD ["vi", "serve", "-addr", "0.0.0.0:8688", "-qdrant", "http://host.docker.internal:6333"]
> ```
> 注意：CGO_ENABLED 必须保留，vi 不是纯 Go 项目。

---

## 🌐 Cloudflare Tunnel 公网暴露（可选）

vi 默认只监听 `127.0.0.1:8688`。要暴露到公网（尤其是手机端摄像头需要 HTTPS），推荐 **Cloudflare Tunnel**——一条出站隧道绕过运营商入站端口封锁，自带 HTTPS 证书。

### 前提

| 项目 | 要求 |
|---|---|
| Cloudflare 账号 | 域名 `51pwn.com` 已托管在 Cloudflare |
| 已创建 Tunnel | `416d8666-6b3c-4232-8fde-a3995eabfa40`（一条隧道多条 ingress） |
| cloudflared 二进制 | `brew install cloudflared` 或官方 release |

### ingress 规则

在 Cloudflare Tunnel 配置文件里追加一条（**不需要为 vi 开新隧道**，跟现有服务共用一条）：

```yaml
# ~/.cloudflared/config.yml（宿主机模式）
# 或 51pwn-docker/cloudflared/config.yml（容器模式）

tunnel: 416d8666-6b3c-4232-8fde-a3995eabfa40
credentials-file: /etc/cloudflared/creds.json   # 容器里路径

ingress:
  # ↓ 新增：vi · 见到你
  - hostname: vi.51pwn.com
    service: http://127.0.0.1:8688       # 宿主机模式
    # service: http://host.docker.internal:8688   # 容器模式（走 host-gateway）
    originRequest:
      connectTimeout: 30s
      httpHostHeader: vi.51pwn.com
  # ↓ 其他现有规则保持不变
  - hostname: tv.51pwn.com
    service: http://host.docker.internal:8080
    ...
  - service: http_status:404
```

### DNS 记录

```bash
# 一条命令：在 Cloudflare DNS 创建 CNAME → 隧道
cloudflared tunnel route dns 416d8666-6b3c-4232-8fde-a3995eabfa40 vi.51pwn.com
# → Added CNAME vi.51pwn.com which will route to this tunnel
```

cloudflared 会自动在 Cloudflare DNS 创建一条 `vi.51pwn.com CNAME -> cfargotunnel.com` 记录。

### Cloudflare Tunnel 运行模式

| 模式 | 适用场景 | 配置路径 | 热重载 |
|---|---|---|---|
| **容器模式**（推荐） | 跟其他服务一起用 docker compose 托管 | `51pwn-docker/cloudflared/config.yml` + `creds.json` | 改完要 `docker compose build cloudflared && up -d` |
| **宿主机进程** | 调试或快速验证 | `~/.cloudflared/config.yml` + `416d...json` | `kill -HUP $(pgrep -f cloudflared)` |

### 验证

```bash
# 本地回环
curl http://127.0.0.1:8688/api/health
# → {"ok":true,"qdrant":true,...}

# 公网（Cloudflare Tunnel 已生效）
curl -s https://vi.51pwn.com/api/health
# → {"ok":true,"qdrant":true,"cooldown_seconds":60,...}
```

### 多服务共存（一条隧道，零冲突）

一条 Cloudflare Tunnel 支持任意多条 ingress。ingress 按 **hostname 精确匹配** + 顺序兜底：

```yaml
ingress:
  - hostname: vi.51pwn.com       # → 127.0.0.1:8688   (vi · 见到你)
  - hostname: tv.51pwn.com       # → 127.0.0.1:8080   (ws-scrcpy)
  - hostname: www.51pwn.com      # → nginx:80         (51pwn 平台)
  - hostname: stock.51pwn.com    # → 127.0.0.1:3000   (strategy-web)
  - hostname: chat.51pwn.com     # → 127.0.0.1:2083   (E2E_chat)
  - service: http_status:404     # 兜底：其他 hostname 返回 404
```

**不会冲突**：每个 hostname 独立匹配，互不影响；尾部 `service: http_status:404` 是必须的 catch-all。

### 与 startAllSs.sh 集成

如果用 `startAllSs.sh` 管理全套服务，vi 已经被纳入：

```bash
cd goSqlite_gorm

# 启动/自愈（vi 在 tmux 会话里自动拉起）
sh startAllSs.sh start
# → [startAllSs] vi 已在 tmux[vi] 运行（8688 + qdrant healthy）
# → [startAllSs] 公网 vi.51pwn.com → http 200

# 状态（vi 独立条目）
sh startAllSs.sh status | grep vi
# [vi tmux]                    session=vi window=0
# [local 8688]                 http=200   http://127.0.0.1:8688
# [public vi]                  http=200   https://vi.51pwn.com

# 健康检查（vi + qdrant 双检查）
sh startAllSs.sh check
# → 本地 /api/health ok=true 且 qdrant=true 才算通过

# 停止（调用 vi/stop.sh → tmux kill → pkill 兜底）
sh startAllSs.sh stop
```

### 故障排查

| 现象 | 可能原因 | 解决 |
|---|---|---|
| 公网 502 | vi 本地没活着 | `curl http://127.0.0.1:8688/api/health`；tmux 看进程 |
| 容器模式 `credentials file not found` | config.yml 里的 `credentials-file` 路径在容器内不存在 | 确认 `docker cp creds.json ...` 或重建镜像时 `COPY` 进去 |
| 容器模式 `host.docker.internal` 不可达 | 缺少 `extra_hosts` | docker-compose.yml 里 cloudflared 服务加 `extra_hosts: ["host.docker.internal:host-gateway"]` |
| 公网 503 / 无连接 | tunnel 连接器全掉了 | `docker logs -f 51pwn-cloudflared` 看是否有 `Registered tunnel connection` |
| 新加的 ingress 规则未生效 | config.yml 没 reload | 宿主机：`kill -HUP $(pgrep -f cloudflared)`；容器：重建镜像 |

---

## ⚠️ 注意事项

### 浏览器要求

| 项目 | 要求 |
|---|---|
| SharedArrayBuffer | COOP=`same-origin` + COEP=`require-corp` 头已自动开启 |
| 安全上下文 | 手机端必须 HTTPS；`127.0.0.1` 被视为安全上下文 |
| ORT WASM 线程 | 跨源隔离时最多 4 线程；否则自动回退单线程（属性模型推理会很慢） |
| 摄像头 | Chrome / Edge / Safari 最新版；Safari 在 iOS 上对 `getUserMedia` 有严格限制 |

### 性能实测（macOS · CPU + proxy + 4 线程）

| 环节 | 耗时 |
|---|---|
| FaceLandmarker detect（10 张） | ~76 ms |
| ArcFace embed（单张） | ~300 ms |
| FairFace gender（单张） | ~250 ms |
| FairFace age（单张） | ~250 ms |
| ViT glasses（单张） | ~300 ms |
| **完整采集入库（含上传）** | ~1.5 s |
| 车牌 detect + recognize | ~2.1 s |

> 稳态吞吐：每 3 秒完成 1 张脸的完整推理 + 上传（推理队列串行，多个 track 排队等待）

### 已知问题

| 问题 | 影响 | 状态 |
|---|---|---|
| MediaPipe GPU delegate 在 macOS ANGLE/Metal 上导致 `GL_CONTEXT_LOST_KHR` | 检测返回 0 张甚至系统崩溃 | 默认禁用，异常时自动重建 CPU 实例 |
| 属性模型 WebGPU 输出错误（logits 全错） | FairFace INT8 必须强制 WASM | 已修复，所有属性模型 EP 固定 `wasm` |
| proxy 模式 Tensor ArrayBuffer 被 detach | 并发推理会炸 | 每个模型独立 `cloneTensor()` |
| 属性模型 WASM 单线程极慢（~8s/张） | 画面卡顿 | proxy + 4 线程后主线程 maxGap < 100 ms |
| `INT8` 动态量化 Conv 层生成 `ConvInteger` | ORT 不支持，直接报错 | 已删除所有 INT8 模型，统一 FP32 |
| **iOS 13+ 方向传感器需要用户授权** | Safari 在用户手势（click）里调用 `DeviceOrientationEvent.requestPermission()` 才能拿到 `webkitCompassHeading` | 首次启动时自动弹窗，允许即可；Android/Chrome 无需授权 |
| **EXIF GPSImgDirection 并非所有相机都写** | 部分卡片机/手机拍的 JPEG 没有方向字段 | 缺失时 `heading` 字段跳过，不影响其他信息入库 |
| **GDPR 保护的位置数据（欧盟用户）** | 部分 iOS Safari 默认不给出 GPS 精确坐标 | 可用定位权限弹窗里手动允许"精确位置"，或降级为城市级逆地理 |

### 存储增长

| 数据 | 增长速率估算 | 清理 |
|---|---|---|
| 人脸裁剪图 | 每张 ~20 KB × 每天几百张 | 手动删除 `data/images/` 下过期目录 |
| 车牌裁剪图 | 每张 ~15 KB | 同上 |
| events.jsonl | 每条 ~200 B | 可截断旧行 |
| Qdrant 向量库 | 每个点 ~2 KB (dense) + payload | 调用 `POST /api/admin/reset` 清库重建 |

### 安全建议

1. 默认 `127.0.0.1:8688` 不对外暴露；公网部署时务必 HTTPS + 防火墙
2. `/api/admin/reset` 当前无鉴权，生产环境应加 token 或 IP 白名单
3. 图片通过 `/media/` 静态服务暴露，若含敏感信息建议加密存储
4. BigDataCloud 逆地理编码免 key 有 QPS 限制，高频场景建议换付费 API

---

## 🔧 故障排查

### 常见错误

| 错误 | 可能原因 | 解决 |
|---|---|---|
| `models/xxx.onnx 404` | 模型文件缺失 | 检查 `models/` 目录，从项目仓库重新下载 |
| `qdrant 连接失败` | Qdrant 未启动 | `./start.sh` 会自动拉起；或手动 `./bin/qdrant` |
| 摄像头被禁 | 非安全上下文 | 改用 `127.0.0.1` 或配 HTTPS |
| 人脸全返回 0 | GPU 委托崩溃 | 在 URL 加 `?mpgpu=0` 强制 CPU |
| 属性模型报错 | INT8 不兼容 | 确认 `face_gender.onnx` / `face_age.onnx` 是 FP32 版本 |
| `ArrayBuffer detached` | proxy 并发推理 | 每个模型 cloneTensor()，见 `face.js attributes()` |

### 日志位置

| 日志 | 路径 |
|---|---|
| vi 后端 | `data/vi.log`（若指定）或 stdout |
| Qdrant | `data/qdrant.log` |
| 事件 | `data/events.jsonl`（每行 JSON） |
| 告警 | `data/alerts.jsonl` |
| 每日总结 | `data/reports/summary-YYYY-MM-DD.md` |

---

## 📅 路线图（可选改进）

- [ ] HTTPS / WSS 部署脚本 + Caddyfile
- [ ] 属性模型 Worker 内并行推理（当前串行 ~8s/张）
- [ ] 视频流录制 + 回放（Go 后端保存 WebM）
- [ ] 多摄像头支持（后端多路采集）
- [ ] TensorRT / OpenVINO 推理后端替换（生产环境加速）
- [ ] 按日期范围选择性清库（`DELETE /api/admin/events?before=2026-11-06`）
- [ ] 前端 WebCodecs API 替代 canvas 预处理
- [ ] 告警 WebSocket 推送 + 邮件 / 飞书通知

---

## 📄 License

本项目按实际情况声明。第三方模型（ArcFace、FairFace、MediaPipe、HyperLPR）使用时请遵守各自许可证。
