package pyjson

import (
	"encoding/json"
	"os"
	"testing"
)

// testdata/vectors.json holds [input, json.dumps(json.loads(input))] pairs
// produced by CPython 3, including every metadefs file shipped with Glance.
func TestDumpsLikePython(t *testing.T) {
	b, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors [][2]string
	if err = json.Unmarshal(b, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors {
		got, err := Dumps([]byte(v[0]))
		if err != nil || got != v[1] {
			t.Errorf("input %.60q\n got %.200s\nwant %.200s (%v)", v[0], got, v[1], err)
		}
	}
}
