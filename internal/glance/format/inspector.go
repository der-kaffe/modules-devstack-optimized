// Package format is the original's image format inspector
// (oslo_utils.imageutils.format_inspector, as Glance 2026.2 uses it): every
// inspector reads the upload stream as it passes, capturing the regions it
// needs, then the wrapper decides the format, its virtual size and its
// safety checks. The port keeps the same regions, rules and messages.
package format

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// ImageFormatError is the original's ImageFormatError.
type ImageFormatError struct{ Msg string }

func (e *ImageFormatError) Error() string { return e.Msg }

func formatError(f string, a ...any) error { return &ImageFormatError{fmt.Sprintf(f, a...)} }

// SafetyCheckFailed lists the failed checks by name.
type SafetyCheckFailed struct{ Failures map[string]string }

func (e *SafetyCheckFailed) Error() string {
	names := make([]string, 0, len(e.Failures))
	for n := range e.Failures {
		names = append(names, n)
	}
	sort.Strings(names)
	return "Safety checks failed: " + strings.Join(names, ",")
}

type violation struct{ msg string }

func (v violation) Error() string { return v.msg }

// region is CaptureRegion / EndCaptureRegion.
type region struct {
	offset, length int64
	minLength      int64 // 0: none
	data           []byte
	end            bool // EndCaptureRegion
	endDone        bool
}

func (r *region) complete() bool {
	if r.end {
		return int64(len(r.data)) == r.length && r.endDone
	}
	if r.minLength > 0 {
		return r.minLength <= int64(len(r.data))
	}
	return r.length == int64(len(r.data))
}

// capture appends only the bytes the region keeps: the result is the
// original's "data += chunk[gap:]; data = data[:length]" (and, for an end
// region, "data = data[-length:]") without copying whole chunks.
func (r *region) capture(chunk []byte, pos int64) {
	if r.end {
		if int64(len(chunk)) >= r.length {
			r.data = append(r.data[:0], chunk[int64(len(chunk))-r.length:]...)
		} else {
			if extra := int64(len(r.data)+len(chunk)) - r.length; extra > 0 {
				n := copy(r.data, r.data[extra:])
				r.data = r.data[:n]
			}
			r.data = append(r.data, chunk...)
		}
		r.offset = pos - int64(len(r.data))
		return
	}
	start := pos - int64(len(chunk))
	if start <= r.offset && r.offset <= pos || r.offset <= start && start <= r.offset+r.length {
		gap := int64(0)
		if start < r.offset {
			gap = r.offset - start
		}
		if room := r.length - int64(len(r.data)); gap < int64(len(chunk)) && room > 0 {
			r.data = append(r.data, chunk[gap:gap+min(int64(len(chunk))-gap, room)]...)
		}
		if int64(len(r.data)) > r.length {
			r.data = r.data[:r.length]
		}
	}
}

// inspector is FileInspector: regions in insertion order, safety checks.
type inspector struct {
	name     string
	total    int64
	names    []string
	regions  map[string]*region
	checks   []check
	finished bool
	impl     impl
	// idle: every region is complete and none captures the end, so later
	// chunks only move total (postProcess and regionComplete would repeat
	// the same outcome on the same data).
	idle        bool
	scratch     []regionState
	nameScratch []string
}

type regionState struct {
	r        *region
	complete bool
}

type check struct {
	name string
	fn   func() error
}

type impl interface {
	formatMatch() bool
	virtualSize() int64
	postProcess()
	regionComplete(name string)
}

func newInspector(name string) *inspector {
	return &inspector{name: name, regions: map[string]*region{}}
}

func (i *inspector) newRegion(name string, r *region) {
	if _, ok := i.regions[name]; ok {
		panic(formatError("Inspector re-added region %s", name))
	}
	i.names = append(i.names, name)
	i.regions[name] = r
}

func (i *inspector) has(name string) bool { _, ok := i.regions[name]; return ok }

func (i *inspector) region(name string) *region { return i.regions[name] }

func (i *inspector) deleteRegion(name string) {
	delete(i.regions, name)
	for n, v := range i.names {
		if v == name {
			i.names = append(i.names[:n], i.names[n+1:]...)
			break
		}
	}
}

func (i *inspector) capture(chunk []byte, only map[string]bool) {
	for _, name := range i.names {
		r := i.regions[name]
		if only != nil && !only[name] {
			continue
		}
		if r.end || !r.complete() {
			r.capture(chunk, i.total)
		}
	}
}

// eatChunk is FileInspector.eat_chunk; a panic(error) inside an inspector
// is the original's exception and excludes it from the wrapper.
func (i *inspector) eatChunk(chunk []byte) (err error) {
	defer func() {
		if p := recover(); p != nil {
			if e, ok := p.(error); ok {
				err = e
				return
			}
			err = fmt.Errorf("%v", p)
		}
	}()
	if i.idle {
		i.total += int64(len(chunk))
		return nil
	}
	pre := i.scratch[:0]
	for _, name := range i.names {
		r := i.regions[name]
		pre = append(pre, regionState{r, r.complete()})
	}
	i.scratch = pre
	state := func(r *region) (seen, complete bool) {
		for _, p := range pre {
			if p.r == r {
				return true, p.complete
			}
		}
		return false, false
	}
	i.total += int64(len(chunk))
	i.capture(chunk, nil)
	i.impl.postProcess()
	var only map[string]bool
	for _, name := range i.names {
		if seen, _ := state(i.regions[name]); !seen {
			if only == nil {
				only = map[string]bool{}
			}
			only[name] = true
		}
	}
	if len(only) > 0 {
		i.capture(chunk, only)
	}
	// A copy: regionComplete may add or remove regions.
	i.nameScratch = append(i.nameScratch[:0], i.names...)
	for _, name := range i.nameScratch {
		if r := i.regions[name]; r != nil && r.complete() {
			if _, was := state(r); !was {
				i.impl.regionComplete(name)
			}
		}
	}
	i.idle = true
	for _, r := range i.regions {
		if r.end || !r.complete() {
			i.idle = false
			break
		}
	}
	return nil
}

func (i *inspector) finish() {
	i.finished = true
	for _, r := range i.regions {
		if r.end {
			r.endDone = true
		}
	}
}

func (i *inspector) complete() bool {
	for _, r := range i.regions {
		if !r.complete() {
			return false
		}
	}
	return true
}

// Name is the format name (str(inspector)).
func (i *inspector) Name() string { return i.name }

func (i *inspector) safetyCheck() error {
	if !i.complete() {
		return formatError("Incomplete file cannot be safety checked")
	}
	if !i.impl.formatMatch() {
		return formatError("Unable to safety check format %s because content does not match", i.name)
	}
	failures := map[string]string{}
	for _, c := range i.checks {
		err := func() (err error) {
			defer func() {
				if p := recover(); p != nil {
					err = violation{"Unexpected error"}
				}
			}()
			return c.fn()
		}()
		var v violation
		var inner *SafetyCheckFailed
		switch {
		case err == nil:
		case errors.As(err, &inner):
			for n, f := range inner.Failures {
				failures["inner_"+n] = f
			}
		case errors.As(err, &v):
			failures[c.name] = v.msg
		default:
			failures[c.name] = "Unexpected error"
		}
	}
	if len(failures) > 0 {
		return &SafetyCheckFailed{failures}
	}
	return nil
}

func u16le(b []byte) uint64 { return uint64(binary.LittleEndian.Uint16(b)) }
func u32le(b []byte) uint64 { return uint64(binary.LittleEndian.Uint32(b)) }
func u64le(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }
func u32be(b []byte) uint64 { return uint64(binary.BigEndian.Uint32(b)) }
func u64be(b []byte) uint64 { return binary.BigEndian.Uint64(b) }

// need panics like struct.unpack on a short buffer.
func need(b []byte, n int) []byte {
	if len(b) < n {
		panic(fmt.Errorf("unpack requires a buffer of %d bytes", n))
	}
	return b
}

type base struct{ i *inspector }

func (base) postProcess()          {}
func (base) regionComplete(string) {}
func (b base) virtualSize() int64  { return b.i.total }

// raw

type raw struct{ base }

func (raw) formatMatch() bool { return true }

func newRaw() *inspector {
	i := newInspector("raw")
	i.impl = raw{base{i}}
	i.checks = []check{{"null", func() error { return nil }}}
	return i
}

// qcow2

type qcow struct {
	base
	header map[string]uint64
	magic  []byte
}

func newQcow() *inspector {
	i := newInspector("qcow2")
	q := &qcow{base: base{i}}
	i.impl = q
	i.newRegion("header", &region{offset: 0, length: 512})
	i.checks = []check{{"backing_file", q.checkBackingFile}, {"data_file", q.checkDataFile}, {"unknown_features", q.checkUnknownFeatures}}
	return i
}

func (q *qcow) regionComplete(string) {
	d := need(q.i.region("header").data, 32)
	q.magic = append([]byte(nil), d[:4]...)
	q.header = map[string]uint64{"version": u32be(d[4:8]), "bf_offset": u64be(d[8:16]), "bf_sz": u32be(d[16:20]), "cluster_bits": u32be(d[20:24]), "size": u64be(d[24:32])}
	if !q.formatMatch() {
		q.header, q.magic = nil, nil
	}
}

func (q *qcow) virtualSize() int64 { return int64(q.header["size"]) }

func (q *qcow) formatMatch() bool {
	return q.i.region("header").complete() && bytes.Equal(q.magic, []byte("QFI\xfb"))
}

func (q *qcow) checkBackingFile() error {
	if u64be(need(q.i.region("header").data, 16)[8:16]) != 0 {
		return violation{"Image has a backing file"}
	}
	return nil
}

func (q *qcow) checkUnknownFeatures() error {
	ver, ok := q.header["version"]
	if ok && ver == 2 {
		return nil
	} else if !ok || ver != 3 {
		return violation{"Unsupported qcow2 version"}
	}
	f := need(q.i.region("header").data, 0x50)[0x48:0x50]
	const maxBit = 4
	maxByte := maxBit / 8
	for i := 0; i < 8; i++ {
		byteNum := 7 - i
		mask := byte(0xff)
		if byteNum == maxByte {
			mask = (1 << (maxBit % 8)) - 1
		} else if byteNum > maxByte {
			mask = 0
		}
		if f[i]&^mask != 0 {
			return violation{"Unknown QCOW2 features found"}
		}
	}
	return nil
}

func (q *qcow) checkDataFile() error {
	f := need(q.i.region("header").data, 0x50)[0x48:0x50]
	byteN := 8 - 1 - 3/8
	bit := byte(1 << (3 - 1%8)) // as the original (operator precedence)
	if f[byteN]&bit != 0 {
		return violation{"Image has data_file set"}
	}
	return nil
}

// qed

type qed struct{ base }

func (q qed) formatMatch() bool {
	r := q.i.region("header")
	return r.complete() && bytes.HasPrefix(r.data, []byte("QED\x00"))
}

func newQED() *inspector {
	i := newInspector("qed")
	i.impl = qed{base{i}}
	i.newRegion("header", &region{offset: 0, length: 512})
	i.checks = []check{{"banned", func() error { return violation{"This file format is not allowed"} }}}
	return i
}

// vhd

type vhd struct{ base }

func (v vhd) formatMatch() bool {
	return bytes.HasPrefix(v.i.region("header").data, []byte("conectix"))
}

func (v vhd) virtualSize() int64 {
	r := v.i.region("header")
	if !r.complete() || !v.formatMatch() {
		return 0
	}
	return int64(u64be(r.data[40:48]))
}

func newVHD() *inspector {
	i := newInspector("vhd")
	i.impl = vhd{base{i}}
	i.newRegion("header", &region{offset: 0, length: 512})
	i.checks = []check{{"null", func() error { return nil }}}
	return i
}

// vhdx

const (
	vhdxMetaRegion = "8B7CA206-4790-4B9A-B8FE-575F050F886E"
	vhdxDiskSize   = "2FA54224-CD1B-4876-B211-5DBED83BF4B8"
	vhdxTableMax   = 32 * 2048
)

type vhdx struct{ base }

func newVHDX() *inspector {
	i := newInspector("vhdx")
	i.impl = &vhdx{base{i}}
	i.newRegion("ident", &region{offset: 0, length: 32})
	i.newRegion("header", &region{offset: 192 * 1024, length: 64 * 1024})
	i.checks = []check{{"null", func() error { return nil }}}
	return i
}

func guid(b []byte) string {
	b = need(b, 16)
	return fmt.Sprintf("%08X-%04X-%04X-%02X%02X-%02X%02X%02X%02X%02X%02X",
		u32le(b[0:4]), u16le(b[4:6]), u16le(b[6:8]), b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15])
}

func (v *vhdx) postProcess() {
	i := v.i
	if i.region("header").complete() && !i.has("metadata") {
		if r := v.findMetaRegion(); r != nil {
			i.newRegion("metadata", r)
		}
	} else if i.has("metadata") && !i.has("vds") {
		if r := v.findMetaEntry(vhdxDiskSize); r != nil {
			i.newRegion("vds", r)
		}
	}
}

func (v *vhdx) formatMatch() bool {
	return bytes.HasPrefix(v.i.region("ident").data, []byte("vhdxfile"))
}

func (v *vhdx) findMetaRegion() *region {
	h := v.i.region("header")
	d := need(h.data, 16)
	if u32le(d[0:4]) != 0x69676572 {
		panic(formatError("Region signature not found at %x", h.offset))
	}
	count := u32le(d[8:12])
	if count >= 2048 {
		panic(formatError("Region count is {int(count)} (limit 2047)"))
	}
	for n := uint64(0); n < count; n++ {
		start := 16 + n*32
		entry := need(d[min(start, uint64(len(d))):], 32)[:32]
		if guid(entry[:16]) == vhdxMetaRegion {
			return &region{offset: int64(u64le(entry[16:24])), length: 2048 * 32}
		}
	}
	return nil
}

func (v *vhdx) findMetaEntry(want string) *region {
	m := v.i.region("metadata")
	buf := m.data
	if len(buf) < 32 {
		return nil
	}
	if !bytes.Equal(buf[:8], []byte("metadata")) {
		panic(formatError("Invalid signature for metadata region: %q", buf[:8]))
	}
	count := u16le(buf[10:12])
	if uint64(len(buf)) < 32+count*32 {
		return nil
	}
	if count >= 2048 {
		panic(formatError("Metadata item count is {int(count)} (limit 2047)"))
	}
	for n := uint64(0); n < count; n++ {
		off := 32 + n*32
		if guid(buf[off:off+16]) == want {
			itemOffset, itemLength := u32le(buf[off+16:off+20]), u32le(buf[off+20:off+24])
			m.length = int64(len(buf))
			return &region{offset: m.offset + int64(itemOffset), length: int64(min(itemLength, vhdxTableMax))}
		}
	}
	return nil
}

func (v *vhdx) virtualSize() int64 {
	if !v.i.has("vds") || !v.i.region("vds").complete() {
		return 0
	}
	d := v.i.region("vds").data
	if len(d) != 8 {
		panic(fmt.Errorf("unpack requires a buffer of 8 bytes"))
	}
	return int64(u64le(d))
}

// vmdk

const (
	vmdkDescOffset = 0x200
	vmdkDescMax    = (1 << 20) - 1
	vmdkGDAtEnd    = 0xFFFFFFFFFFFFFFFF
	vmdkMinHeader  = 64
)

type vmdk struct {
	base
	descText *string
	vmdkType string
}

func newVMDK() *inspector {
	i := newInspector("vmdk")
	v := &vmdk{base: base{i}}
	i.impl = v
	i.newRegion("header", &region{offset: 0, length: 512, minLength: vmdkMinHeader})
	i.newRegion("descriptor", &region{offset: 0, length: vmdkDescMax, minLength: 4})
	i.checks = []check{{"descriptor", v.checkDescriptor}}
	return i
}

func (v *vmdk) sparseHeader(name string, offset int) (sig []byte, ver, descSec, descNum, gdOffset uint64) {
	d := need(v.i.region(name).data[min(offset, len(v.i.region(name).data)):], vmdkMinHeader)
	return d[:4], u32le(d[4:8]), u64le(d[28:36]), u64le(d[36:44]), u64le(d[56:64])
}

func (v *vmdk) postProcess() {
	i := v.i
	if !i.has("header") || !i.region("header").complete() {
		return
	}
	sig, ver, descSec, descNum, gd := v.sparseHeader("header", 0)
	isText := true
	for _, c := range i.region("header").data {
		if c >= 0x80 {
			isText = false
			break
		}
		r := rune(c)
		if !unicode.IsPrint(r) && !unicode.IsSpace(r) {
			isText = false
			break
		}
	}
	if !bytes.Equal(sig, []byte("KDMV")) {
		if isText {
			i.deleteRegion("header")
			return
		}
		panic(formatError("Signature KDMV not found: %q", sig))
	}
	if ver != 1 && ver != 2 && ver != 3 {
		panic(formatError("Unsupported format version {int(ver)}"))
	}
	if gd == vmdkGDAtEnd && !i.has("footer") {
		i.newRegion("footer", &region{offset: 1536, length: 1536, end: true})
		i.checks = append(i.checks, check{"footer", v.checkFooter})
	}
	descOffset := descSec * 512
	descSize := min(descNum*512, vmdkDescMax)
	if descOffset != vmdkDescOffset {
		panic(formatError("Wrong descriptor location"))
	}
	if i.region("descriptor").offset == 0 {
		i.deleteRegion("descriptor")
		i.newRegion("descriptor", &region{offset: int64(descOffset), length: int64(descSize)})
	}
}

func (v *vmdk) regionComplete(name string) {
	if name != "descriptor" {
		return
	}
	d := v.i.region("descriptor").data
	if n := bytes.IndexByte(d, 0); n >= 0 {
		d = d[:n]
	}
	for _, c := range d {
		if c >= 0x80 {
			return // not ASCII: the original logs and keeps no descriptor
		}
	}
	text := strings.ToLower(string(d))
	vmdkType := "formatnotfound"
	if n := strings.Index(text, `createtype="`); n >= 0 {
		start := n + len(`createtype="`)
		end := strings.Index(text[start:], `"`)
		if end < 0 {
			end = -1 - start // str.find returned -1
		}
		if end < 64 {
			switch {
			case end >= 0:
				vmdkType = text[start : start+end]
			case start < len(text)-1: // str.find gave -1: text[start:-1]
				vmdkType = text[start : len(text)-1]
			default:
				vmdkType = ""
			}
		}
	}
	v.descText, v.vmdkType = &text, vmdkType
}

func (v *vmdk) formatMatch() bool {
	if v.i.has("header") {
		return bytes.HasPrefix(v.i.region("header").data, []byte("KDMV"))
	}
	if v.descText == nil {
		panic(fmt.Errorf("'VMDKInspector' object has no attribute 'vmdktype'"))
	}
	return v.vmdkType != "formatnotfound"
}

func (v *vmdk) virtualSize() int64 {
	if v.descText == nil || *v.descText == "" {
		return 0
	}
	if v.vmdkType != "monolithicsparse" && v.vmdkType != "streamoptimized" {
		return 0
	}
	d := need(v.i.region("header").data, 44)
	return int64(u64le(d[12:20]) * 512)
}

func (v *vmdk) checkDescriptor() error {
	if v.descText == nil || *v.descText == "" {
		return violation{"No descriptor found"}
	}
	if v.vmdkType != "monolithicsparse" && v.vmdkType != "streamoptimized" {
		return violation{"Unsupported subformat"}
	}
	var extents []string
	for _, line := range strings.Split(*v.descText, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#") || line == "":
		case strings.HasPrefix(line, "ddb"):
		case strings.Contains(line, "=") && !strings.Contains(strings.SplitN(line, "=", 2)[0], " "):
		case line[:strings.IndexByte(line+" ", ' ')] == "rw" || line[:strings.IndexByte(line+" ", ' ')] == "rdonly" || line[:strings.IndexByte(line+" ", ' ')] == "noaccess":
			extents = append(extents, line)
		default:
			return violation{"Invalid VMDK descriptor data"}
		}
	}
	for _, e := range extents {
		if strings.Contains(e, "/") {
			return violation{"Invalid extent filenames found"}
		}
	}
	if len(extents) == 0 {
		return violation{"No extents found"}
	}
	return nil
}

func (v *vmdk) checkFooter() error {
	hs, hv, hds, hdn, _ := v.sparseHeader("header", 0)
	fs, fv, fds, fdn, fg := v.sparseHeader("footer", 512)
	if !bytes.Equal(hs, fs) {
		return violation{"Header and footer signature do not match"}
	}
	if hv != fv {
		return violation{"Header and footer versions do not match"}
	}
	if hds != fds || hdn != fdn {
		return violation{"Footer specifies a different descriptor than header"}
	}
	if fg == vmdkGDAtEnd {
		return violation{"Footer indicates another footer"}
	}
	d := v.i.region("footer").data
	pad := make([]byte, 496)
	m := need(d, 512)[:512]
	if u32le(m[8:12]) != 0 || u32le(m[12:16]) != 3 || !bytes.Equal(m[16:], pad) {
		return violation{"Footer marker is invalid"}
	}
	e := d[len(d)-512:]
	if u64le(e[0:8]) != 0 || u32le(e[8:12]) != 0 || u32le(e[12:16]) != 0 || !bytes.Equal(e[16:], pad) {
		return violation{"End-of-stream marker is invalid"}
	}
	return nil
}

// vdi

type vdi struct{ base }

func (v vdi) formatMatch() bool {
	r := v.i.region("header")
	return r.complete() && u32le(r.data[0x40:0x44]) == 0xBEDA107F
}

func (v vdi) virtualSize() int64 {
	if !v.formatMatch() {
		return 0
	}
	return int64(u64le(v.i.region("header").data[0x170:0x178]))
}

func newVDI() *inspector {
	i := newInspector("vdi")
	i.impl = vdi{base{i}}
	i.newRegion("header", &region{offset: 0, length: 512})
	i.checks = []check{{"null", func() error { return nil }}}
	return i
}

// iso

type iso struct{ base }

func (s iso) formatMatch() bool {
	if !s.i.complete() {
		return false
	}
	sig := s.i.region("header").data[1:6]
	return bytes.Equal(sig, []byte("CD001")) || bytes.Equal(sig, []byte("NSR02")) || bytes.Equal(sig, []byte("NSR03"))
}

func (s iso) virtualSize() int64 {
	if !s.i.complete() || !s.formatMatch() {
		return 0
	}
	h := s.i.region("header").data
	if h[0] != 1 {
		return 0
	}
	return int64(u32le(h[80:84]) * u16le(h[128:130]))
}

func newISO() *inspector {
	i := newInspector("iso")
	i.impl = iso{base{i}}
	i.newRegion("system_area", &region{offset: 0, length: 32 * 1024})
	i.newRegion("header", &region{offset: 32 * 1024, length: 2 * 1024})
	i.checks = []check{{"null", func() error { return nil }}}
	return i
}

// gpt

type gpt struct{ base }

func (g gpt) formatMatch() bool {
	r := g.i.region("mbr")
	if !r.complete() {
		return false
	}
	fat := r.data[0x10] == 2 && r.data[0x15] == 0xF8
	return u16le(r.data[510:512]) == 0xAA55 && !fat
}

func newGPT() *inspector {
	i := newInspector("gpt")
	g := gpt{base{i}}
	i.impl = g
	i.newRegion("mbr", &region{offset: 0, length: 512})
	i.checks = []check{{"mbr", g.checkMBR}}
	return i
}

func (g gpt) checkMBR() error {
	d := g.i.region("mbr").data
	var valid []int
	found := false
	for n := 0; n < 4; n++ {
		p := d[446+16*n : 446+16*n+16]
		boot, sh, ss, st, ostype, startLBA := p[0], p[1], p[2], p[3], p[4], u32le(p[8:12])
		if boot != 0 && boot != 0x80 {
			return violation{fmt.Sprintf("MBR PTE %d has invalid boot flag", n)}
		}
		if ostype != 0 {
			valid = append(valid, n)
		}
		if ostype == 0xEE {
			found = true
			if sh != 0 || ss != 2 || st != 0 {
				return violation{"GPT MBR has invalid start CHS"}
			}
			if startLBA != 1 {
				return violation{"GPT MBR has invalid start LBA"}
			}
		}
	}
	if found && !(len(valid) == 1 && valid[0] == 0) {
		return violation{"GPT MBR defines invalid extra partitions"}
	}
	if len(valid) == 0 {
		return violation{"GPT MBR has no partitions defined"}
	}
	return nil
}

// luks (no passphrase: Glance passes none, so no inner format)

type luks struct{ base }

func (l luks) formatMatch() bool {
	d := l.i.region("header").data
	return len(d) >= 6 && bytes.Equal(d[:6], []byte("LUKS\xba\xbe"))
}

func (l luks) virtualSize() int64 {
	d := need(l.i.region("header").data, 168)
	return l.i.total - int64(u32be(d[104:108]))*512
}

func newLUKS() *inspector {
	i := newInspector("luks")
	l := luks{base{i}}
	i.impl = l
	i.newRegion("header", &region{offset: 0, length: 592})
	i.checks = []check{
		{"version", func() error {
			if v := u16be(need(i.region("header").data, 8)[6:8]); v != 1 {
				return violation{fmt.Sprintf("LUKS version %d is not supported", v)}
			}
			return nil
		}},
		{"passphrase", func() error { return nil }},
		{"inner_safety", func() error { return nil }},
	}
	return i
}

func u16be(b []byte) uint64 { return uint64(binary.BigEndian.Uint16(b)) }
