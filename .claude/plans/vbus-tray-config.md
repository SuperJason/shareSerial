# 托盘菜单中查看/修改 FTDI VBUS 配置

## 目标

在系统托盘右键菜单中直接查看并修改 `ftdi_enabled` / `ftdi_active_low` / `ftdi_cbus_pin`（以及查看 `ftdi_serial` 的当前值），修改后立即保存到配置文件并重启桥接生效，不必再手改 JSON 或进 Web UI。

## 现状

- `ftdiManager` 只在 `newBridgeApp`（main.go:385）启动时按配置创建一次；`UpdateConfig`（webui.go:107）只改内存并写文件，不重建 app。
- 托盘菜单（tray.go）已有 VBUS 状态/ON/OFF 三项，但配置只能通过 Web UI 或手改 JSON。
- systray v1.2.2 支持子菜单：`(item *MenuItem).AddSubMenuItem / AddSubMenuItemCheckbox`，以及 `Check()/Uncheck()` 复选状态。

## 改动

### 1. `cmd/bridge/webui_vbus.go` — 控制器层新增方法

- `FTDIConfigView() (enabled, activeLow bool, pin int, serial string, hubMode bool)`：锁内读 `c.cfg` 与运行状态返回。
- `ApplyFTDIConfig(mutate func(*bridgeConfig))`：
  1. 锁内拷贝 `c.cfg`，执行 `mutate`（改 enabled/activeLow/pin 之一）；
  2. `cfg.Validate()` + `saveConfig(c.cfgPath, cfg)`（写回 JSON）；
  3. 更新 `c.cfg`，然后 `Restart()` 重建 bridgeApp（ftdiManager 随之按新配置创建）；
  4. 失败记日志并返回错误（`c.lastErr` 已有通路显示在状态里）。

### 2. `cmd/bridge/tray.go` — 新增「VBUS 配置」子菜单

菜单结构（放在 VBUS ON/OFF 之后、分隔线处）：

```
DUT VBUS: <状态>
插入 DUT USB (VBUS ON)
拔出 DUT USB (VBUS OFF)
───
VBUS 配置 ▸            （顶层父项，Disable 使其不可点但有子菜单）
  ☑ 启用 VBUS 控制      （ftdi_enabled 复选）
  ☑ 低电平有效          （ftdi_active_low 复选）
  CBUS 引脚: 0          （点击循环 0→1→2→3）
  序列号: 自动检测/XXXX  （只读状态项，Disable；提示在配置文件中改）
```

- 复选项用 `AddSubMenuItemCheckbox`，点击后调 `ApplyFTDIConfig` 立即生效并重启桥接（约几秒中断，与 VBUS 切换本身的中断同量级，日志中已有说明）。
- 引脚项点击后 `pin = (pin+1) % 4` 再应用。
- `updateTrayVBUS`（2 秒轮询）同时刷新子菜单的勾选状态与标题，保证外部（Web UI/手改文件后重启）变更也能反映。
- 事件循环 `trayEventLoop` 增加 4 个 `ClickedCh` case。

### 3. 不改的部分

- `newBridgeApp`、`ftdiManager` 内部逻辑不动；`ftdi_serial` 仍只能通过配置文件/Web UI 修改（托盘无文本输入）。
- 现有 VBUS ON/OFF 菜单与 Web UI 行为不变。

## 验证

- `go build ./cmd/bridge`、`go test ./cmd/bridge`（现有 ftdi_test/hub4com_test 不受影响）。
- 运行 `-tray` 手动验证：勾选/取消「启用 VBUS 控制」后菜单在重启完成 2 秒内变为可用；JSON 文件同步更新。
