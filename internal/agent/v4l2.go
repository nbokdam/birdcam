//go:build linux

package agent

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"unsafe"

	"golang.org/x/sys/unix"

	"birdcam/internal/protocol"
)

// Minimal V4L2 ioctl bindings (no cgo).

const (
	v4l2BufTypeVideoCapture = 1

	v4l2FrmsizeDiscrete = 1

	v4l2FrmivalDiscrete = 1
)

func fourcc(s string) uint32 {
	return uint32(s[0]) | uint32(s[1])<<8 | uint32(s[2])<<16 | uint32(s[3])<<24
}

var (
	pixH264 = fourcc("H264")
	pixMJPG = fourcc("MJPG")
)

type v4l2Fmtdesc struct {
	Index       uint32
	Type        uint32
	Flags       uint32
	Description [32]byte
	PixelFormat uint32
	MbusCode    uint32
	Reserved    [3]uint32
}

type v4l2Frmsizeenum struct {
	Index       uint32
	PixelFormat uint32
	Type        uint32
	Union       [24]byte
	Reserved    [2]uint32
}

type v4l2Frmivalenum struct {
	Index       uint32
	PixelFormat uint32
	Width       uint32
	Height      uint32
	Type        uint32
	Union       [24]byte
	Reserved    [2]uint32
}

func iowr(nr, size uintptr) uintptr {
	const dirRW = 3
	return dirRW<<30 | size<<16 | 'V'<<8 | nr
}

var (
	vidiocEnumFmt            = iowr(2, unsafe.Sizeof(v4l2Fmtdesc{}))
	vidiocEnumFramesizes     = iowr(74, unsafe.Sizeof(v4l2Frmsizeenum{}))
	vidiocEnumFrameintervals = iowr(75, unsafe.Sizeof(v4l2Frmivalenum{}))
)

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	for {
		_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
		if e == 0 {
			return nil
		}
		if e == unix.EINTR {
			continue
		}
		return e
	}
}

func le32(b []byte, off int) uint32 { return binary.NativeEndian.Uint32(b[off:]) }

var standardSizes = [][2]int{{160, 120}, {320, 240}, {640, 480}, {800, 600}, {1280, 720}, {1920, 1080}}
var standardFPS = []int{60, 30, 25, 15, 10, 5}

// EnumCaptureFormats queries the video capture formats of a V4L2 node and
// returns only h264 and mjpeg ones. A node without capture formats (e.g. a
// metadata node) yields an empty slice.
func EnumCaptureFormats(path string) ([]protocol.VideoFormat, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)

	var out []protocol.VideoFormat
	seen := map[string]bool{}
	for i := uint32(0); i < 64; i++ {
		d := v4l2Fmtdesc{Index: i, Type: v4l2BufTypeVideoCapture}
		if err := ioctl(fd, vidiocEnumFmt, unsafe.Pointer(&d)); err != nil {
			if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTTY) {
				break
			}
			return out, fmt.Errorf("ENUM_FMT %s: %w", path, err)
		}
		var codec string
		switch d.PixelFormat {
		case pixH264:
			codec = "h264"
		case pixMJPG:
			codec = "mjpeg"
		default:
			continue
		}
		for _, sz := range enumSizes(fd, d.PixelFormat) {
			key := fmt.Sprintf("%s/%dx%d", codec, sz[0], sz[1])
			if seen[key] {
				continue // some drivers list a size twice
			}
			seen[key] = true
			fps := enumFPS(fd, d.PixelFormat, sz[0], sz[1])
			out = append(out, protocol.VideoFormat{Codec: codec, Width: sz[0], Height: sz[1], FPS: fps})
		}
	}
	return out, nil
}

func enumSizes(fd int, pix uint32) [][2]int {
	var sizes [][2]int
	for i := uint32(0); i < 256; i++ {
		s := v4l2Frmsizeenum{Index: i, PixelFormat: pix}
		if err := ioctl(fd, vidiocEnumFramesizes, unsafe.Pointer(&s)); err != nil {
			break
		}
		if s.Type == v4l2FrmsizeDiscrete {
			sizes = append(sizes, [2]int{int(le32(s.Union[:], 0)), int(le32(s.Union[:], 4))})
			continue
		}
		// continuous / stepwise: offer the standard sizes inside the range.
		minW, maxW, stepW := int(le32(s.Union[:], 0)), int(le32(s.Union[:], 4)), int(le32(s.Union[:], 8))
		minH, maxH, stepH := int(le32(s.Union[:], 12)), int(le32(s.Union[:], 16)), int(le32(s.Union[:], 20))
		for _, c := range standardSizes {
			if c[0] < minW || c[0] > maxW || c[1] < minH || c[1] > maxH {
				continue
			}
			if stepW > 1 && (c[0]-minW)%stepW != 0 || stepH > 1 && (c[1]-minH)%stepH != 0 {
				continue
			}
			sizes = append(sizes, c)
		}
		break
	}
	return sizes
}

func enumFPS(fd int, pix uint32, w, h int) []int {
	set := map[int]bool{}
	for i := uint32(0); i < 256; i++ {
		s := v4l2Frmivalenum{Index: i, PixelFormat: pix, Width: uint32(w), Height: uint32(h)}
		if err := ioctl(fd, vidiocEnumFrameintervals, unsafe.Pointer(&s)); err != nil {
			break
		}
		if s.Type == v4l2FrmivalDiscrete {
			num, den := le32(s.Union[:], 0), le32(s.Union[:], 4)
			if num > 0 {
				set[int((den+num/2)/num)] = true
			}
			continue
		}
		// stepwise: fractions are min interval, max interval, step.
		minN, minD := le32(s.Union[:], 0), le32(s.Union[:], 4)
		maxN, maxD := le32(s.Union[:], 8), le32(s.Union[:], 12)
		if minN == 0 || maxN == 0 {
			break
		}
		hi := int(minD / minN) // shortest interval = highest fps
		lo := int(maxD / maxN)
		for _, f := range standardFPS {
			if f >= lo && f <= hi {
				set[f] = true
			}
		}
		break
	}
	fps := make([]int, 0, len(set))
	for f := range set {
		fps = append(fps, f)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(fps)))
	return fps
}
