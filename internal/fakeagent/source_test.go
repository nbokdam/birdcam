package fakeagent

import (
	"os"
	"testing"
)

func TestSplitAndGroup(t *testing.T) {
	stream := []byte{
		0, 0, 0, 1, 0x67, 1, 2, // SPS
		0, 0, 1, 0x68, 3, // PPS
		0, 0, 0, 1, 0x65, 0x88, 4, // IDR, first_mb=0
		0, 0, 1, 0x65, 0x10, 5, // IDR second slice (first_mb != 0)
		0, 0, 0, 1, 0x41, 0x9a, 6, // P frame
		0, 0, 1, 0x09, 0xf0, // AUD
		0, 0, 1, 0x41, 0x9a, 7, // P frame
	}
	nals := SplitNALs(stream)
	if len(nals) != 7 {
		t.Fatalf("got %d NALs: %x", len(nals), nals)
	}
	if nals[2][0] != 0x65 || len(nals[2]) != 3 {
		t.Fatalf("4-byte start code not stripped: %x", nals[2])
	}
	aus := GroupAccessUnits(nals)
	if len(aus) != 3 || len(aus[0]) != 4 || len(aus[1]) != 1 || len(aus[2]) != 2 {
		t.Fatalf("access units: %x", aus)
	}
}

func TestFileSource(t *testing.T) {
	path := os.Getenv("BIRDCAM_H264")
	if path == "" {
		t.Skip("set BIRDCAM_H264 to a raw H.264 capture")
	}
	src, err := FileSource(path)
	if err != nil {
		t.Fatal(err)
	}
	ls := src.(*loopSource)
	idr := 0
	for _, au := range ls.aus {
		for _, n := range SplitNALs(au) {
			if n[0]&0x1f == 5 {
				idr++
			}
		}
	}
	t.Logf("%d frames, %d IDR", len(ls.aus), idr)
	if idr == 0 || len(ls.aus) < 30 {
		t.Fatal("unexpected capture structure")
	}
}
