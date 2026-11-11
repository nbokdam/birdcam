package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"birdcam/internal/protocol"
)

// Discovered is a protocol.Camera plus agent-side information: which device
// node provides which codec (the H264 node and the MJPEG node of one camera
// are different /dev nodes).
type Discovered struct {
	protocol.Camera
	Nodes map[string]string `json:"nodes"` // codec -> device node

	// Native H.264 bitrate control via a Sonix UVC extension unit (agent-side
	// only; not part of the protocol). XUNode/XUUnit say where to send the
	// ioctls, XUBitrateBps is the value read back when the camera appeared.
	NativeBitrateControl bool   `json:"native_bitrate_control,omitempty"`
	XUNode               string `json:"xu_node,omitempty"`
	XUUnit               uint8  `json:"xu_unit,omitempty"`
	XUBitrateBps         int    `json:"xu_bitrate_bps,omitempty"`
}

// Discoverer enumerates cameras. All roots are configurable for tests.
type Discoverer struct {
	ByIDDir    string // /dev/v4l/by-id
	SysRoot    string // /sys
	ProcAsound string // /proc/asound
	// Query returns the h264/mjpeg capture formats of a device node.
	Query func(path string) ([]protocol.VideoFormat, error)
	// ProbeXU checks a camera with H.264 for native bitrate control (nil =
	// never). node is the by-id path to open, real the resolved /dev/videoN.
	ProbeXU func(sysRoot, node, real string) (XUProbe, bool)

	probeMu    sync.Mutex
	probeCache map[string]xuProbeEntry // camera id -> result, kept while the camera stays present
}

type xuProbeEntry struct {
	node string
	res  XUProbe
	ok   bool
}

// NewDiscoverer returns a Discoverer using the real system paths.
func NewDiscoverer() *Discoverer {
	return &Discoverer{
		ByIDDir:    "/dev/v4l/by-id",
		SysRoot:    "/sys",
		ProcAsound: "/proc/asound",
		Query:      EnumCaptureFormats,
		ProbeXU:    ProbeBitrateXU,
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
	present := map[string]bool{}
	defer func() { d.pruneProbes(present) }()
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
		present[cam.ID] = true
		if cam.Nodes["h264"] != "" {
			if p, ok := d.probe(cam.ID, firstNode, firstReal); ok {
				cam.NativeBitrateControl = true
				cam.XUNode, cam.XUUnit, cam.XUBitrateBps = p.Node, p.Unit, p.BitrateBps
			}
		}
		out = append(out, cam)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// probe runs ProbeXU once per camera appearance and caches the result (also a
// negative one), so the 10 s rescan does not poke the camera again.
func (d *Discoverer) probe(id, node, real string) (XUProbe, bool) {
	if d.ProbeXU == nil {
		return XUProbe{}, false
	}
	d.probeMu.Lock()
	defer d.probeMu.Unlock()
	if e, ok := d.probeCache[id]; ok && e.node == node {
		return e.res, e.ok
	}
	res, ok := d.ProbeXU(d.SysRoot, node, real)
	if d.probeCache == nil {
		d.probeCache = map[string]xuProbeEntry{}
	}
	d.probeCache[id] = xuProbeEntry{node: node, res: res, ok: ok}
	return res, ok
}

// pruneProbes forgets cameras that vanished, so a replug is probed again.
func (d *Discoverer) pruneProbes(present map[string]bool) {
	d.probeMu.Lock()
	defer d.probeMu.Unlock()
	for id := range d.probeCache {
		if !present[id] {
			delete(d.probeCache, id)
		}
	}
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
			if oc.NativeBitrateControl != nil && !*oc.NativeBitrateControl {
				c.NativeBitrateControl = false
			}
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
