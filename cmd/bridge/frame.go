package main

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Wire format of the framed TCP port (2603):
//
//	[u32 BE payload length][u8 direction][u64 BE unix-millis][payload]
//
// Direction: 1 = RX (device -> host), 2 = TX (host -> device), 3 = system note.
// Incoming frames from a client are always treated as TX.
const (
	dirRx  byte = 1
	dirTx  byte = 2
	dirSys byte = 3

	frameHeaderLen = 4 + 1 + 8
	maxFrameLen    = 1 << 20
)

func dirByte(dir string) byte {
	switch dir {
	case "rx":
		return dirRx
	case "tx":
		return dirTx
	default:
		return dirSys
	}
}

func dirLabel(dir string) string {
	switch dir {
	case "rx":
		return "RX"
	case "tx":
		return "TX"
	default:
		return "SYS"
	}
}

func appendFrame(dst []byte, dir byte, ts int64, payload []byte) []byte {
	var hdr [frameHeaderLen]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(len(payload)))
	hdr[4] = dir
	binary.BigEndian.PutUint64(hdr[5:13], uint64(ts))
	dst = append(dst, hdr[:]...)
	return append(dst, payload...)
}

type frame struct {
	dir     byte
	ts      int64
	payload []byte
}

// readFrame reads exactly one frame. It returns io.EOF only when the stream
// ends cleanly on a frame boundary.
func readFrame(r io.Reader) (frame, error) {
	var hdr [frameHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return frame{}, err
	}
	n := binary.BigEndian.Uint32(hdr[0:4])
	if n > maxFrameLen {
		return frame{}, fmt.Errorf("frame too large: %d bytes", n)
	}
	f := frame{dir: hdr[4], ts: int64(binary.BigEndian.Uint64(hdr[5:13]))}
	if n > 0 {
		f.payload = make([]byte, n)
		if _, err := io.ReadFull(r, f.payload); err != nil {
			return frame{}, err
		}
	}
	return f, nil
}
