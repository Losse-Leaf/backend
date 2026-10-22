# 生产部署件（VPS · 单二进制自更新）

本目录是**生产机上的真实部署件**，全部纳入版本控制 ——
原先它们只活在 VPS 的 `/home/ubuntu/looseleaf/` 里，既无法评审也无法复现
（机器一旦重建就没了）。

## 文件

| 文件 | 说明 |
| :--- | :--- |
| `update.sh` | 自动版本检测与零停机热更脚本（`looseleaf-updater.timer` 每 6 小时拉起一次） |
| `units/looseleaf.service` | 后端服务单元（`User=ubuntu`，`Restart=always`） |
| `units/looseleaf-updater.service` | 更新器单元（oneshot） |
| `units/looseleaf-updater.timer` | 更新器定时器（每 6 小时） |

## 安装到生产机

```bash
# 1. 目录与脚本
sudo mkdir -p /home/ubuntu/looseleaf
sudo cp scripts/deploy/update.sh /home/ubuntu/looseleaf/update.sh
sudo chown ubuntu:ubuntu /home/ubuntu/looseleaf/update.sh
sudo chmod 0755 /home/ubuntu/looseleaf/update.sh

# 2. systemd 单元
sudo cp scripts/deploy/units/looseleaf*.service /etc/systemd/system/
sudo cp scripts/deploy/units/looseleaf-updater.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now looseleaf.service
sudo systemctl enable --now looseleaf-updater.timer
```

## 供应链安全（重要）

`update.sh` 会从**三个第三方 GitHub 代理**下载二进制：

```
https://gh-proxy.com/  https://ghfast.top/  https://ghproxy.net/
```

这些是免费、无声誉保证的第三方中转。下载物会被 `chmod +x` 并由 systemd 以服务身份拉起，
而该账号在本机有**免密 sudo** ⇒ 一个被投毒的代理就能拿下 root。

因此脚本强制执行：

1. **校验和只从可信源取** —— `checksums.txt` 经 **github.com 直连**下载
   （备用 `api.github.com`），**绝不走代理**；
2. **每个下载的二进制都必须通过 sha256 校验** —— 不通过就立刻删除、换下一个节点；
3. 所有节点都失败 ⇒ **保持当前版本运行**、退出码非 0，
   绝不拿未经验证的产物上生产。

代理可以喂错二进制，但喂不出**通过 sha256 校验**的二进制。

> 2026-10-22 加固前，脚本只校验「文件大小 ≥ 20MB」，完全不验 sha256 ——
> 而 Release 明明发布了 `checksums.txt`。详见总台账 `BE-44`。
