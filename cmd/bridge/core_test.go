package main

import (
	"strings"
	"testing"
	"time"
)

// ---------- framer ----------

func TestFramerSplitsCRLF(t *testing.T) {
	f := newFramer(8192, time.Second)
	chunks := f.Push([]byte("alpha\r\nbeta\r\n"))
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(chunks))
	}
	if string(chunks[0].Data) != "alpha\r\n" || string(chunks[1].Data) != "beta\r\n" {
		t.Fatalf("unexpected chunks: %q %q", chunks[0].Data, chunks[1].Data)
	}
	if !chunks[0].Complete || !chunks[1].Complete {
		t.Fatal("chunks should be complete")
	}
}

// A CRLF split across two reads must produce one line, not two.
func TestFramerHoldsTrailingCR(t *testing.T) {
	f := newFramer(8192, time.Second)
	if got := f.Push([]byte("alpha\r")); len(got) != 0 {
		t.Fatalf("trailing CR must wait, got %d chunks", len(got))
	}
	got := f.Push([]byte("\nbeta\r\n"))
	if len(got) != 2 {
		t.Fatalf("want 2 chunks, got %d (%v)", len(got), got)
	}
	if string(got[0].Data) != "alpha\r\n" {
		t.Fatalf("want CRLF joined, got %q", got[0].Data)
	}
}

// MCU consoles often terminate with a bare CR.
func TestFramerBareCR(t *testing.T) {
	f := newFramer(8192, time.Second)
	got := f.Push([]byte("one\rtwo\r"))
	if len(got) != 1 || string(got[0].Data) != "one\r" {
		t.Fatalf("want [one\\r], got %v", got)
	}
	tail := f.FlushIdle(time.Now().Add(2 * time.Second))
	if len(tail) != 1 || string(tail[0].Data) != "two\r" || !tail[0].Complete {
		t.Fatalf("idle flush should release the trailing CR line, got %v", tail)
	}
}

func TestFramerIdleFlushPartial(t *testing.T) {
	f := newFramer(8192, 100*time.Millisecond)
	if got := f.Push([]byte("progress 42%")); len(got) != 0 {
		t.Fatalf("no terminator yet, got %d", len(got))
	}
	if got := f.FlushIdle(time.Now()); len(got) != 0 {
		t.Fatalf("not idle yet, got %d", len(got))
	}
	got := f.FlushIdle(time.Now().Add(200 * time.Millisecond))
	if len(got) != 1 || string(got[0].Data) != "progress 42%" || got[0].Complete {
		t.Fatalf("want one partial chunk, got %#v", got)
	}
}

func TestFramerMaxLine(t *testing.T) {
	f := newFramer(8, time.Second)
	got := f.Push([]byte("0123456789ABCDEF"))
	if len(got) != 2 {
		t.Fatalf("want 2 pieces, got %d", len(got))
	}
	for _, c := range got {
		if len(c.Data) != 8 || c.Complete {
			t.Fatalf("unexpected piece %q complete=%v", c.Data, c.Complete)
		}
	}
}

func TestFramerNoDataLoss(t *testing.T) {
	f := newFramer(5, time.Second)
	in := "abc\r\ndef\rghijkl\r\n"
	var out strings.Builder
	for _, c := range f.Push([]byte(in)) {
		out.Write(c.Data)
	}
	for _, c := range f.Flush() {
		out.Write(c.Data)
	}
	if out.String() != in {
		t.Fatalf("byte stream not preserved:\n in=%q\nout=%q", in, out.String())
	}
}

// ---------- ring ----------

func TestRingEvictsByCount(t *testing.T) {
	r := newRing(5, 1<<20)
	for i := 0; i < 10; i++ {
		r.Append(Rec{Dir: "rx", Text: "x"})
	}
	recs, last := r.Since(0, 100)
	if len(recs) != 5 {
		t.Fatalf("want 5 retained, got %d", len(recs))
	}
	if recs[0].Seq != 6 || last != 10 {
		t.Fatalf("want seq 6..10, got %d..%d", recs[0].Seq, last)
	}
	if r.Oldest() != 6 {
		t.Fatalf("oldest = %d, want 6", r.Oldest())
	}
}

func TestRingCursorAndTail(t *testing.T) {
	r := newRing(100, 1<<20)
	for i := 0; i < 5; i++ {
		r.Append(Rec{Dir: "rx", Text: "x"})
	}
	recs, last := r.Since(2, 100)
	if len(recs) != 3 || last != 5 || recs[0].Seq != 3 {
		t.Fatalf("Since(2) = %d recs, last %d, first %d", len(recs), last, recs[0].Seq)
	}
	tail, seq := r.Tail(2)
	if len(tail) != 2 || seq != 5 || tail[1].Seq != 5 {
		t.Fatalf("Tail(2) = %d recs, seq %d", len(tail), seq)
	}
}

func TestRingEvictsByBytes(t *testing.T) {
	r := newRing(1000, 200)
	for i := 0; i < 20; i++ {
		r.Append(Rec{Dir: "rx", Text: strings.Repeat("a", 50)})
	}
	count, bytes, _, dropped := r.Stats()
	if bytes > 200 {
		t.Fatalf("ring exceeded byte budget: %d", bytes)
	}
	if count == 0 || dropped == 0 {
		t.Fatalf("expected eviction, count=%d dropped=%d", count, dropped)
	}
}

func TestRingClearKeepsSequence(t *testing.T) {
	r := newRing(10, 1<<20)
	r.Append(Rec{Text: "a"})
	r.Clear()
	if recs, _ := r.Since(0, 10); len(recs) != 0 {
		t.Fatalf("clear should drop records, got %d", len(recs))
	}
	rec := r.Append(Rec{Text: "b"})
	if rec.Seq != 2 {
		t.Fatalf("sequence must keep increasing, got %d", rec.Seq)
	}
}

// ---------- codec / rendering ----------

func TestCodecGBKSplitMultibyte(t *testing.T) {
	c := newCodec("gbk")
	encoded := newCodec("gbk").Encode([]byte("中文测试"))
	if string(encoded) == "中文测试" {
		t.Fatal("encode did not change the bytes")
	}
	half := len(encoded) / 2
	first := c.Decode(encoded[:half])
	second := c.Decode(encoded[half:])
	got := string(first) + string(second)
	if got != "中文测试" {
		t.Fatalf("split decode = %q, want 中文测试", got)
	}
	if len(first) == 0 {
		t.Fatal("first half should decode to at least one rune")
	}
}

func TestDisplayTextEscapesControls(t *testing.T) {
	text, binary := displayText([]byte("a\x1b[31mb\r\n"))
	if binary {
		t.Fatal("text line flagged binary")
	}
	if text != `a\e[31mb` {
		t.Fatalf("got %q", text)
	}
	if _, binary := displayText([]byte{0x00, 0x01, 0x02, 0xff}); !binary {
		t.Fatal("control-byte payload should be flagged binary")
	}
}

func TestHexStringWrapsAt16(t *testing.T) {
	b := make([]byte, 20)
	got := hexString(b)
	if strings.Count(got, "\n") != 1 {
		t.Fatalf("want one wrap after 16 bytes, got %q", got)
	}
}

func TestSendHex(t *testing.T) {
	got, err := SendHex("55 AA 01")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "\x55\xaa\x01" {
		t.Fatalf("got %x", got)
	}
	if _, err := SendHex("55 A"); err == nil {
		t.Fatal("odd digit count must fail")
	}
	if _, err := SendHex("ZZ"); err == nil {
		t.Fatal("non-hex must fail")
	}
}

// ---------- bus ----------

func TestBusFansOutAndExcludes(t *testing.T) {
	b := newBus()
	s1 := b.add("a", 8)
	s2 := b.add("b", 8)
	if n := b.publish([]byte("hi"), dirRx, 0, nil); n != 2 {
		t.Fatalf("want 2 recipients, got %d", n)
	}
	if string((<-s1.ch).data) != "hi" || string((<-s2.ch).data) != "hi" {
		t.Fatal("both subscribers must receive the chunk")
	}
	b.publish([]byte("echo"), dirRx, s1.id, nil)
	if len(s1.ch) != 0 {
		t.Fatal("excluded subscriber must not receive the chunk")
	}
	if string((<-s2.ch).data) != "echo" {
		t.Fatal("other subscriber must receive it")
	}
}

// A wedged subscriber must never block the serial read loop.
func TestBusDropsForSlowSubscriber(t *testing.T) {
	b := newBus()
	s := b.add("slow", 1)
	drops := 0
	for i := 0; i < 5; i++ {
		b.publish([]byte("0123456789"), dirRx, 0, func(*sub) { drops++ })
	}
	if len(s.ch) != 1 {
		t.Fatalf("queue should stay at capacity, len=%d", len(s.ch))
	}
	if s.dropped != 4 {
		t.Fatalf("want 4 drops, got %d", s.dropped)
	}
	if drops != 1 {
		t.Fatalf("drop should be reported once per episode, got %d", drops)
	}
}

func TestBusRemoveClosesQueue(t *testing.T) {
	b := newBus()
	s := b.add("x", 4)
	b.remove(s)
	select {
	case <-s.done:
	default:
		t.Fatal("done channel should be closed")
	}
	if b.count() != 0 {
		t.Fatal("subscriber not removed")
	}
}
