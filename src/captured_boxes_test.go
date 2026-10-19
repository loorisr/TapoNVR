package main

import (
	"encoding/hex"
	"testing"
)

// Real payloads captured from the go2rtc relay (cam0) and directly (cam1).
func TestParserAgainstCapturedPayloads(t *testing.T) {
	cases := []struct {
		name string
		hex  string
		want int
	}{
		// cam0: version 1, count 2, units MOTION + VEHICLE (complete NAL).
		{"cam0-motion+vehicle", "06052754504c494e4b4d41524b4552424f582e0102ffff0118df11171d241683061741101e1d3d15720080", 2},
		// cam0: empty marker record (count=0): valid SEI, no boxes.
		{"cam0-empty", "06051554504c494e4b4d41524b4552424f582e0100ffff0080", 0},
		// cam1: version 1, count 1, unit PET.
		{"cam1-pet", "06051e54504c494e4b4d41524b4552424f582e0101ffff050afa1db513c827100080", 1},
		// cam1: IPC-DETECTED-BOX heartbeat (rec[1]=0): valid SEI, no box.
		{"cam1-ipc-heartbeat", "06fffffe224950432d44455445435445442d424f580100ffff7000000300000300180393c2030080", 0},
	}
	for _, c := range cases {
		raw, err := hex.DecodeString(c.hex)
		if err != nil {
			t.Fatalf("%s: bad hex: %v", c.name, err)
		}
		var d seiRTPDepacketizer
		var boxes []detBox
		for _, n := range d.decode(raw) {
			boxes = append(boxes, seiBoxesFromNAL(n)...)
		}
		if len(boxes) != c.want {
			t.Fatalf("%s: boxes=%d want %d: %+v", c.name, len(boxes), c.want, boxes)
		}
	}
}
