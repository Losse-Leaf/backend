# 《散页》 · Loose Leaf Backend Core

> **Living Archive API Hub · 统一数据中枢与 R2 媒体签名引擎**
>
> 组织主页: [https://github.com/Losse-Leaf](https://github.com/Losse-Leaf)  
> 线上服务: [https://codeactivityhub.top](https://codeactivityhub.top)  
> 备案号: [浙ICP备2026081664号](https://beian.miit.gov.cn/)

---

## 架构概览

本仓库是《散页》生态链的**单一事实来源 (Single Source of Truth)**，采用纯 Go 编写，支撑 Web 展厅端、macOS 桌面端与 Android 移动端的核心数据存取。

- **语言与框架**：Go 1.24+ / Gin Web Framework / GORM
- **核心数据库**：SQLite3（开启 WAL 预写日志与单写并发优化）
- **对象存储**：Cloudflare R2（S3 兼容协议，零出口流量费，客户端直传机制）
- **安全鉴权**：`ADMIN_API_KEY` Bearer 鉴权保护写操作

---

## 核心 API 概览

| 模块 | 方法 | 端点 | 说明 |
| :--- | :--- | :--- | :--- |
| **健康探针** | `GET` | `/api/health` | 检查运行时状态与存储连通性 |
| **生活档案** | `GET` | `/api/v1/entries` | 分页时间流列表 (支持类型/年份筛选) |
| | `POST` | `/api/v1/entries` | 新建生活记录 (需 AdminKey) |
| | `PUT` | `/api/v1/entries/:id` | 修改生活记录 (需 AdminKey) |
| | `DELETE` | `/api/v1/entries/:id` | 删除记录 (需 AdminKey) |
| **独立记账** | `GET` | `/api/v1/transactions` | 月度记账明细列表 |
| | `GET` | `/api/v1/transactions/summary` | 月度收支概览与类目光谱数据 |
| | `POST` | `/api/v1/transactions` | 记一笔账 (需 AdminKey) |
| **项目经历** | `GET` | `/api/v1/projects` | 造物经历列表 |
| **当下状态** | `GET` | `/api/now` | 「此刻的我」当前状态 |
| | `PUT` | `/api/now` | 更新当下状态 (需 AdminKey) |
| **存储直传** | `POST` | `/api/v1/uploads/presign` | 生成 R2 上传临时预签名 URL |

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

## Docker 容器化部署

```bash
# 使用 Docker Compose 一键启动
docker compose up -d --build
```
