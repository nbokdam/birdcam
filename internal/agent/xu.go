//go:build linux

package agent

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Native H.264 bitrate control for Sonix-based UVC cameras (e.g. 32e4:9422)
// through the vendor "USR" UVC extension unit. See docs/PIPELINES.md section 6.
//
// SAFETY: the same cameras have a second Sonix XU ("SYS",
// {28f03370-6311-4a2e-ba2c-6890eb334016}) with flash-write commands. This file
// must never issue SET_CUR on any unit but the one found by the USR GUID, and
// the only SET_CUR in the whole agent is xuSetCur below, which accepts nothing
// but selector 2 with the 0x9a 0x02 sub-command select or a 3-byte bitrate.

// USRXUGUID and SYSXUGUID are the Sonix extension unit GUIDs.
const (
	USRXUGUID = "dddf7394-973e-4727-bed9-04ed6426dc67"
	SYSXUGUID = "28f03370-6311-4a2e-ba2c-6890eb334016" // never written to; listed only to document and test the restriction
)

const (
	uvcSetCur  = 0x01
	uvcGetCur  = 0x81
	uvcGetLen  = 0x85
	xuSelH264  = 2 // H264_CTRL selector of the USR XU
	xuSubRate  = 0x02
	xuSubMagic = 0x9a

	// MinBitrateBps / MaxBitrateBps bound plausible and settable bitrates
	// (the wire format is a 24-bit integer).
	MinBitrateBps = 100_000
	MaxBitrateBps = 0xFFFFFF
)

// xuQueryFunc performs one UVC_CTRL_QUERY on an already opened device.
type xuQueryFunc func(unit, selector uint8, query uint16, data []byte) error

// uvcXUControlQuery mirrors struct uvc_xu_control_query. The explicit pad puts
// the data pointer at offset 8 on both 32 and 64 bit (as C does).
type uvcXUControlQuery struct {
	Unit     uint8
	Selector uint8
	Query    uint16
	Size     uint16
	_        [2]byte
	Data     *byte
}

var uvcIOCCtrlQuery = iowr2(unsafe.Sizeof(uvcXUControlQuery{}), 'u', 0x21)

func iowr2(size uintptr, typ, nr uintptr) uintptr {
	const dirRW = 3
	return dirRW<<30 | size<<16 | typ<<8 | nr
}

// ioctlQuerier returns an xuQueryFunc bound to fd.
//
// Pointer safety: Data points into a Go-allocated []byte that is passed by the
// caller and stays reachable (runtime.KeepAlive) until after the syscall.
// Go's GC does not move heap objects, and the query struct is passed as the
// uintptr(unsafe.Pointer(...)) argument of unix.Syscall itself, which the
// compiler keeps alive and pinned for the duration of the call. Data is a real
// *byte, so the GC also sees it. The buffers are always non-empty heap slices.
func ioctlQuerier(fd int) xuQueryFunc {
	return func(unit, selector uint8, query uint16, data []byte) error {
		if len(data) == 0 {
			return errors.New("xu: empty buffer")
		}
		q := &uvcXUControlQuery{Unit: unit, Selector: selector, Query: query, Size: uint16(len(data)), Data: &data[0]}
		for {
			_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uvcIOCCtrlQuery, uintptr(unsafe.Pointer(q)))
			runtime.KeepAlive(q)
			runtime.KeepAlive(data)
			if e == unix.EINTR {
				continue
			}
			if e != 0 {
				return e
			}
			return nil
		}
	}
}

func xuGetLen(q xuQueryFunc, unit uint8) (int, error) {
	b := make([]byte, 2)
	if err := q(unit, xuSelH264, uvcGetLen, b); err != nil {
		return 0, err
	}
	n := int(binary.LittleEndian.Uint16(b))
	if n < 3 || n > 64 {
		return 0, fmt.Errorf("xu: unexpected H264_CTRL length %d", n)
	}
	return n, nil
}

// xuSetCur is the ONLY place that issues SET_CUR. It accepts nothing but
// selector 2 payloads of the form 9a 02 (select bitrate sub-command) or a
// 3-byte bitrate whose big-endian value is in the settable range. The payload
// is zero-padded to the control length n.
func xuSetCur(q xuQueryFunc, unit uint8, n int, payload []byte) error {
	isSelect := len(payload) == 2 && payload[0] == xuSubMagic && payload[1] == xuSubRate
	isValue := false
	if len(payload) == 3 {
		bps := int(payload[0])<<16 | int(payload[1])<<8 | int(payload[2])
		isValue = bps >= MinBitrateBps && bps <= MaxBitrateBps
	}
	if !isSelect && !isValue {
		return fmt.Errorf("xu: refusing SET_CUR with payload %x", payload)
	}
	buf := make([]byte, n)
	copy(buf, payload)
	return q(unit, xuSelH264, uvcSetCur, buf)
}

// xuReadBitrate reads the camera's current H.264 bitrate in bit/s.
func xuReadBitrate(q xuQueryFunc, unit uint8) (int, error) {
	n, err := xuGetLen(q, unit)
	if err != nil {
		return 0, err
	}
	if err := xuSetCur(q, unit, n, []byte{xuSubMagic, xuSubRate}); err != nil {
		return 0, err
	}
	buf := make([]byte, n)
	if err := q(unit, xuSelH264, uvcGetCur, buf); err != nil {
		return 0, err
	}
	return int(buf[0])<<16 | int(buf[1])<<8 | int(buf[2]), nil
}

// encodeBitrate returns the 3 big-endian bytes of a bitrate in bit/s.
func encodeBitrate(bps int) ([]byte, error) {
	if bps < MinBitrateBps || bps > MaxBitrateBps {
		return nil, fmt.Errorf("bitrate %d bit/s out of range [%d, %d]", bps, MinBitrateBps, MaxBitrateBps)
	}
	return []byte{byte(bps >> 16), byte(bps >> 8), byte(bps)}, nil
}

// xuWriteBitrate sets the camera's H.264 bitrate (RAM only; lost on replug).
func xuWriteBitrate(q xuQueryFunc, unit uint8, bps int) error {
	val, err := encodeBitrate(bps)
	if err != nil {
		return err
	}
	n, err := xuGetLen(q, unit)
	if err != nil {
		return err
	}
	if err := xuSetCur(q, unit, n, []byte{xuSubMagic, xuSubRate}); err != nil {
		return err
	}
	return xuSetCur(q, unit, n, val)
}

// XUProbe is the result of probing a camera for native bitrate control.
type XUProbe struct {
	Unit       uint8
	Node       string // device node used for the XU ioctls
	BitrateBps int    // value read back when probing
}

// ProbeBitrateXU checks whether the camera behind a video node has the USR XU
// and answers the bitrate read-back with a plausible value. Read-only apart
// from the harmless sub-command select. sysRoot is normally "/sys"; node is
// the device path to open, real its resolved /dev/videoN name.
func ProbeBitrateXU(sysRoot, node, real string) (XUProbe, bool) {
	unit, ok := FindXUUnit(sysRoot, filepath.Base(real), USRXUGUID)
	if !ok {
		return XUProbe{}, false
	}
	bps, err := readBitrateAt(node, unit)
	if err != nil || bps < MinBitrateBps || bps > MaxBitrateBps {
		return XUProbe{}, false
	}
	return XUProbe{Unit: unit, Node: node, BitrateBps: bps}, true
}

func readBitrateAt(node string, unit uint8) (int, error) {
	fd, err := unix.Open(node, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	return xuReadBitrate(ioctlQuerier(fd), unit)
}

// ApplyBitrate sets the camera's H.264 bitrate via the XU and returns the
// value read back afterwards.
func ApplyBitrate(node string, unit uint8, bps int) (int, error) {
	fd, err := unix.Open(node, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	q := ioctlQuerier(fd)
	if err := xuWriteBitrate(q, unit, bps); err != nil {
		return 0, err
	}
	return xuReadBitrate(q, unit)
}

// FindXUUnit locates the extension unit with the given GUID for a video
// device (e.g. "video0") from the USB configuration descriptors in sysfs.
func FindXUUnit(sysRoot, videoName, guid string) (uint8, bool) {
	iface, err := filepath.EvalSymlinks(filepath.Join(sysRoot, "class/video4linux", videoName, "device"))
	if err != nil {
		return 0, false
	}
	// The interface directory ("1-1.2:1.0") is a child of the USB device.
	desc, err := os.ReadFile(filepath.Join(filepath.Dir(iface), "descriptors"))
	if err != nil {
		return 0, false
	}
	u, ok := ParseXUUnits(desc)[guid]
	return u, ok
}
