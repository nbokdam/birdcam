package agent

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

// guidBytes encodes a GUID string in the on-wire mixed-endian layout.
func guidBytes(t *testing.T, g string) []byte {
	t.Helper()
	var f [5]uint64
	if _, err := fmt.Sscanf(g, "%x-%x-%x-%x-%x", &f[0], &f[1], &f[2], &f[3], &f[4]); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 16)
	binary.LittleEndian.PutUint32(b[0:], uint32(f[0]))
	binary.LittleEndian.PutUint16(b[4:], uint16(f[1]))
	binary.LittleEndian.PutUint16(b[6:], uint16(f[2]))
	binary.BigEndian.PutUint16(b[8:], uint16(f[3]))
	for i := 0; i < 6; i++ {
		b[10+i] = byte(f[4] >> (8 * (5 - i)))
	}
	return b
}

func xuDesc(t *testing.T, id uint8, guid string) []byte {
	d := []byte{26, 0x24, 0x06, id}
	d = append(d, guidBytes(t, guid)...)
	d = append(d, 0, 0, 0, 0, 0, 0) // bNumControls, nrInPins, ..., bControlSize... (filler)
	d[0] = byte(len(d))
	return d
}

func synthDescriptors(t *testing.T) []byte {
	var b []byte
	b = append(b, 9, 0x02, 0, 0, 3, 1, 0, 0x80, 50)                        // configuration
	b = append(b, 9, 0x04, 0, 0, 1, 14, 1, 0, 0)                           // VC interface
	b = append(b, 13, 0x24, 0x01, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0)            // VC header
	b = append(b, xuDesc(t, 3, SYSXUGUID)...)                              // SYS = unit 3
	b = append(b, xuDesc(t, 4, USRXUGUID)...)                              // USR = unit 4
	b = append(b, 9, 0x04, 1, 0, 0, 14, 2, 0, 0)                           // VS interface
	b = append(b, xuDesc(t, 9, "11111111-2222-3333-4444-555555555555")...) // must be ignored (not VC)
	return b
}

func TestParseXUUnits(t *testing.T) {
	got := ParseXUUnits(synthDescriptors(t))
	if len(got) != 2 || got[USRXUGUID] != 4 || got[SYSXUGUID] != 3 {
		t.Fatalf("got %v", got)
	}
	// the raw bytes for the USR GUID start with 94 73 df dd (little-endian first field)
	if !bytes.HasPrefix(guidBytes(t, USRXUGUID), []byte{0x94, 0x73, 0xdf, 0xdd, 0x3e, 0x97, 0x27, 0x47, 0xbe, 0xd9}) {
		t.Fatal("test helper endianness")
	}
	if len(ParseXUUnits([]byte{9, 4, 0})) != 0 || len(ParseXUUnits(nil)) != 0 {
		t.Fatal("garbage must give nothing")
	}
}

func TestFindXUUnit(t *testing.T) {
	root := t.TempDir()
	usb := filepath.Join(root, "devices/usb1/1-1")
	mk(t, filepath.Join(usb, "descriptors"), string(synthDescriptors(t)))
	mk(t, filepath.Join(usb, "1-1:1.0/x"), "")
	if err := os.MkdirAll(filepath.Join(root, "class/video4linux/video0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(usb, "1-1:1.0"), filepath.Join(root, "class/video4linux/video0/device")); err != nil {
		t.Fatal(err)
	}
	if u, ok := FindXUUnit(root, "video0", USRXUGUID); !ok || u != 4 {
		t.Fatal(u, ok)
	}
	if _, ok := FindXUUnit(root, "video9", USRXUGUID); ok {
		t.Fatal("missing device")
	}
}

func TestQueryStructLayout(t *testing.T) {
	var q uvcXUControlQuery
	if unsafe.Offsetof(q.Data) != 8 {
		t.Fatalf("data offset %d", unsafe.Offsetof(q.Data))
	}
	if unsafe.Sizeof(uintptr(0)) == 8 && (unsafe.Sizeof(q) != 16 || uvcIOCCtrlQuery != 0xC0107521) {
		t.Fatalf("size %d req %#x", unsafe.Sizeof(q), uvcIOCCtrlQuery)
	}
}

func TestEncodeBitrate(t *testing.T) {
	b, err := encodeBitrate(6_500_000)
	if err != nil || !bytes.Equal(b, []byte{0x63, 0x2e, 0xa0}) {
		t.Fatalf("%x %v", b, err)
	}
	b, _ = encodeBitrate(1_000_000)
	if !bytes.Equal(b, []byte{0x0f, 0x42, 0x40}) {
		t.Fatalf("%x", b)
	}
	for _, bad := range []int{0, 99_999, MaxBitrateBps + 1} {
		if _, err := encodeBitrate(bad); err == nil {
			t.Errorf("%d accepted", bad)
		}
	}
}

type xuCall struct {
	unit, sel uint8
	query     uint16
	data      []byte
}

// fakeXU emulates the USR XU of a Sonix camera and records all calls.
type fakeXU struct {
	calls   []xuCall
	rate    int
	selRate bool
	length  int
}

func (f *fakeXU) query(unit, sel uint8, query uint16, data []byte) error {
	f.calls = append(f.calls, xuCall{unit, sel, query, append([]byte(nil), data...)})
	switch query {
	case uvcGetLen:
		binary.LittleEndian.PutUint16(data, uint16(f.length))
	case uvcSetCur:
		if len(data) != f.length {
			return fmt.Errorf("bad length")
		}
		if data[0] == 0x9a && data[1] == 0x02 {
			f.selRate = true
		} else if f.selRate {
			f.rate = int(data[0])<<16 | int(data[1])<<8 | int(data[2])
			f.selRate = false
		}
	case uvcGetCur:
		data[0], data[1], data[2] = byte(f.rate>>16), byte(f.rate>>8), byte(f.rate)
	}
	return nil
}

func TestXUReadWrite(t *testing.T) {
	f := &fakeXU{rate: 6_500_000, length: 11}
	got, err := xuReadBitrate(f.query, 4)
	if err != nil || got != 6_500_000 {
		t.Fatal(got, err)
	}
	if err := xuWriteBitrate(f.query, 4, 2_000_000); err != nil {
		t.Fatal(err)
	}
	if got, _ := xuReadBitrate(f.query, 4); got != 2_000_000 {
		t.Fatal(got)
	}
	var sets [][]byte
	for _, c := range f.calls {
		if c.unit != 4 || c.sel != 2 {
			t.Errorf("call to unit %d sel %d", c.unit, c.sel)
		}
		if c.query == uvcSetCur {
			sets = append(sets, c.data)
		}
	}
	want := [][]byte{
		append([]byte{0x9a, 0x02}, make([]byte, 9)...), // read select
		append([]byte{0x9a, 0x02}, make([]byte, 9)...), // write select
		append([]byte{0x1e, 0x84, 0x80}, make([]byte, 8)...),
		append([]byte{0x9a, 0x02}, make([]byte, 9)...), // read select
	}
	if len(sets) != len(want) {
		t.Fatalf("sets %x", sets)
	}
	for i := range want {
		if !bytes.Equal(sets[i], want[i]) {
			t.Errorf("set %d = %x want %x", i, sets[i], want[i])
		}
	}
}

func TestXUSetCurNarrow(t *testing.T) {
	f := &fakeXU{length: 11}
	// SET_CUR must refuse anything but the select sub-command or a plausible bitrate.
	for _, p := range [][]byte{{0x9a, 0x01}, {0x00, 0x00}, {0xff, 0xff, 0xff, 0xff}, {0, 0, 1}, nil} {
		if err := xuSetCur(f.query, 4, 11, p); err == nil {
			t.Errorf("payload %x accepted", p)
		}
	}
	if len(f.calls) != 0 {
		t.Fatal("refused payload reached the device")
	}
	if err := xuWriteBitrate(f.query, 4, 50); err == nil || len(f.calls) != 0 {
		t.Fatal("out of range write reached the device")
	}
	// the SYS unit is never the target: the unit comes from the USR GUID lookup.
	units := ParseXUUnits(synthDescriptors(t))
	if units[USRXUGUID] == units[SYSXUGUID] {
		t.Fatal("USR and SYS unit must differ")
	}
}

func TestProbeCachedOncePerAppearance(t *testing.T) {
	d := fakeTree(t)
	n := 0
	d.ProbeXU = func(sys, node, real string) (XUProbe, bool) {
		n++
		return XUProbe{Unit: 4, Node: node, BitrateBps: 6_500_000}, true
	}
	for i := 0; i < 3; i++ {
		cams := d.Discover()
		var withCtl, h264 int
		for _, c := range cams {
			if c.NativeBitrateControl {
				withCtl++
				if c.XUUnit != 4 || c.XUBitrateBps != 6_500_000 {
					t.Errorf("%+v", c)
				}
			}
			if c.Nodes["h264"] != "" {
				h264++
			}
		}
		if withCtl != 1 || h264 != 1 {
			t.Fatalf("ctl %d h264 %d", withCtl, h264)
		}
	}
	if n != 1 {
		t.Fatalf("probed %d times", n)
	}
	// override disables
	off := false
	cams := ApplyOverrides(d.Discover(), map[string]CameraConfig{"usb-acme-cam-42": {NativeBitrateControl: &off}})
	if cams[0].NativeBitrateControl {
		t.Fatal("override ignored")
	}
}
