package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"birdcam/internal/protocol"
)

// Discovered is a protocol.Camera plus agent-side information: which device
// node provides which codec (the H264 node and the MJPEG node of one camera
// are different /dev nodes).
type Discovered struct {
	protocol.Camera
	Nodes map[string]string `json:"nodes"` // codec -> device node
}

// Discoverer enumerates cameras. All roots are configurable for tests.
type Discoverer struct {
	ByIDDir    string // /dev/v4l/by-id
	SysRoot    string // /sys
	ProcAsound string // /proc/asound
	// Query returns the h264/mjpeg capture formats of a device node.
	Query func(path string) ([]protocol.VideoFormat, error)
}

// NewDiscoverer returns a Discoverer using the real system paths.
func NewDiscoverer() *Discoverer {
	return &Discoverer{
		ByIDDir:    "/dev/v4l/by-id",
		SysRoot:    "/sys",
		ProcAsound: "/proc/asound",
		Query:      EnumCaptureFormats,
	}
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns a by-id prefix into a stable camera id.
func Slug(prefix string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(prefix), "-"), "-")
}

const videoIndexSep = "-video-index"

// SplitByIDName splits "usb-Foo_Bar-video-index2" into ("usb-Foo_Bar", 2).
func SplitByIDName(name string) (prefix string, index int, ok bool) {
	i := strings.LastIndex(name, videoIndexSep)
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(name[i+len(videoIndexSep):])
	if err != nil {
		return "", 0, false
	}
	return name[:i], n, true
}

type byIDNode struct {
	link  string // by-id path
	real  string // resolved /dev/videoN
	index int
}

// Discover scans the by-id directory and returns cameras sorted by id.
func (d *Discoverer) Discover() []Discovered {
	links, _ := filepath.Glob(filepath.Join(d.ByIDDir, "*"+videoIndexSep+"*"))
	groups := map[string][]byIDNode{}
	for _, l := range links {
		prefix, idx, ok := SplitByIDName(filepath.Base(l))
		if !ok {
			continue
		}
		real, err := filepath.EvalSymlinks(l)
		if err != nil {
			continue
		}
		groups[prefix] = append(groups[prefix], byIDNode{link: l, real: real, index: idx})
	}

	var out []Discovered
	for prefix, nodes := range groups {
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].index < nodes[j].index })
		cam := Discovered{
			Camera: protocol.Camera{ID: Slug(prefix), Name: prefixName(prefix)},
			Nodes:  map[string]string{},
		}
		var firstNode, firstReal string
		for _, n := range nodes {
			formats, err := d.Query(n.link)
			if err != nil || len(formats) == 0 {
				continue // metadata node or unreadable
			}
			used := false
			for _, f := range formats {
				if _, dup := cam.Nodes[f.Codec]; dup {
					continue
				}
				used = true
				break
			}
			if !used {
				continue
			}
			if firstNode == "" {
				firstNode, firstReal = n.link, n.real
			}
			for _, f := range formats {
				if cam.Nodes[f.Codec] == "" || cam.Nodes[f.Codec] == n.link {
					cam.Nodes[f.Codec] = n.link
					cam.Formats = append(cam.Formats, f)
				}
			}
		}
		if len(cam.Formats) == 0 {
			continue
		}
		cam.VideoDevice = firstNode
		if p, ok := cam.Nodes["h264"]; ok {
			cam.VideoDevice = p
		}
		if n := d.sysName(firstReal); n != "" {
			cam.Name = n
		}
		cam.AudioDevice = d.FindALSA(firstReal)
		out = append(out, cam)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// prefixName makes a fallback display name from a by-id prefix.
func prefixName(prefix string) string {
	s := strings.TrimPrefix(prefix, "usb-")
	return strings.ReplaceAll(s, "_", " ")
}

// sysName returns the V4L2 card name ("H264 USB Camera: H264 USB Cam" -> "H264 USB Camera").
func (d *Discoverer) sysName(realNode string) string {
	if realNode == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(d.SysRoot, "class/video4linux", filepath.Base(realNode), "name"))
	if err != nil {
		return ""
	}
	n := strings.TrimSpace(string(b))
	if i := strings.Index(n, ": "); i > 0 {
		n = n[:i]
	}
	return strings.TrimSpace(n)
}

var cardDir = regexp.MustCompile(`^card(\d+)$`)

// FindALSA finds the ALSA capture device belonging to the same USB device as
// the given video node (e.g. "/dev/video0"), as "hw:CARD=<id>,DEV=0". Returns
// "" if there is none.
func (d *Discoverer) FindALSA(realNode string) string {
	if realNode == "" {
		return ""
	}
	devLink := filepath.Join(d.SysRoot, "class/video4linux", filepath.Base(realNode), "device")
	iface, err := filepath.EvalSymlinks(devLink)
	if err != nil {
		return ""
	}
	usbDev := filepath.Dir(iface) // interface "1-1.2:1.0" -> USB device "1-1.2"
	cards, _ := filepath.Glob(filepath.Join(usbDev, "*", "sound", "card*"))
	sort.Strings(cards)
	for _, c := range cards {
		m := cardDir.FindStringSubmatch(filepath.Base(c))
		if m == nil {
			continue
		}
		id, err := os.ReadFile(filepath.Join(d.ProcAsound, "card"+m[1], "id"))
		if err != nil {
			continue
		}
		if s := strings.TrimSpace(string(id)); s != "" {
			return "hw:CARD=" + s + ",DEV=0"
		}
	}
	return ""
}

// ApplyOverrides applies per-camera config (disabled, name, audio device).
func ApplyOverrides(cams []Discovered, cfg map[string]CameraConfig) []Discovered {
	out := make([]Discovered, 0, len(cams))
	for _, c := range cams {
		if oc, ok := cfg[c.ID]; ok {
			if oc.Disabled {
				continue
			}
			if oc.Name != "" {
				c.Name = oc.Name
			}
			c.Rotation = oc.Rotation
			switch oc.AudioDevice {
			case "":
			case "none":
				c.AudioDevice = ""
			default:
				c.AudioDevice = oc.AudioDevice
			}
		}
		out = append(out, c)
	}
	return out
}

// ProtocolCameras strips agent-side info.
func ProtocolCameras(cams []Discovered) []protocol.Camera {
	out := make([]protocol.Camera, 0, len(cams))
	for _, c := range cams {
		out = append(out, c.Camera)
	}
	return out
}

func camerasEqual(a, b []Discovered) bool { return reflect.DeepEqual(a, b) }
