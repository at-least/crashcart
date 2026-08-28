package symbolicate

import (
	"bytes"
	"debug/macho"
	"fmt"
	"strings"
)

// lcUUID is the LC_UUID load command; debug/macho exposes it only as raw bytes.
const lcUUID macho.LoadCmd = 0x1b

// MachOUUID returns the LC_UUID of a Mach-O image as a lower-case dashed
// UUID — the debug_id Sentry SDKs put in debug_meta.images. For fat
// (universal) binaries the arm64 slice wins, else the first slice with a
// UUID. ok=false when data is not Mach-O or carries no UUID.
func MachOUUID(data []byte) (string, bool) {
	if fat, err := macho.NewFatFile(bytes.NewReader(data)); err == nil {
		defer fat.Close()
		var first string
		for _, a := range fat.Arches {
			id, ok := fileUUID(a.File)
			if !ok {
				continue
			}
			if a.Cpu == macho.CpuArm64 {
				return id, true
			}
			if first == "" {
				first = id
			}
		}
		return first, first != ""
	}
	f, err := macho.NewFile(bytes.NewReader(data))
	if err != nil {
		return "", false
	}
	defer f.Close()
	return fileUUID(f)
}

// IsMachO reports whether data parses as a thin or fat Mach-O image.
func IsMachO(data []byte) bool {
	if f, err := macho.NewFatFile(bytes.NewReader(data)); err == nil {
		f.Close()
		return true
	}
	f, err := macho.NewFile(bytes.NewReader(data))
	if err != nil {
		return false
	}
	f.Close()
	return true
}

func fileUUID(f *macho.File) (string, bool) {
	for _, l := range f.Loads {
		raw := l.Raw()
		if len(raw) < 24 || macho.LoadCmd(f.ByteOrder.Uint32(raw)) != lcUUID {
			continue
		}
		u := raw[8:24]
		return strings.ToLower(fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])), true
	}
	return "", false
}
