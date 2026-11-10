# serial-log

单文件串口终端 + TCP 桥接工具，用来替代原来的 com0com / hub4com 组合：它自己独占
串口，把内容渲染成一个真正的终端窗口，同时把同一份字节流镜像到三个 TCP 端口上，
供其它程序或 AI Agent 读写。

```
        ┌──────────────── serial-log.exe ────────────────┐
串口 ──►│ 读线程 → 行分帧 → 环形缓冲 → 终端窗口(WebView2)│
        │                    ├──► 2601 原始双工           │
        │                    ├──► 2602 只读镜像           │
        │                    └──► 2603 帧化               │
        └────────────────────────────────────────────────┘
```

## 特性

- **终端窗口**：xterm.js 渲染，支持 ANSI 颜色、回车覆盖、光标控制；20000 行回滚
- **可拖动的滚动条**：自己画的一条（xterm 原生的被文本层盖住，鼠标点不到）
- **颜色方案**：规则和配色都是 JSON 文件，内置两套，可以自己加任意多套
- **抓取日志**：默认关闭，需要时从右键菜单开启，文件名带串口名和时间
- **三个 TCP 端口**：2601 原始双工、2602 只读镜像、2603 带方向和时间戳的帧化
- **HTTP 读取接口**：`/api/text` 给脚本和 Agent 用，纯文本 + 游标，可以只取增量
- **便携**：配置和颜色方案都在 exe 旁边，整个文件夹拷走就能用

## 构建与运行

```bash
go build -ldflags "-H=windowsgui" -o serial-log.exe ./cmd/bridge
```

`-H=windowsgui` 是必须的：没有它，双击会先弹出一个控制台窗口。构建脚本见 `build.bat`。

```bash
serial-log.exe -p 3 -b 115200 -window
```

| 参数 | 说明 |
|---|---|
| `-p` | 串口号，`3` 或 `COM3` 都行 |
| `-b` | 波特率，默认 115200 |
| `-P` | 校验位 none/odd/even/mark/space |
| `-frame` | 数据位/停止位，如 `8N1` |
| `-wpolicy` | 2601 写策略：`first`（先连上的独占）/ `all` / `readonly` |
| `-window` / `-no-window` | 是否开窗口，默认开 |
| `-console` | 附加（或新建）一个控制台，让横幅和日志能在终端里看到；双击时不需要 |
| `-demo` | 不连串口，输出演示数据 |
| `-config` | 配置文件路径，默认 exe 同目录的 `serial-log.json` |
| `-capture` | 启动即开始抓取（默认关闭） |
| `-capturedir` | 抓取目录 |

启动横幅会打印串口、三个 TCP 端口、配置文件路径和颜色方案目录：

```
 serial-log 2.0.0
 port    : COM3 @ 115200 8N1
 capture : off (right-click the window to start)
 raw TCP : 2601   tap: 2602   framed: 2603   policy: first
 config  : C:\Tools\serial-log\serial-log.json
 schemes : C:\Tools\serial-log\schemes
 window  : http://127.0.0.1:18080/
```

## 窗口

工具栏只有端口、波特率、数据/校验/停止位、字符集、打开/关闭、状态和 RX/TX 计数，
没有过滤行、没有发送框——键盘输入直接发往串口。设备本身会回显，所以本地不回显，
打 `echo hi` 回车，看到的就和坐在目标机前面一样。

双击启动时只会出现终端窗口。exe 是按 **GUI 子系统**链接的（`-ldflags -H=windowsgui`），
Windows 根本不会给它分配控制台窗口，所以不存在"先弹一个黑窗口"的情况。

代价是命令行输出默认没有去处：横幅、错误信息都会写进运行日志
（`Documents\serial-log\bridge.log`）。想在终端里看到它们：

- 从 cmd / PowerShell / Windows Terminal 启动时带 `-console`，会复用（或新建）一个控制台；
- 启动期致命错误（端口非法、配置错误）会弹对话框提示，不会静默退出。

### 字体

右键菜单的 `Text` 一节列出四种等宽字体（Cascadia Mono / Consolas / JetBrains Mono /
Courier New），正在用的打勾；字号用 `Ctrl+=` / `Ctrl+-` / `Ctrl+0` 或 `Ctrl+滚轮`
调整，范围 9–30。两者都记在窗口本地，下次打开保持。

### 快捷键

| 快捷键 | 作用 |
|---|---|
| `Ctrl+C` | 有选中内容时复制；没有选中时作为中断字符 `^C` 发给设备 |
| `Ctrl+Shift+C` | 强制复制（传统终端习惯） |
| `Ctrl+V` | 粘贴（只粘贴一次） |
| `Ctrl+F` | 打开搜索栏 |
| `Ctrl+=` / `Ctrl+-` | 放大 / 缩小字号 |
| `Ctrl+0` | 恢复默认字号 13 |
| `Ctrl+滚轮` | 放大 / 缩小字号 |

复制出来的永远是纯文本，颜色只存在于显示层，不会混进剪贴板。

### 右键菜单

```
开始/停止抓取
设置抓取目录…
─────────────
搜索…                     Ctrl+F
复制                      Ctrl+C
粘贴                      Ctrl+V
─────────────
颜色
● Color output            WindTerm (Monokai)   ← 开关：实心=开，空心=关；右侧是当前方案
│ ✔ WindTerm (Monokai)                        ← 子项：缩进 + 左侧引导线
│   Simple
│   my-android
─────────────
清屏
```

颜色关闭时，下面的方案列表整体变灰并且点不动。

## 颜色方案

颜色只在**显示层**生效：往显示流里插入 SGR 序列，TCP 端口和抓取文件里的字节一个都
不变（抓到的文件里没有一个 ESC 字节，可以自己验证）。

### 内置方案

| 方案 | 说明 |
|---|---|
| `WindTerm (Monokai)`（默认） | 关键词和配色取自 WindTerm 2.7.0 的 `terminal/schemes/linux/linux.lexer` 与 `global/themes/dige-black/scheme.theme`，包括标点类规则。内核时间戳里的数字是紫色的——它的 lexer 没有专给 `[ 1234.567]` 的规则，数字落到 `constant.numeric`；`:-107` 里的 `107` 不上色——它的数字规则排除了"前面是 `-`、且 `-` 前不是空白"的情况 |
| `Simple` | 安静版：只给错误、警告、成功、时间戳、数字上色 |

### 文件放在哪

```
serial-log.exe
serial-log.json        ← 设置
schemes\
  windterm.json        ← 首次运行由内置默认值写出
  simple.json
  my-android.json      ← 你自己的，文件名随意，数量不限
```

启动横幅里的 `schemes :` 就是实际目录。如果 exe 所在目录不可写（比如装在
`Program Files`），会回退到 `%LOCALAPPDATA%\serial-log\`。

菜单显示的内容**就是那个目录的内容**：

- 每次打开菜单都会重新读一遍目录，新增/删除文件立刻反映，不用重启
- 磁盘上的文件优先于内置副本（同名即覆盖）
- 点了当前正在用的方案，会重新读文件并重绘，改完点一下就能看到效果
- JSON 写坏了不会悄悄消失，菜单里显示 `<文件名> (invalid JSON)`

### 手写一个方案

1. 复制一份 `schemes\simple.json`，改名——**文件名就是方案 id**，例如 `my-android.json`
2. 改 `name`（菜单里显示的名字）和 `rules`
3. 右键菜单里选中它；如果它已经在用，再点一次即重新加载

文件结构：

```json
{
  "name": "My Android",
  "description": "可选，只给人看",
  "rules": [
    { "name": "标签",     "color": "#ff79c6", "pattern": "\\b(?:init|zygote|system_server)\\b" },
    { "name": "启动完成", "color": "#8be9fd", "pattern": "\\b(?:Starting|Started|Boot completed)\\b" },
    { "name": "i2c",      "color": "#66d9ef", "pattern": "i2c_geni" }
  ]
}
```

每条规则的字段：

| 字段 | 必填 | 说明 |
|---|---|---|
| `name` | 否 | 规则名，只用于调试和告警信息 |
| `color` | 是 | `#rrggbb`，按 24 位真彩写入（`38;2;r;g;b`），不受终端 16 色调色板影响 |
| `pattern` | 是 | JavaScript 正则表达式字符串，**反斜杠要写成两个** |
| `flags` | 否 | 默认 `gi`，一般不用改 |

### 正则表达式说明

匹配用的是浏览器的 JavaScript `RegExp`，每来一段新文本按下面的流程处理：

1. 所有规则各自扫一遍文本，收集全部命中
2. 按**起点从左到右**决定归属；起点相同则看**规则在数组里的先后**
3. 已被占用的字符不会被后面的规则改写

这模仿的是词法分析器的扫描顺序，而不是"后面的规则覆盖前面的"。所以
`--no-verify` 会被 option 规则整体吃掉，不会被里面的 `no`（error 关键词）抢走一段。

**支持**：`\b`、`\d`、`\w`、`[]`、`()`、`(?:)`、`(?=)`、`(?<=)`、命名分组，以及
`i`、`g` 等修饰符。

**不支持**（从 PCRE / Onigmo 抄规则时最容易踩的坑）：

| 写法 | 原因 | 改用 |
|---|---|---|
| `(?i)abc` | JS 没有内联修饰符 | 本工具会自动剥掉并加 `i`，但最好直接不写 |
| `\d++`、`\w*+` | JS 没有占有量词 | `\d+`、`\w*` |
| `[[:xdigit:]]` | JS 没有 POSIX 字符类 | `[0-9a-fA-F]` |
| `(?x)` | 不支持扩展模式 | 把注释去掉 |

**JSON 转义坑（最常见的错误）**：JSON 里 `\b` 表示退格符，不是单词边界。写词边界
必须写成 `\\b`；写成 `\b` 时规则会变成"退格 + b"，永远匹配不上——工具会在浏览器
控制台里告警并跳过这条规则。

参考资料：

- MDN 的正则表达式指南（语法、断言、字符类）：<https://developer.mozilla.org/zh-CN/docs/Web/JavaScript/Guide/Regular_expressions>
- regex101，语言选 **JavaScript**，可以逐条试验并看到分组结果：<https://regex101.com/>
- 想照着 WindTerm 的规则改，直接看它安装目录里的
  `terminal/schemes/linux/linux.lexer`（Onigmo 语法，抄的时候注意上面那张表）

## 抓取日志

默认**关闭**——只观察不抓取时一个字节都不落盘。从右键菜单开启后，按
`<串口名>_<日期>_<时间>.log` 建文件（例如 `COM3_20261106_095124.log`），
关闭即停止，一个抓取周期一个文件。

```
2026-11-05 18:08:13.804 RX  COM3       [ 1548.470740] i2c_geni 4a84000.i2c: i2c error :-107
2026-11-05 18:08:20.117 TX  local      echo ui_final_9
2026-11-05 18:08:20.119 RX  COM3       ui_final_9
```

列依次是：时间、方向（RX/TX/SYS）、来源（串口或客户端）、正文。200ms 刷盘，
单文件 64MB 轮转。抓取开关本身不持久化，避免哪天忘了关把磁盘写满；想开机就抓，
用 `-capture` 或在配置里写 `"capture_enabled": true`。

## 搜索

`Ctrl+F` 或右键菜单打开右上角搜索栏：在整段缓冲里搜索，全部高亮，计数显示
`当前/总数`，`↑`/`↓`（或 `Enter`/`Shift+Enter`）逐个跳转。点 `.*` 切换正则模式，
非法表达式显示 `bad pattern`。

## 三个 TCP 端口

| 端口 | 名称 | 方向 | 用途 |
|---|---|---|---|
| 2601 | raw | 双向 | 普通桥接：写入即发往串口，串口数据原样回送 |
| 2602 | tap | 只读 | 旁路监听 RX+TX（带方向标签），永不写串口 |
| 2603 | framed | 双向 | 带方向和时间戳的帧化协议，适合程序解析 |

帧格式（大端）：

```
偏移    长度  字段
0       4     payload 长度，大端
4       1     方向：1 = RX，2 = TX，3 = 系统提示
5       8     到达时间戳，Unix 毫秒，大端
13      n     payload 原始字节（含行结束符）
```

`-wpolicy` 决定 2601 的写权限：`first` 只允许第一个连上的客户端写（它断开后下一个
接管），`all` 谁都能写，`readonly` 一律不能写。

## HTTP API

窗口本身就是一个网页（`http://127.0.0.1:18080/`），所有能力都有接口。只监听回环地址。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/state` | 端口状态、RX/TX 计数、抓取状态、配置 |
| GET | `/api/hello` | 自我描述：这是什么、当前状态、能力清单、读/写示例 |
| GET | `/llms.txt` | 同一份描述的纯文本（Markdown）版 |
| GET | `/api/ports` | 可用串口列表 |
| GET | `/api/records?tail=N` | 最近 N 条记录（JSON，带 seq/ts/dir） |
| GET | `/api/records?after=SEQ&limit=N` | seq 之后的新记录（JSON） |
| **GET** | **`/api/text?tail=N`** | **最近 N 行，纯文本** |
| **GET** | **`/api/text?after=SEQ&limit=N`** | **seq 之后的新行，纯文本** |
| POST | `/api/send` | `{"text":"..."}` 或 `{"hex":"41 42"}` 发往串口 |
| POST | `/api/open`、`/api/close` | 打开 / 关闭串口 |
| POST | `/api/clear` | 清空内存缓冲 |
| POST | `/api/capture` | `{"on":true,"dir":"..."}` 开关抓取；只给 `dir` 就是换目录 |
| GET | `/api/schemes` | 颜色方案列表 |
| GET | `/api/scheme/<id>` | 某个方案的规则 JSON |
| GET | `/api/export?format=txt\|hex` | 导出当前缓冲 |
| POST | `/api/demo?on=true\|false` | 演示数据开关 |

### `/api/text`：给 Agent 读取输出

不用落临时文件，也不用处理 JSON 转义：

```bash
# 最近 200 行
curl -s "http://127.0.0.1:18080/api/text?tail=200"

# 只要新内容：先记下游标，再带上它
NEXT=$(curl -s -D - -o /dev/null "http://127.0.0.1:18080/api/text?tail=1" |
       grep -i '^x-seq-next' | tr -d '\r' | cut -d' ' -f2)
curl -s "http://127.0.0.1:18080/api/text?after=$NEXT&limit=500"
```

响应头里有三个游标：

| 头 | 含义 |
|---|---|
| `X-Seq-Next` | 这次读完之后的下一个游标，**下次 `after=` 传它** |
| `X-Seq-Last` | 当前最后一条的序号 |
| `X-Seq-Oldest` | 缓冲里还保留的最旧序号（比它小的已经被丢弃） |

参数：`ts=1` 每行加 `HH:MM:SS.mmm`，`dir=1` 每行加 `RX`/`TX`/`SYS` 前缀。
默认只给正文。二进制负载（解码不出文本的块）以十六进制串输出，不会把控制字符塞进
纯文本流。

### 给 Agent 的自我描述

Agent 第一次接触这个工具时，一次请求就能拿到全部上下文：

```bash
curl -s http://127.0.0.1:18080/api/hello
```

返回内容：

| 字段 | 内容 |
|---|---|
| `schema` | 结构版本号，字段以后有变动时递增 |
| `summary` / `how_it_works` | 这是什么、数据怎么流动 |
| `state` | 与 `/api/state` 相同：串口开没开、参数、抓取状态、缓冲占用、订阅者 |
| `buffer` | `seq_last` / `seq_oldest` / 条数 / 容量 |
| `transports` | 五条读取路径（两路 HTTP + 三路 TCP）的地址、格式、是否双工 |
| `capabilities` | 能做什么，每条带可直接复制的 `call` 和 `body`；写入类会注明当前写策略 |
| `paths` | exe / 配置文件 / 颜色方案目录 / 抓取目录 / endpoint 文件 |
| `notes` | 限制与坑（颜色只是显示层、缓冲会淘汰、端口关闭时的表现等） |

同样内容还有一份散文版，给更愿意读文本的客户端：

```bash
curl -s http://127.0.0.1:18080/llms.txt
```

两者都是**实时生成**的，`state`、`buffer` 部分永远是当前值，不会过期。

**端口怎么找**：默认 18080。启动时还会在配置文件旁边写一个
`serial-log.endpoint.json`：

```json
{ "service": "serial-log", "web_port": 18080, "version": "2.0.0",
  "url": "http://127.0.0.1:18080/", "hello": "http://127.0.0.1:18080/api/hello",
  "pid": 15792, "started_at": "2026-11-06T11:33:24+08:00" }
```

用 `-web` 换端口（或以后支持随机端口）时读这个文件即可，不必猜；里面的 `pid`
可以用来判断文件是不是上次遗留的。

一个 Agent 的典型开场：

1. `GET /api/hello` → 串口开着、COM3 @ 115200、缓冲里有 431 条、推荐走 `/api/text`
2. `GET /api/text?tail=200` → 看现场
3. 记下响应头 `X-Seq-Next`，之后只 `GET /api/text?after=<cursor>` 取增量

## 配置文件

`serial-log.json`，和 exe 放在一起。命令行参数优先于配置文件，且只有实际传了的
参数才会覆盖：

```json
{
  "com_port": "COM3", "baud_rate": 115200, "data_bits": 8,
  "stop_bits": 1, "parity": "none", "encoding": "utf-8",
  "log_file": "C:\\Users\\<you>\\Documents\\serial-log\\bridge.log",
  "capture_enabled": false, "capture_dir": "C:\\Users\\<you>\\Documents\\serial-log",
  "target_ip": "", "target_port": 2601, "tap_port": 2602, "frame_port": 2603,
  "write_policy": "first", "web_port": 18080,
  "window": true, "auto_reopen": true,
  "line_idle_ms": 100, "max_line_bytes": 8192,
  "max_records": 200000, "max_bytes": 33554432
}
```

`target_ip` 是原始端口的**绑定地址**（`""` = 所有网卡，`127.0.0.1` = 只允许本机）。
HTTP API 永远只绑回环。

## 目录结构

```
serial-log.exe
serial-log.json
schemes\*.json
```

整个文件夹可以随便拷。`.gitignore` 已经把运行期产生的文件排除掉了。

## 实现要点

- 读线程只做"读 → 行分帧 → 入环形缓冲 → 分发"，渲染和 TCP 各走各的队列；
  某一方消费慢就丢它自己的数据，绝不拖住串口读取
- 行分帧：`LF`、裸 `CR`、100ms 空闲、单行 8KB 上限
- 字符集按会话解码（UTF-8 / GBK / Big5 / Shift-JIS…），跨读包被拆开的字符会拼回去
- 串口被占用或拔掉后自动重连（1s → 4s 退避）
- 断线、占用、无权限都会在窗口底部给出提示

## 测试

```bash
go test ./...
```

覆盖分帧、环形缓冲、字符集、总线、应用流程、抓取开关与文件命名、三个 TCP 端口、
HTTP API（含 `/api/text`）、终端输出流。

## 已知限制

- 目标在内核控制台上疯狂打印时，UART 输出路径没有流控，你敲进去的字符可能被打乱；
  本工具发出的就是它记录的字节，可以用抓取的文件核对
- 颜色规则只作用于显示层，不改动 TCP 和抓取文件的内容
- 窗口需要 Edge WebView2 运行时（Windows 11 自带）；没有时回退为只打印 URL
