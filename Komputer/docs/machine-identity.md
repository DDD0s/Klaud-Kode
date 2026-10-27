# 机器身份：把这台电脑的特征搬到大脑上

目标一句话：**不是伪造身份，而是让大脑里的 CLI 报告这台真实电脑的身份。**
本机的硬件特征被采集出来，搬去覆盖 Linux 服务器自己的特征；网络出口保持不变（VPS），
不经过本机。

出口为什么不搬：部分本机所在网络无法直连上游，甚至有封号风险，所以出口问题与身份问题
必须分开处理。身份只管"CLI 认为自己是哪台电脑"，不管它从哪个 IP 说话。

## 组成

四段，全部在同一个二进制 `kloud-kode` 里：

| 段 | 在哪里运行 | 做什么 |
|---|---|---|
| `identity capture` | 本机（Windows / Linux / macOS） | 读真实机信息，写 `machine-identity.json` |
| Body `GET /identity` | 本机（Body 进程） | 带 Body token 时返回同一份档案，供大脑拉取 |
| `identity apply` | VM 内，或 `--root` 离线写 rootfs | 覆盖 VM 里的 hostname、machine-id、时区、locale、用户 |
| `identity qemu` | 大脑宿主机 | 用同一份档案生成 QEMU 身份参数和 guest.env 种子 |

## 采集了什么

hostname、machine-id（Windows 用 MachineGuid 转 32 位 hex）、SMBIOS（整机厂商/型号/版本/
序列号/UUID/family、主板厂商/型号/序列号）、CPU 型号与核心数、内存、磁盘（型号/序列号/容量）、
网卡 MAC 与说明、GPU、OS 名称/版本/架构、时区（IANA + 原始 Windows 名 + UTC 偏移）、
locale、用户名与 home。

固件占位值（如 "To be filled by O.E.M."、"System Product Name"、全 0/全 F）会被剔除：
把共享占位符写进 VM 比留空更糟。

## 如何使用

在你自己的电脑上采集：

```powershell
kloud-kode identity capture --out machine-identity.json
kloud-kode identity show        # 打码汇总，不打印完整序列号
```

> `machine-identity.json` 含真实硬件序列号，按凭据对待：文件权限为 0600，不要提交进仓库。

如果电脑上开着 Body，大脑也可以主动拉（token 必须一致）：

```bash
curl -H "Authorization: Bearer $KLOUD_KODE_BODY_TOKEN" http://<本机内网地址>:8787/identity
```

在大脑上生成并应用：

```bash
# QEMU 身份参数（uuid / smbios / vCPU / 内存）+ guest.env
kloud-kode identity qemu --profile machine-identity.json

# VM 模板内（root 权限）；先 dry-run 看计划
sudo kloud-kode identity apply --profile machine-identity.json --dry-run
sudo kloud-kode identity apply --profile machine-identity.json --create-user

# 也可以在宿主机上先写入离线 rootfs
kloud-kode identity apply --profile machine-identity.json --root ./vm-rootfs --create-user
```

## 覆盖到 / 覆盖不到的范围

覆盖的：`hostname(1)`、`/etc/machine-id` 与 dbus 同步副本、`/etc/localtime`、locale、
`/etc/hosts` 中的本机名、同名用户账户、SMBIOS（厂商、型号、序列号、UUID）/vCPU 数/内存（QEMU）。

覆盖不到的：
- **CPU 型号字符串**：TCG 下 `lscpu` 读到的是 QEMU 模型名。用 `-cpu host` 只会等于宿主机；
  仅当宿主机 CPU 与本级相同时才准确。
- **磁盘序列号与网卡 MAC**：需要在你现有的 drive/netdev 行上添加（`identity qemu` 把值导出在 guest.env 里），
  生成器不凭空拼一台整车出来。
- **虚拟化痕迹**：virtio 设备、hypervisor CPUID、没有电池/TPM/传感器等仍可被识出；
  这是不靠提示词说服、但也不能靠这层身份解决的部分。
- **出口地理位置**：时区可以抄本机，出口 IP 仍是 VPS。如果 IP 所在地与时区差好几个时区，
  建议时区改跟出口走，避免"地理自相矛盾"——这一项取舍取决于你，不由工具替你决定。

## 与 vm2api 的区别

vm2api 的身份来自一个虚构机型库的随机抽样再固定（且其生成器闭源、许可证为非商用），
且它要解决的是订阅号池的风控。我们这边不抽样：**采集本机，搬过去盖掉**。技术通道相同
（QEMU 参数 + 种子文件 + 客机内写系统事实），来源不同。
