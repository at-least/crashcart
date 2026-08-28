package symbolicate

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// Mach-O magic numbers and the load command we care about.
const (
	machoMagic64    = 0xfeedfacf
	machoMagic32    = 0xfeedface
	fatMagic        = 0xcafebabe
	lcUUID          = 0x1b
	machoHeaderSize = 28 // 32-bit header; 64-bit adds a 4-byte reserved field
)

// MachOUUID returns the LC_UUID of a Mach-O image as a lower-case dashed
// UUID — the debug_id Sentry SDKs put in debug_meta.images. Fat binaries
// yield the first architecture's UUID. ok=false when data is not Mach-O or
// carries no UUID.
func MachOUUID(data []byte) (string, bool) {
	if len(data) < 8 {
		return "", false
	}
	if binary.BigEndian.Uint32(data) == fatMagic {
		n := binary.BigEndian.Uint32(data[4:])
		for i := uint32(0); i < n && i < 32; i++ {
			off := 8 + int(i)*20
			if off+20 > len(data) {
				break
			}
			start := int(binary.BigEndian.Uint32(data[off+8:]))
			size := int(binary.BigEndian.Uint32(data[off+12:]))
			if start < 0 || size < 0 || start+size > len(data) {
				continue
			}
			if id, ok := MachOUUID(data[start : start+size]); ok {
				return id, true
			}
		}
		return "", false
	}
	var bo binary.ByteOrder = binary.LittleEndian
	magic := binary.LittleEndian.Uint32(data)
	if magic != machoMagic64 && magic != machoMagic32 {
		bo = binary.BigEndian
		magic = binary.BigEndian.Uint32(data)
		if magic != machoMagic64 && magic != machoMagic32 {
			return "", false
		}
	}
	hdr := machoHeaderSize
	if magic == machoMagic64 {
		hdr += 4
	}
	if len(data) < hdr {
		return "", false
	}
	ncmds := int(bo.Uint32(data[16:]))
	off := hdr
	for i := 0; i < ncmds && off+8 <= len(data); i++ {
		cmd := bo.Uint32(data[off:])
		size := int(bo.Uint32(data[off+4:]))
		if size < 8 || off+size > len(data) {
			return "", false
		}
		if cmd == lcUUID && size >= 24 {
			u := data[off+8 : off+24]
			return strings.ToLower(fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])), true
		}
		off += size
	}
	return "", false
}
