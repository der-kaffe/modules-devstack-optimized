package format

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestAgainstOriginalCorpus compares, image by image and disk_format by
// disk_format, the decision with the original format_inspector's. Set
// GLANCE_GO_FORMAT_CORPUS to a directory with img/*.img and expected.json
// (scripts/glance_format_corpus.sh generates both with qemu-img and the
// original oslo.utils module).
func TestAgainstOriginalCorpus(t *testing.T) {
	dir := os.Getenv("GLANCE_GO_FORMAT_CORPUS")
	if dir == "" {
		t.Skip("GLANCE_GO_FORMAT_CORPUS not set")
	}
	b, err := os.ReadFile(filepath.Join(dir, "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string]map[string]string
	if err = json.Unmarshal(b, &expected); err != nil {
		t.Fatal(err)
	}
	cases := 0
	for img, formats := range expected {
		for disk, want := range formats {
			for _, chunk := range []int{64 << 10, 1 << 20, 4096} {
				f, err := os.Open(filepath.Join(dir, "img", img))
				if err != nil {
					t.Fatal(err)
				}
				w := NewWrapper()
				buf := make([]byte, chunk)
				for {
					n, err := f.Read(buf)
					if n > 0 {
						_, _ = w.Write(buf[:n])
					}
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				f.Close()
				w.Finish()
				vs, err := w.Check(disk, true, nil)
				got := fmt.Sprintf("vs:%d", vs)
				if err != nil {
					got = "err:" + err.Error()
				}
				if got != want {
					t.Errorf("%s as %s (chunk %d): got %s, original %s", img, disk, chunk, got, want)
				}
				cases++
			}
		}
	}
	t.Logf("%d cases", cases)
}
