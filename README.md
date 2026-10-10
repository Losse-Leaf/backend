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
- **安全鉴权**：SQLite 管理员母密钥（`site_configs.master_passcode` SHA-256 哈希）与受信任设备子钥匙配对组（`devices` 表 Bearer / `X-Device-Token`）保护写操作与私密财务数据，无需配置 `.env` 密钥环境变量

---

## 核心 API 概览

| 模块 | 方法 | 端点 | 说明 |
| :--- | :--- | :--- | :--- |
| **健康探针** | `GET` | `/api/health` | 检查运行时状态与存储连通性 |
| **系统装帧** | `GET` | `/api/v1/system/status` | 系统初始化与管理员密钥设置状态探针 |
| | `POST` | `/api/v1/system/bootstrap` | 首次装帧双次设密并签发首台设备配对码 |
| **设备配对** | `POST` | `/api/v1/auth/pair/confirm` | 新设备握手确认并激活系统初始化状态 |
| | `GET` | `/api/v1/auth/pair/status` | 轮询配对工单与装帧激活状态 (`?ticket=...`) |
| | `POST` | `/api/v1/auth/verify` | 日常欢迎扉页输入管理员密钥登录并签发设备凭证 |
| **生活档案** | `GET` | `/api/v1/entries` | 分页时间流列表 (支持类型/年份/增量游标/ETag) |
| | `POST` | `/api/v1/entries` | 新建生活记录 (需鉴权) |
| | `PUT` | `/api/v1/entries/:id` | 修改生活记录 (需鉴权) |
| | `DELETE` | `/api/v1/entries/:id` | 删除记录 (需鉴权，支持软删除与物理清理) |
| **独立记账** | `GET` | `/api/v1/transactions` | 月度记账明细列表 (需鉴权) |
| | `GET` | `/api/v1/transactions/summary` | 月度收支概览与类目光谱数据 (需鉴权) |
| | `POST` | `/api/v1/transactions` | 记一笔账 (需鉴权) |
| **项目经历** | `GET` | `/api/v1/projects` | 造物经历列表 |
| **旅游足迹** | `GET` | `/api/v1/footprints/atlas` | 中国市级点亮地图聚合数据 |
| | `GET` | `/api/v1/footprints/cities/:city_code/drawer` | 城市回忆抽屉四重聚合数据 (访客自动脱敏账单) |
| **三维心愿** | `GET` | `/api/v1/wishes` | 心愿单列表 (`item` / `travel` / `milestone`，访客自动脱敏实付金额) |
| | `POST` | `/api/v1/wishes/:id/resolve` | 心愿圆满归册事务接口 (联动记账/时间流/足迹) |
| **生活年鉴** | `GET` | `/api/v1/almanac/summary` | 年度生活年鉴五幕聚合数据 (访客自动脱敏收支) |
| **媒体上传** | `POST` | `/api/v1/files/upload` | 统一文件上传与对象存储引擎 (需鉴权) |

---

## 本地开发与启动

```bash
# 1. 复制环境变量 (可选，仅需配置 R2 存储时填写，无需配置任何密钥)
cp .env.example .env

# 2. 运行测试
go test -v ./...

# 3. 本地启动服务 (默认端口 8088)
go run main.go
```

### 忘记管理员密钥的 SQLite 重置指南

若主理人遗忘在首次装帧时设定的管理员密钥，只需在服务器或本地终端执行以下 SQLite 指令重置初始化状态与口令哈希（**所有手账、账单、心愿单、足迹等业务数据 100% 完好无损**）：

```bash
sqlite3 data/looseleaf.db "UPDATE site_configs SET initialized = 0, master_passcode = '';"
```

执行后刷新浏览器即可重新进入「首次装帧」向导设定新密钥并绑定设备。


## 生产编译与单二进制分发 (Single Binary & Embed)

根据 `docs/ecosystem.md` §十.4 规范，后端原生支持 `//go:embed all:web/dist` 将 Web 静态展厅打入单一二进制产物中，做到**去 Docker 化、零环境依赖极简分发**。

### 1. 编译静态单二进制产物

在编译前，只需将前端构建产物放至 `web/dist`（或依赖 GitHub Actions 自动拉取与注入）：

```bash
# 本地编译 (macOS / 当前系统，自动内嵌 web/dist)
CGO_ENABLED=0 go build -ldflags="-s -w" -o looseleaf-server .

# 交叉编译为 Linux (64位 x86 服务器)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o looseleaf-linux-amd64 .

# 交叉编译为 Linux ARM (如部分云服务器或树莓派)
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o looseleaf-linux-arm64 .
```

编译出的单文件约 15~20MB，内嵌完整 Web 展厅与 API 核心，无需安装 Node.js、Nginx 或 Docker，直接运行即可。

### 2. CI/CD 质量门禁与自动发布流水线

- **质量门禁 (CI)**：每次提交触发 `.github/workflows/ci.yml`，执行 `go mod verify`、`go vet ./...`、`go test -v ./...` 及零 CGO 编译校验；
- **全架构发布 (Release)**：推送 `v*` 标签时触发 `.github/workflows/release.yml`，自动解压前端 Release 包并编译 Linux (amd64/arm64)、macOS (Intel/Apple Silicon) 与 Windows 产物及 SHA-256 校验清单挂载至 Release。

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
