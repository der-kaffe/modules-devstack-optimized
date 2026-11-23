package format

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
)

// Wrapper is InspectWrapper over every format: Write feeds it the stream
// (io.TeeReader), Finish marks the end.
type Wrapper struct {
	inspectors []*inspector
	errored    map[*inspector]bool
	finished   bool
}

// All is the original's ALL_FORMATS.
var All = []string{"raw", "qcow2", "vhd", "vhdx", "vmdk", "vdi", "qed", "iso", "gpt", "luks"}

func NewWrapper() *Wrapper {
	return &Wrapper{errored: map[*inspector]bool{}, inspectors: []*inspector{
		newRaw(), newQcow(), newVHD(), newVHDX(), newVMDK(), newVDI(), newQED(), newISO(), newGPT(), newLUKS()}}
}

func (w *Wrapper) Write(p []byte) (int, error) {
	for _, i := range w.inspectors {
		if w.errored[i] {
			continue
		}
		if err := i.eatChunk(p); err != nil {
			w.errored[i] = true // excluded from consideration, as the original
		}
	}
	return len(p), nil
}

func (w *Wrapper) Finish() {
	for _, i := range w.inspectors {
		i.finish()
	}
	w.finished = true
}

func call[T any](fn func() T) (v T, err error) {
	defer func() {
		if p := recover(); p != nil {
			if e, ok := p.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("%v", p)
			}
		}
	}()
	return fn(), nil
}

// formats is InspectWrapper.formats (nil: not yet known).
func (w *Wrapper) formats() ([]*inspector, error) {
	complete := true
	var matches []*inspector
	for _, i := range w.inspectors {
		if i.name == "raw" {
			continue
		}
		complete = complete && i.complete()
		m, err := call(i.impl.formatMatch)
		if err != nil {
			return nil, err
		}
		if m {
			matches = append(matches, i)
		}
	}
	if !complete && !w.finished {
		return nil, nil
	}
	if len(matches) == 0 {
		return w.inspectors[:1], nil // raw
	}
	return matches, nil
}

func names(list []*inspector) []string {
	out := make([]string, 0, len(list))
	for _, i := range list {
		out = append(out, i.name)
	}
	sort.Strings(out)
	return out
}

// ErrInvalidImageData is the original's InvalidImageData (HTTP 415).
type ErrInvalidImageData struct{ Msg string }

func (e *ErrInvalidImageData) Error() string { return e.Msg }

// Check is Glance's set_data decision after the upload (location.py):
// the virtual size to record (0: unknown) or InvalidImageData. Only bare
// containers are inspected; with requireMatch off a mismatch is logged.
func (w *Wrapper) Check(diskFormat string, requireMatch bool, nonfatal []string) (int64, error) {
	invalid := func(msg string) (int64, error) { return 0, &ErrInvalidImageData{msg} }
	var insp *inspector
	var virtual int64
	err := func() error {
		matches, err := w.formats()
		if err != nil {
			return err
		}
		got := names(matches)
		if len(got) == 2 && got[0] == "gpt" && got[1] == "iso" {
			for _, m := range matches {
				if m.name == "iso" {
					insp = m
				}
			}
		} else {
			if matches == nil {
				return errors.New("'NoneType' object has no attribute")
			}
			if len(matches) > 1 {
				return formatError("Multiple formats detected: %s", strings.Join(got, ","))
			}
			insp = matches[0]
		}
		switch {
		case insp.name == diskFormat:
			v, err := call(insp.impl.virtualSize)
			if err != nil {
				return err
			}
			virtual = v
		case insp.name == "raw" && !slices.Contains(All, diskFormat):
			// not an inspectable type (ami, ari, aki, ploop): allowed
		case diskFormat == "raw":
			virtual = insp.total // as raw: its actual size in bytes
		default:
			return formatError("Image disk_format %s does not match stream data %s", diskFormat, insp.name)
		}
		return insp.safetyCheck()
	}()
	var fe *ImageFormatError
	var sf *SafetyCheckFailed
	switch {
	case err == nil:
		return virtual, nil
	case errors.As(err, &fe):
		if requireMatch {
			return invalid(fe.Msg)
		}
		return 0, nil
	case errors.As(err, &sf):
		fatal := map[string]bool{}
		for name := range sf.Failures {
			if !slices.Contains(nonfatal, name) {
				fatal[name] = true
			}
		}
		if diskFormat == "aki" || diskFormat == "ari" {
			delete(fatal, "mbr")
		}
		if insp != nil && insp.name == "gpt" && len(fatal) == 0 {
			return virtual, nil
		}
		return invalid("Image failed safety checks")
	default:
		if requireMatch {
			return invalid("Image format inspection failed")
		}
		return virtual, nil
	}
}

// DetectFile is the original's detect_file_format plus the safety check
// the conversion plugin runs: the format name, or an error with its
// message ("Image has disallowed configuration", "Image format detection
// failed", "Unable to inspect image").
func DetectFile(path string, nonfatal []string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("Unable to inspect image")
	}
	defer f.Close()
	w := NewWrapper()
	buf := make([]byte, 4096)
	var found *inspector
	for found == nil {
		n, err := f.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if m, e := w.formats(); e == nil && len(m) == 1 {
				found = m[0]
			}
		}
		if err != nil {
			break
		}
	}
	w.Finish()
	if found == nil {
		m, e := w.formats()
		if e != nil || len(m) != 1 {
			return "", errors.New("Image format detection failed")
		}
		found = m[0]
	}
	if err := found.safetyCheck(); err != nil {
		var sf *SafetyCheckFailed
		if errors.As(err, &sf) {
			fatal := 0
			for name := range sf.Failures {
				if !slices.Contains(nonfatal, name) {
					fatal++
				}
			}
			if found.name == "gpt" && fatal == 0 {
				return found.name, nil
			}
			return "", errors.New("Image has disallowed configuration")
		}
		return "", errors.New("Image format detection failed")
	}
	return found.name, nil
}
