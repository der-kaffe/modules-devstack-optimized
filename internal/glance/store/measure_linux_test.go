package store

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"runtime"
	"testing"
	"time"
)

// oddReader returns irregular chunk sizes to cross buffer boundaries.
type oddReader struct {
	data []byte
	step int
}

func (r *oddReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	r.step = r.step%7 + 1
	n := min(len(p), len(r.data), r.step*4093)
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func serialInfo(b []byte) Info {
	m, h := md5.Sum(b), sha512.Sum512(b)
	return Info{Size: int64(len(b)), Checksum: hex.EncodeToString(m[:]), SHA512: hex.EncodeToString(h[:])}
}

// A2: parallel MD5 must give exactly the serial digests and written bytes.
func TestMeasureParallelMatchesSerial(t *testing.T) {
	for _, size := range []int{0, 1, BufferSize - 1, BufferSize, BufferSize + 1, 3*BufferSize + 7, 8<<20 + 3} {
		data := bytes.Repeat([]byte{0x00, 0xff, 'g', 'o'}, size/4+1)[:size]
		var written bytes.Buffer
		n := int64(size)
		got, err := measure(context.Background(), &oddReader{data: data}, written.Write, n, &n)
		if err != nil || got != serialInfo(data) || !bytes.Equal(written.Bytes(), data) {
			t.Fatalf("size %d: %v %+v", size, err, got)
		}
		got, err = measure(context.Background(), bytes.NewReader(data), nil, n+100, nil)
		if err != nil || got != serialInfo(data) {
			t.Fatalf("unknown length %d: %v", size, err)
		}
	}
}

func TestMeasureLeavesNoGoroutineOnAnyExit(t *testing.T) {
	before := runtime.NumGoroutine()
	data := bytes.Repeat([]byte("x"), 3*BufferSize)
	n := int64(len(data))
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	short := int64(10)
	failing := func([]byte) (int, error) { return 0, errors.New("disk") }
	for i, run := range []func() error{
		func() error {
			_, err := measure(context.Background(), bytes.NewReader(data), failing, n, &n)
			return err
		},
		func() error {
			_, err := measure(context.Background(), bytes.NewReader(data), nil, short, nil)
			return err
		},
		func() error {
			_, err := measure(context.Background(), bytes.NewReader(data), nil, n, &short)
			return err
		},
		func() error { _, err := measure(cancelled, bytes.NewReader(data), nil, n, nil); return err },
		func() error { _, err := measure(context.Background(), bytes.NewReader(data), nil, n, &n); return err },
	} {
		if err := run(); (err == nil) != (i == 4) {
			t.Fatal(i, err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if runtime.NumGoroutine() > before {
		t.Fatalf("goroutines leaked: %d > %d", runtime.NumGoroutine(), before)
	}
}
