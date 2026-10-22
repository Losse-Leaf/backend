#!/usr/bin/env bash
# ==============================================================================
# Loose-Leaf 自动版本检测与极速热更脚本 (Zero-Downtime Safe Auto-Updater)
# ==============================================================================
#
# 安全要点（2026-10-22 加固）：
#
#   二进制本体从**第三方 GitHub 代理**下载（gh-proxy / ghfast / ghproxy）——
#   这些是免费、无声誉保证的第三方中转，只要其中任意一个被劫持或投毒，
#   就能把**任意二进制**塞到这台机器上。
#   而本脚本会把下载物 `chmod +x` 并让 systemd 以服务身份拉起，
#   该账号在本机有**免密 sudo** ⇒ 等于把 root 交出去。
#
#   所以：**校验和必须从可信源取**（github.com 直连，不走代理），
#   二进制本体才可以从代理走（代理只负责「快」）。
#   代理可以喂错二进制，但喂不出**通过 sha256 校验**的二进制。
#
#   校验失败 ⇒ 立即删除、换下一个节点；全部节点都失败 ⇒ 保持当前版本运行、
#   退出码非 0，绝不拿未经验证的东西上生产。
#
# 配套：
#   - systemd 单元：looseleaf-updater.service / looseleaf-updater.timer
#   - 配套的 systemd 单元文件见同目录 unit 文件
# ==============================================================================
set -euo pipefail

INSTALL_DIR="/home/ubuntu/looseleaf"
BINARY_NAME="looseleaf"
SERVICE_NAME="looseleaf.service"
CURRENT_VER_FILE="${INSTALL_DIR}/.current_version"
LOG_FILE="${INSTALL_DIR}/updater.log"
REPO="Losse-Leaf/backend"

log() {
    echo "[$(date "+%Y-%m-%d %H:%M:%S")] $*" | tee -a "${LOG_FILE}"
}

cd "${INSTALL_DIR}"

# 1. 检测系统架构
ARCH=$(uname -m)
case "$ARCH" in
    x86_64) TARGET="amd64" ;;
    aarch64|arm64) TARGET="arm64" ;;
    *) log "❌ 不支持的硬件架构: $ARCH"; exit 1 ;;
esac
ASSET_NAME="${BINARY_NAME}-linux-${TARGET}"

# 2. 探测云端 GitHub 最新 Release 版本号 (多镜像源毫秒级嗅探)
log "🔍 正在检查云端最新 Release 版本..."
LATEST_TAG=""

if command -v jq >/dev/null 2>&1; then
    LATEST_TAG=$(curl -s --connect-timeout 4 "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null | jq -r .tag_name 2>/dev/null || true)
fi

if [ -z "$LATEST_TAG" ] || [ "$LATEST_TAG" = "null" ]; then
    LATEST_TAG=$(curl -sIL --connect-timeout 5 "https://gh-proxy.com/https://github.com/${REPO}/releases/latest" 2>/dev/null | grep -i "location:" | tail -n 1 | awk -F "/" "{print \$NF}" | tr -d "\r\n " || true)
fi

if [ -z "$LATEST_TAG" ] || [ "$LATEST_TAG" = "null" ]; then
    LATEST_TAG=$(curl -sIL --connect-timeout 5 "https://ghfast.top/https://github.com/${REPO}/releases/latest" 2>/dev/null | grep -i "location:" | tail -n 1 | awk -F "/" "{print \$NF}" | tr -d "\r\n " || true)
fi

if [ -z "$LATEST_TAG" ] || [ "$LATEST_TAG" = "null" ]; then
    log "⚠️ 未能获取到云端版本号，跳过更新。"
    exit 0
fi

# 3. 读取本地当前运行版本
CURRENT_TAG=""
if [ -f "$CURRENT_VER_FILE" ]; then
    CURRENT_TAG=$(cat "$CURRENT_VER_FILE" | tr -d "\r\n ")
fi

log "云端最新版本: ${LATEST_TAG} | 本地当前版本: ${CURRENT_TAG:-未知}"

if [ -n "$CURRENT_TAG" ] && [ "$CURRENT_TAG" = "$LATEST_TAG" ]; then
    log "✅ 当前已是最新版本 (${CURRENT_TAG})，无需更新。"
    exit 0
fi

# ---------------------------------------------------------------------------
# 4. 取期望的 sha256 —— **只从可信源取**（github.com 直连 / api.github.com），
#    绝不从下面的代理镜像取。
# ---------------------------------------------------------------------------
log "🔐 正在从 GitHub 直连获取 ${LATEST_TAG} 的校验和..."
EXPECTED_SHA=""
CHECKSUM_URL="https://github.com/${REPO}/releases/download/${LATEST_TAG}/checksums.txt"
if curl -fsL --connect-timeout 8 --max-time 40 "${CHECKSUM_URL}" -o /tmp/looseleaf-checksums.txt 2>/dev/null; then
    EXPECTED_SHA=$(grep -E "[[:space:]]${ASSET_NAME}\$" /tmp/looseleaf-checksums.txt 2>/dev/null | awk '{print $1}' | head -n 1 || true)
fi

if [ -z "$EXPECTED_SHA" ]; then
    # 备用：api.github.com 的资产下载端点（同样是 GitHub 直连）
    if curl -fsL --connect-timeout 8 --max-time 40 \
        "https://api.github.com/repos/${REPO}/releases/tags/${LATEST_TAG}" \
        -o /tmp/looseleaf-release.json 2>/dev/null; then
        if command -v jq >/dev/null 2>&1; then
            ASSET_URL=$(jq -r --arg n "${ASSET_NAME}" '.assets[] | select(.name == $n) | .browser_download_url' /tmp/looseleaf-release.json 2>/dev/null | head -n 1 || true)
            if [ -n "$ASSET_URL" ] && [ "$ASSET_URL" != "null" ]; then
                curl -fsL --connect-timeout 8 --max-time 40 "${ASSET_URL}" -o /tmp/looseleaf-checksums.txt 2>/dev/null || true
                EXPECTED_SHA=$(grep -E "[[:space:]]${ASSET_NAME}\$" /tmp/looseleaf-checksums.txt 2>/dev/null | awk '{print $1}' | head -n 1 || true)
            fi
        fi
    fi
fi

if [ -z "$EXPECTED_SHA" ]; then
    log "❌ 取不到 ${LATEST_TAG} 的 sha256 校验和（trusted source 直连失败）。"
    log "   为避免装上未经验证的二进制，本次**放弃更新**，保持当前版本运行。"
    exit 1
fi
log "🔐 期望 sha256 (${ASSET_NAME}): ${EXPECTED_SHA}"

# 5. 高速镜像队列 (按实测 8.9MB/s 优先级排序，低于 300KB/s 自动秒级切换)
MIRRORS=(
    "https://gh-proxy.com/"
    "https://ghfast.top/"
    "https://ghproxy.net/"
)

TEMP_BIN="${INSTALL_DIR}/looseleaf.new"
rm -f "${TEMP_BIN}"
DOWNLOAD_SUCCESS=false

for mirror in "${MIRRORS[@]}"; do
    TARGET_URL="${mirror}https://github.com/${REPO}/releases/download/${LATEST_TAG}/${ASSET_NAME}"
    log "⚡ 正在尝试节点: ${mirror}"
    if curl -fSL --progress-bar --connect-timeout 5 --speed-limit 300000 --speed-time 6 --max-time 120 "${TARGET_URL}" -o "${TEMP_BIN}"; then
        # 校验下载完整性 (单二进制应 >= 25MB)
        FILE_SIZE=$(wc -c < "${TEMP_BIN}" 2>/dev/null || echo 0)
        if [ "$FILE_SIZE" -lt 20000000 ]; then
            log "⚠️ 下载文件残缺 (${FILE_SIZE} 字节)，切换下一节点..."
            rm -f "${TEMP_BIN}"
            continue
        fi

        # ── 关键：sha256 校验 ─────────────────────────────────────
        # 代理节点可能返回**被篡改**的二进制（节点被劫持/投毒/DNS 污染）。
        # 校验和来自 GitHub 直连，代理喂不出通过校验的二进制。
        ACTUAL_SHA=$(sha256sum "${TEMP_BIN}" 2>/dev/null | awk '{print $1}' || true)
        if [ -z "$ACTUAL_SHA" ]; then
            log "⚠️ 无法计算 sha256，切换下一节点..."
            rm -f "${TEMP_BIN}"
            continue
        fi
        if [ "$ACTUAL_SHA" != "$EXPECTED_SHA" ]; then
            log "🚨 sha256 校验不通过！节点 ${mirror} 返回的二进制与 GitHub 官方校验和不符。"
            log "    期望: ${EXPECTED_SHA}"
            log "    实际: ${ACTUAL_SHA}"
            log "    该节点已被丢弃（可能是劫持或投毒），切换下一节点..."
            rm -f "${TEMP_BIN}"
            continue
        fi

        log "✅ 高速下载完成 (大小: $((FILE_SIZE / 1024 / 1024)) MB, sha256 校验通过)"
        DOWNLOAD_SUCCESS=true
        break
    else
        log "⚠️ 节点速度较慢或超时，秒级切换备用节点..."
        rm -f "${TEMP_BIN}"
    fi
done

if [ "$DOWNLOAD_SUCCESS" != "true" ]; then
    log "❌ 所有加速节点均未完成**通过 sha256 校验**的下载，保持当前版本运行。"
    rm -f "${TEMP_BIN}"
    exit 1
fi

chmod +x "${TEMP_BIN}"

# 6. 安全平滑热更并重启 systemd
log "📦 验证通过，正在备份并重启服务..."
if [ -f "${INSTALL_DIR}/${BINARY_NAME}" ]; then
    cp -f "${INSTALL_DIR}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}.bak"
fi

sudo systemctl stop "${SERVICE_NAME}"
mv -f "${TEMP_BIN}" "${INSTALL_DIR}/${BINARY_NAME}"
sudo systemctl start "${SERVICE_NAME}"
sleep 2

# 7. 健康探针验证与自动回滚保护
if curl -sf --connect-timeout 5 http://127.0.0.1:8080/api/health >/dev/null 2>&1; then
    echo "${LATEST_TAG}" > "${CURRENT_VER_FILE}"
    log "🎉 服务已成功秒级升级至 ${LATEST_TAG}！探针检测正常。"
    rm -f "${INSTALL_DIR}/${BINARY_NAME}.bak"
else
    log "⚠️ 新版本探针检测异常，正在自动回滚至旧版本..."
    sudo systemctl stop "${SERVICE_NAME}"
    if [ -f "${INSTALL_DIR}/${BINARY_NAME}.bak" ]; then
        mv -f "${INSTALL_DIR}/${BINARY_NAME}.bak" "${INSTALL_DIR}/${BINARY_NAME}"
        sudo systemctl start "${SERVICE_NAME}"
    fi
    log "❌ 已自动回滚，保留旧版本稳定运行。"
    exit 1
fi
