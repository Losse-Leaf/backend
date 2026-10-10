# 《散页》 · Loose Leaf Backend Core

> **Living Archive API Hub · 统一数据中枢与 R2 媒体签名引擎**
>
> 组织主页: [https://github.com/Losse-Leaf](https://github.com/Losse-Leaf)  
> 线上服务: [https://codeactivityhub.top](https://codeactivityhub.top)

---

## 架构概览

本仓库是《散页》生态链的**单一事实来源 (Single Source of Truth)**，采用纯 Go 编写，支撑 Web 展厅端、macOS 桌面端与 Android 移动端的核心数据存取。

- **语言与框架**：Go 1.24+ / Gin Web Framework / GORM
- **核心数据库**：SQLite3（开启 WAL 预写日志与读写并发优化）
- **对象存储**：Cloudflare R2（S3 兼容协议，零出口流量费）与本地磁盘直存双轨支持
- **安全鉴权**：`ADMIN_API_KEY` 与受信任设备配对组 Bearer 鉴权保护写操作

---

## 核心 API 概览

| 模块 | 方法 | 端点 | 说明 |
| :--- | :--- | :--- | :--- |
| **健康探针** | `GET` | `/api/health` | 检查运行时状态与存储连通性 |
| **生活档案** | `GET` | `/api/v1/entries` | 分页时间流列表 (支持类型/年份/增量游标/ETag) |
| | `POST` | `/api/v1/entries` | 新建生活记录 (需鉴权) |
| | `PUT` | `/api/v1/entries/:id` | 修改生活记录 (需鉴权) |
| | `DELETE` | `/api/v1/entries/:id` | 删除记录 (需鉴权，支持软删除与物理清理) |
| **独立记账** | `GET` | `/api/v1/transactions` | 月度记账明细列表 |
| | `GET` | `/api/v1/transactions/summary` | 月度收支概览与类目光谱数据 |
| | `POST` | `/api/v1/transactions` | 记一笔账 (需鉴权) |
| **项目经历** | `GET` | `/api/v1/projects` | 造物经历列表 |
| **旅游足迹** | `GET` | `/api/v1/footprints/atlas` | 中国市级点亮地图聚合数据 |
| | `GET` | `/api/v1/footprints/cities/:city_code/drawer` | 城市回忆抽屉四重聚合数据 |
| **三维心愿** | `GET` | `/api/v1/wishes` | 心愿单列表 (`item` / `travel` / `milestone`) |
| | `POST` | `/api/v1/wishes/:id/resolve` | 心愿圆满归册事务接口 (联动记账/时间流/足迹) |
| **生活年鉴** | `GET` | `/api/v1/almanac/summary` | 年度生活年鉴五幕聚合数据 |
| **媒体上传** | `POST` | `/api/v1/files/upload` | 统一文件上传与对象存储引擎 |

---

## 本地开发与启动

```bash
# 1. 复制环境变量
cp .env.example .env

# 2. 运行测试
go test -v ./...

# 3. 本地启动服务 (默认端口 8088)
go run main.go
```

## 生产编译与单二进制部署

Go 语言最大的优势之一就是编译为**零外部依赖、CGO-Free 的单个静态可执行文件**，无需安装 Docker 或配置复杂的运行环境。

### 1. 编译静态二进制

```bash
# 本地编译 (macOS / 当前系统)
go build -ldflags="-s -w" -o server main.go

# 交叉编译为 Linux (64位 x86 服务器)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o server main.go

# 交叉编译为 Linux ARM (如部分云服务器或树莓派)
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o server main.go
```

编译出的 `server` 单文件约 15~20MB，直接通过 `scp` 或 `rsync` 丢到服务器上即可。

### 2. 服务器 Systemd 守护进程 (推荐)

在 Linux 服务器创建服务配置文件 `/etc/systemd/system/loose-leaf.service`：

```ini
[Unit]
Description=Loose Leaf Backend API Service
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/var/www/loose-leaf/backend
ExecStart=/var/www/loose-leaf/backend/server
Restart=always
RestartSec=3
Environment=PORT=8088
Environment=GIN_MODE=release

[Install]
WantedBy=multi-user.target
```

启动与管理：

```bash
# 重新加载服务配置并开机自启
sudo systemctl daemon-reload
sudo systemctl enable loose-leaf
sudo systemctl start loose-leaf

# 查看运行状态与日志
sudo systemctl status loose-leaf
journalctl -u loose-leaf -f
```
