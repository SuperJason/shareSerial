# serial-log — serial monitor window + TCP sharing

A single-binary Windows tool that opens a serial port, shows the traffic in a
log window, and shares the very same stream with any number of other programs
over TCP — in **both** directions (device output out, commands in).

It replaces the earlier hub4com/com0com-based bridge: no kernel driver, no
virtual COM pairs, no helper process. The tool owns the physical port and is the
single source of truth for the byte stream.

```
                        ┌──────────── log window (WebView2) ── line view / HEX
COM3 ── serial core ──┬─┤
                      │ └──────────── capture files (timestamped txt + framed bin)
                      │
                      ├──────────── :2601 raw     duplex byte stream
                      ├──────────── :2602 tap     read-only mirror (RX + TX)
                      └──────────── :2603 framed  timestamped records, both ways
```

## Quick start

```bash
go build -o serial-log.exe ./cmd/bridge
./serial-log.exe -p 3 -b 115200
```

The window opens on `COM3 @ 115200`. Pick another port/baud in the toolbar and
press **Open** — no restart needed.

Headless (server, CI bench, RDP session):

```bash
./serial-log.exe -p 3 -b 115200 -no-window
# then browse to http://127.0.0.1:18080/
```

No hardware at hand? `./serial-log.exe -demo` feeds a synthetic device through
the exact same pipeline.

## Transports

| Port | Name | Direction | Payload |
|---|---|---|---|
| 2601 | raw | duplex | exactly the bytes on the wire, no framing |
| 2602 | tap | read-only | RX **and** TX, i.e. a mirror of the physical line |
| 2603 | framed | duplex | `[u32 len][u8 dir][u64 ts_ms][payload]` |

Write policy (`-wpolicy`):

- `first` (default) — the first connected raw/framed client owns the write
  side until it disconnects, then the next client may take it. Protects the
  device from two programs interleaving bytes in one command.
- `all` — every client may write (the old bridge behaviour).
- `readonly` — TCP clients can watch but never write.

Tap clients can never write, regardless of policy.

`cmd/tcp_demo` is a console client for both transports:

```bash
go run ./cmd/tcp_demo                # raw, port 2601
go run ./cmd/tcp_demo -framed        # framed, port 2603
```

## Framed protocol (:2603 and the `.bin` capture)

```
offset  size  field
0       4     payload length, big endian
4       1     direction: 1 = RX (device -> host), 2 = TX, 3 = system note
5       8     arrival timestamp, unix milliseconds, big endian
13      n     payload bytes (raw, terminator included)
```

A client that speaks this framing gets direction and timing for free; the raw
port (2601) is for tools that only want the bytes.

## Terminal window

The window is a serial terminal, not a log table: output is rendered as it
arrives (ANSI colours, carriage returns and cursor movement included) and
keystrokes go straight out the port. The device echoes what it receives, so
there is no local echo — typing `echo hi` and pressing Enter looks exactly like
sitting in front of the target's console.

- toolbar: port, baud, data/parity/stop bits, device charset, Open/Close,
  status, RX/TX byte counters — nothing else
- 20 000 lines of scrollback with a draggable scrollbar on the right
- right-click menu: start/stop capture, capture folder, search, copy, paste,
  clear scrollback
- **Ctrl+C** copies when something is selected, otherwise it is sent to the
  device as the interrupt character (^C); **Ctrl+V** pastes; Ctrl+Shift+C also
  copies

### Capture

Capture is **off by default** — a session that is only being watched writes
nothing. Start it from the right-click menu; the file is created as
`<PORT>_YYYYmmdd_HHMMSS.log` (e.g. `COM3_20261105_183126.log`) in the configured
folder and grows while capture stays on, so captures from two ports are always
distinguishable. Only text is written (timestamped, one line per record, with the
RX/TX/SYS tag); raw binary is not captured — use the framed TCP port for that.

`Set capture folder…` changes the folder (persisted); a running capture is
restarted into the new folder under a fresh name. The on/off state itself is not persisted, so a
forgotten session cannot keep filling the disk: start the app with `-capture`
or set `"capture_enabled": true` if you want it on from launch.

### Colour

Output is colourised in the browser from a scheme file, so the screen reads
faster. Two are built in and you can add your own.

| scheme | look |
|---|---|
| `WindTerm (Monokai)` (default) | the keyword sets from WindTerm 2.7.0's `terminal/schemes/linux/linux.lexer` and the colours from `global/themes/dige-black/scheme.theme`, punctuation included — the dense look. Note that its lexer has no rule for kernel `[ 1234.567]` stamps, so the digits fall through to `constant.numeric` (violet) and only the brackets are green, which is what WindTerm itself shows on a serial console |
| `Simple` | quiet: errors, warnings, timestamps and numbers only |

The right-click menu shows colour as a parent/child pair:

```
COLOR
● Color output            WindTerm (Monokai)   <- switch, active scheme on the right
│ ✔ WindTerm (Monokai)                        <- schemes, indented under the switch
│   Simple
│   Android Boot
└   Quiet
```

The switch carries a filled dot when on and a hollow one when off; the selected
scheme carries a ✔ and a highlighted row, so "on" and "which one" are told apart
at a glance. With colour off the whole list greys out and stops responding.
Switching repaints the visible history immediately.

### Where the files live

The tool is a portable folder: the config sits beside the executable as
`serial-log.json`, and the colour schemes beside it in `schemes\`:

```
serial-log.exe
serial-log.json          <- settings (port, baud, capture folder, ...)
schemes  windterm.json          <- seeded on first run from the built-in defaults
  simple.json
  my-android.json        <- yours: any number of files, any names
```

The startup banner prints both paths. If the folder is not writable (installed
under `Program Files`) it falls back to `%LOCALAPPDATA%\serial-log\`.

The menu is built from that folder: add, edit or delete a `*.json` and reopen
the menu — the list re-reads the folder every time it opens, and the file on
disk always wins over the built-in copy. Selecting the scheme that is already
active re-reads it, so an edit shows up without restarting the tool. A file with
broken JSON is listed as `<name> (invalid JSON)` instead of disappearing.

```json
{
  "name": "Android Boot",
  "rules": [
    { "name": "tag",  "color": "#ff79c6", "pattern": "\\b(?:init|zygote|system_server)\\b" },
    { "name": "boot", "color": "#8be9fd", "pattern": "\\b(?:Starting|Started|Boot completed)\\b" }
  ]
}
```

Two traps worth knowing. JSON needs the backslash doubled: write \\b for a word boundary, because a single one is the JSON escape for a backspace character and such a rule can never match (the tool says so in the browser console). And the patterns are plain JavaScript regexes: an inline (?i) is stripped and turned into the i flag, which is on anyway.

### Search

Ctrl+F or the context menu opens the search bar at the top right: the pattern
is matched against the whole buffer, all hits are highlighted, the counter shows
`current/total`, and ↑ / ↓ (Enter / Shift+Enter) walk through them. The `.*`
button next to the field switches the pattern to a regular expression (it is
highlighted while active); an invalid pattern is reported as `bad pattern`.
The counter only takes space while there is something to report.

Live output and the input path are two independent streams over SSE
(`/api/stream`, RX only) and `POST /api/send`, so a slow screen never blocks the
serial read loop, and typing never waits for rendering.

> Note on kernel consoles: a target that spams `printk` through the same UART
> can corrupt bytes you type (the console output path has no flow control).
> The bridge sends exactly the bytes it records — check the captured log if a
> command arrives mangled.


## Capture files

Written only while capture is on (200 ms flush), rotated at 64 MB, one file per
capture session:

```
2026-11-05 18:08:13.804 RX  COM3       [ 1548.470740] i2c_geni 4a84000.i2c: i2c error :-107
2026-11-05 18:08:20.117 TX  local      echo ui_final_9
2026-11-05 18:08:20.119 RX  COM3       ui_final_9
```

Column order: timestamp, direction (RX/TX/SYS), source (port or client), text.
Default folder `%USERPROFILE%\Documents\serial-log`, changeable from the
context menu or `-capturedir`.

## Configuration

`%LOCALAPPDATA%\conf.json` (override with `-config`) is read at startup, command
line flags win, and the resolved settings are written back. Every field:

```json
{
  "com_port": "COM3", "baud_rate": 115200, "data_bits": 8,
  "stop_bits": 1, "parity": "none", "encoding": "utf-8",
  "log_file": "", "capture_enabled": false, "capture_dir": "",
  "target_ip": "", "target_port": 2601, "tap_port": 2602, "frame_port": 2603,
  "write_policy": "first", "web_port": 18080,
  "window": true, "auto_reopen": true,
  "line_idle_ms": 100, "max_line_bytes": 8192,
  "max_records": 200000, "max_bytes": 33554432
}
```

`target_ip` is the **bind address** for the raw port (`""` = all interfaces,
`127.0.0.1` = this machine only). The HTTP API always binds loopback.

Flags: `-p -b -P -tap -frame -wpolicy -web -ip -log -capture -capturedir -enc
-window -no-window -demo -scan -config -version`.

## Behaviour notes

- **Auto reopen**: unplug/replug, a device reboot that drops DTR, or a port that
  was still held at startup all recover automatically (1 s → 4 s backoff).
- **Slow consumers cannot stall the port**: every TCP client and the window has
  its own bounded path; a wedged client loses chunks and is reported in the
  runtime log, the serial read loop keeps running.
- **No byte rewriting**: the capture and the TCP transports carry exactly what
  came off the wire. `\r` is only interpreted when splitting display rows.
- **WebView2**: the window needs the Edge WebView2 runtime (present on Windows
  11). Without it the tool falls back to printing the URL.

## HTTP API

Loopback only, used by the window; handy for scripting.

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/state` | status, counters, config, subscriber list |
| GET | `/api/ports` | available serial ports |
| GET | `/api/records?after=<seq>&limit=n` | history by cursor |
| GET | `/api/records?tail=n` | last n records |
| GET | `/api/stream` | SSE: live RX bytes for the terminal |
| POST | `/api/open` | `{com_port,baud_rate,data_bits,stop_bits,parity,encoding}` |
| POST | `/api/close` | close the port |
| POST | `/api/send` | `{text,hex,eol:"none|cr|lf|crlf"}` |
| POST | `/api/clear` | drop the in-memory log |
| POST | `/api/capture` | `{on:bool, dir:"…"}` toggle capture; `{dir}` alone just stores the folder |
| GET | `/api/export?format=txt\|hex` | download retained history |
| POST | `/api/demo?on=true\|false` | toggle the synthetic source |

## Build

```bash
go build ./...                     # everything
go build -o serial-log.exe ./cmd/bridge
go test ./cmd/bridge               # unit + end-to-end tests (fake device)
```

Pure Go, no cgo, no node/npm: the frontend is hand-written HTML/CSS/JS embedded
in the binary. For a window without a console, add `-ldflags -H=windowsgui`.

## Differences from the hub4com version

Removed: hub4com supervision, com0com pair management, FTDI CBUS/VBUS control,
the systray menu and the old HTTP control panel. Everything removed is still in
the `main` branch history. The reason: all of it existed to share one physical
port between several *serial* programs, and this tool now owns the port itself
and shares the stream over TCP, which needs no driver.
