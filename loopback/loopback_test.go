package loopback

import (
	"encoding/binary"
	"io"
	"os"
	"testing"
	"time"
)

func TestSimpleWriteRead(t *testing.T) {
	r, w := Pipe(Options{})
	msg := []byte("hello, world")
	n, err := w.Write(msg)
	if err != nil {
		t.Fatalf("write error: %v", err)
	}
	if n != len(msg) {
		t.Fatalf("write of too few bytes, expected %d, got %d", len(msg), n)
	}
	buf := make([]byte, 100)
	n, err = r.Read(buf)
	if err != nil {
		t.Fatalf("read error: %v", err)
	}
	if n != len(msg) {
		t.Fatalf("too few bytes")
	}
	buf = buf[0:n]
	if string(buf) != string(msg) {
		t.Fatalf("received wrong data: %q", buf)
	}
}

func TestOutputClose(t *testing.T) {
	r, w := Pipe(Options{})
	writePacket(t, w, make([]byte, 14), 0)
	w.Close()

	buf := make([]byte, 14)
	readPacket(t, r, buf, 0)
	_, err := r.Read(buf)
	if err != os.EOF {
		t.Fatalf("expected os.EOF, got %v", err)
	}
}

func TestInputClose(t *testing.T) {
	r, w := Pipe(Options{MTU: 100, InLimit: 2 * 100, OutLimit: 2 * 100})
	sync := make(chan bool)
	go func() {
		buf := make([]byte, 100)
		for i := 0; i < 10; i++ {
			_, err := w.Write(buf)
			if err != nil {
				if err != os.EPIPE {
					t.Fatalf("expected EPIPE error; got %v", err)
				}
				break
			}
		}
		sync <- true
	}()
	r.Close()
	select {
	case <-time.After(0.2e9):
		t.Fatalf("close did not wake up writer")
	case <-sync:
	}
}

func TestLatency(t *testing.T) {
	const (
		n       = 10
		latency = int64(0.1e9)
		leeway  = int64(0.01e9)
	)
	r, w := Pipe(Options{Latency: 0.1e9})
	go writeNValues(t, w, n, make([]byte, 14), 0.1e9)
	buf := make([]byte, 14)
	for i := 0; i < 10; i++ {
		now, sentTime := readPacket(t, r, buf, i)
		if abs(now-sentTime-latency) > leeway {
			t.Errorf("expected latency of %dns; got %dns\n", latency, now-sentTime)
		}
	}
}

func TestBandwidth(t *testing.T) {
	const (
		n          = 10
		packetSize = 8192
		bandwidth  = int64((1024 * 1024) / 8) // 1 Mbit in bytes
		delay      = 1e9 / bandwidth          // byte delay in ns.
	)
	r, w := Pipe(Options{ByteDelay: delay, MTU: 8192})
	go writeNValues(t, w, n, make([]byte, 8192), 0)
	buf := make([]byte, 8192)
	var t0, t1 int64
	for i := 0; i < n; i++ {
		now, sent := readPacket(t, r, buf, i)
		t1 = now
		if t0 == 0 {
			t0 = sent
		}
	}
	expect := delay * int64(packetSize*n)
	got := t1 - t0
	if abs(expect-got)*100/expect > 1 {
		t.Error("wrong bandwidth; expected %dns; got %dns", expect, got)
	}
}

func TestWriteBlocking(t *testing.T) {
	r, w := Pipe(Options{MTU: 14, InLimit: 14 * 2, OutLimit: 14 * 2})
	sync := make(chan bool)
	go func() {
		// fill buffers - 2 for the buffer at each end, and
		// one blocked in transit.
		writeNValues(t, w, 5, make([]byte, 14), 0)
		sync <- true
		// write one more, which should block.
		writePacket(t, w, make([]byte, 14), 99)
		sync <- true
	}()
	// Check that write has not blocked filling the buffers.
	select {
	case <-time.After(0.2e9):
		t.Fatalf("writer blocked too early")
	case <-sync:
	}

	time.Sleep(0.2e9)

	// Check that write has correctly blocked.
	select {
	case <-sync:
		t.Fatalf("writer did not block")
	default:
	}

	// check that write unblocks when we read a packet.
	readPacket(t, r, make([]byte, 14), 0)
	time.Sleep(0.2e9)
	select {
	case <-time.After(0.2e9):
		t.Fatalf("writer still blocked")
	case <-sync:
	}
}

func BenchmarkPacketTransfer(b *testing.B) {
	r, w := Pipe(Options{})
	bufSize := 100
	b.SetBytes(int64(bufSize))
	go func() {
		buf := make([]byte, bufSize)
		for i := b.N - 1; i >= 0; i-- {
			w.Write(buf)
		}
	}()
	buf := make([]byte, bufSize)
	for i := b.N - 1; i >= 0; i-- {
		n, err := r.Read(buf)
		if n != bufSize || err != nil {
			panic("read failed")
		}
	}
}

func BenchmarkPipeTransfer(b *testing.B) {
	r, w := io.Pipe()
	bufSize := 100
	b.SetBytes(int64(bufSize))
	go func() {
		buf := make([]byte, bufSize)
		for i := b.N - 1; i >= 0; i-- {
			w.Write(buf)
		}
	}()
	buf := make([]byte, bufSize)
	for i := b.N - 1; i >= 0; i-- {
		n, err := r.Read(buf)
		if n != bufSize || err != nil {
			panic("read failed")
		}
	}
}

const check = 0xfea1

func writeNValues(t *testing.T, s io.Writer, n int, buf []byte, period int64) {
	for i := 0; i < n; i++ {
		writePacket(t, s, buf, i)
		if period > 0 {
			time.Sleep(period)
		}
	}
}

func writePacket(t *testing.T, s io.Writer, buf []byte, index int) {
	if len(buf) < 14 {
		panic("buf too small for header")
	}
	binary.LittleEndian.PutUint16(buf, check)
	binary.LittleEndian.PutUint64(buf[2:], uint64(time.Nanoseconds()))
	binary.LittleEndian.PutUint32(buf[10:], uint32(index))
	n, err := s.Write(buf)
	if err != nil {
		t.Fatalf("write error: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("write count: expected %d; got %d", len(buf), n)
	}
}

func readPacket(t *testing.T, s io.Reader, buf []byte, index int) (int64, int64) {
	n, err := s.Read(buf)
	now := time.Nanoseconds()
	if err != nil {
		t.Fatalf("read error: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("read count: expected %d; got %d", len(buf), n)
	}
	c := int(binary.LittleEndian.Uint16(buf))
	if c != check {
		t.Fatalf("invalid check; expected %#x; got %#x", check, c)
	}
	sentIndex := int(binary.LittleEndian.Uint32(buf[10:14]))
	if sentIndex != index {
		t.Errorf("block arrived out of order; expected %d; got %d", index, sentIndex)
	}
	sentTime := int64(binary.LittleEndian.Uint64(buf[2:10]))
	return now, sentTime
}

func abs(x int64) int64 {
	if x >= 0 {
		return x
	}
	return -x
}
