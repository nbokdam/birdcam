package agent

import "fmt"

// ParseXUUnits parses raw USB configuration descriptors (sysfs "descriptors")
// and returns the UVC extension units found in video-control interfaces as a
// map from lowercase GUID string to bUnitID.
func ParseXUUnits(desc []byte) map[string]uint8 {
	out := map[string]uint8{}
	inVC := false
	for i := 0; i+2 <= len(desc); {
		l := int(desc[i])
		if l < 2 || i+l > len(desc) {
			break
		}
		d := desc[i : i+l]
		switch d[1] {
		case 0x04: // interface descriptor
			// bInterfaceClass 14 (video), subclass 1 (video control)
			inVC = l >= 9 && d[5] == 14 && d[6] == 1
		case 0x24: // class-specific interface
			// VC_EXTENSION_UNIT: bUnitID @3, guidExtensionCode @4..20
			if inVC && l >= 20 && d[2] == 0x06 {
				out[formatGUID(d[4:20])] = d[3]
			}
		}
		i += l
	}
	return out
}

// formatGUID renders 16 bytes in UVC/Microsoft mixed-endian order: the first
// three fields are little-endian, the rest are as stored.
func formatGUID(b []byte) string {
	return fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		b[3], b[2], b[1], b[0], b[5], b[4], b[7], b[6], b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15])
}
