# tcp_serial

Go programs for bridging a serial port to TCP multi-clients and an interactive TCP console demo.

## 1) Build

```bash
go mod tidy
go build ./...
```

## 2) Start serial to TCP bridge server

Windows example (COM3 @ 115200):

```bash
go run ./cmd/bridge -p 3 -b 115200 -ip 0.0.0.0 -P 2601 -log bridge.log
```

With COM scan menu:

```bash
go run ./cmd/bridge -scan -b 115200 -ip 0.0.0.0 -P 2601
```

## 2b) hub4com managed mode (share a physical COM port with many TCP sessions)

Instead of opening the physical COM port directly (which only one app may open),
the bridge can spawn and supervise a `hub4com.exe` child that bridges the
physical port to a com0com virtual pair. The bridge then opens the other end
of the pair and serves any number of TCP clients:

```
COM22 (physical) <-> hub4com.exe <-> COM24 ~~com0com pair~~ COM23 <-> bridge <-> TCP :2601
```

```bash
go run ./cmd/bridge -config bridge_com23.json -hub4com C:\work\tools\hub4com\hub4com-2.1.0.0-386\hub4com.exe -phy COM22 -hub-slave COM24 -p 23 -b 115200 -P 2601
```

Config fields (or flags):

- `hub4com_path` (`-hub4com`) - path to hub4com.exe. When set, hub4com mode is enabled.
- `hub_physical_port` (`-phy`) - the physical COM port hub4com opens (e.g. COM22).
- `hub_slave_port` (`-hub-slave`) - the com0com pair end hub4com attaches to
  (e.g. COM24). Default: peer of `-p` (COM23 -> COM24, CNCA0 -> CNCB0, ...).
- `hub_extra_slaves` (`-hub-slave-extra`, comma-separated) - additional hub4com
  slave ports for other consumers (e.g. a local serial monitor). Each needs its
  own com0com pair; the consumer opens the pair peer. Physical RX is broadcast
  to ALL slaves, and each slave's TX goes to the physical port only (consumers
  are isolated from each other).
- `hub4com_log_file` - where hub4com's own output is logged (default: hub4com.log
  next to the bridge log).

Multi-consumer example (local WindTerm + remote AI):

```
hub4com: COM22(0) <-> COM24(1), COM27(2)      routes: 0:All, 1:0, 2:0
pair 1: COM24 ~~ COM23  -> bridge -> TCP :2601 -> remote AI
pair 2: COM27 ~~ COM28  -> local serial monitor (WinTerm opens COM28)
```

```json
{
  "com_port": "COM23",
  "hub4com_path": "C:\\work\\tools\\hub4com\\hub4com-2.1.0.0-386\\hub4com.exe",
  "hub_physical_port": "COM22",
  "hub_slave_port": "COM24",
  "hub_extra_slaves": ["COM27"]
}
```

Hub4com mode behavior:

- Bridge spawns `hub4com.exe --octs=off --baud=<b> \\.\COM22 \\.\COM24` and waits
  until hub4com holds the slave port (readiness probe).
- `--octs=off` disables CTS handshaking on hub4com output; required for reliable
  data flow with com0com virtual pairs (the default `octs=on` stalls and can
  corrupt the pair with ERROR 31).
- If hub4com crashes (device unplugged etc.), the bridge restarts it with
  backoff and reopens the bridge serial port automatically.
- When the bridge exits, the hub4com child is killed.
- Menu option 2 (select COM port) switches the hub4com physical port; option 3
  (change baudrate) restarts hub4com with the new baud.

Prerequisites: com0com driver with a virtual pair must be installed (the pair
COM23/COM24 is persistent once created with setupc).

## 3) TCP demo client console

Run one or more clients to verify multi-client behavior:

```bash
go run ./cmd/tcp_demo -ip 127.0.0.1 -P 2601 -log tcp_demo.log
```

Interactive behavior:

- Incoming TCP payload is printed in console as readable text (`[recv] ...`).
- You can type text in console and press Enter to send to serial via bridge (`[send]`).
- Type `/quit` to exit this client.

Supported bridge parameters:

- `-p` COM port number or name, e.g. `3` or `COM3`
- `-b` baud rate
- `-log` log file name
- `-ip` TCP listen IP address
- `-P` TCP listen port (default: `2601`)
- `-scan` scan COM ports and select one interactively
- `-config` local config file path (default: `%LOCALAPPDATA%\\conf.json`)

Socket mode behavior:

- If `-ip` is NOT provided in command line: bridge runs as TCP server and listens on `0.0.0.0:-P`.
- If `-ip` IS provided in command line: bridge runs as TCP client and repeatedly connects to `-ip:-P`.

Config behavior:

- On startup, bridge reads config from local `bridge_config.json` (or `-config` path).
- Any value provided by command-line parameters overrides config file values.
- After resolving final values, bridge saves all settings back to the config file.

Bridge mode:

- Bridge runs as TCP server and accepts multiple clients.
- Data read from serial is broadcast to all connected clients.
- Data from all clients is forwarded to serial port.
- If no clients are connected, serial RX data is cached (FIFO, max 4MB).
- When at least one client connects, cached data is flushed first, then live data continues.

Runtime control menu:

- Press `Ctrl+A` in bridge console to open menu.
- If terminal does not pass `Ctrl+A`, type `/menu` then press Enter.
- Menu stays open after each action; choose `0` to go back.
- Menu accepts inputs like `1` or `1.`.
- Menu actions:
	- `1` Stop and exit:
		close listener, disconnect all clients, close serial port, exit process.
	- `2` Close serial and select another COM port:
		list COM ports and reopen with selected one.
	- `3` Change baudrate:
		choose from common baud list or input custom baud value.
	- `4` Send file to socket clients:
		input file path and send whole file bytes to all connected clients.
	- `5` Log rx/tx data to file:
		input file path and append all RX/TX packets as hex log lines.

