package symbolicate

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// fakeMachO builds a minimal 64-bit image: header, one LC_SEGMENT_64-sized
// dummy command and (optionally) an LC_UUID.
func fakeMachO(cpu uint32, uuid []byte) []byte {
	b := make([]byte, 32)
	binary.LittleEndian.PutUint32(b, 0xfeedfacf)
	binary.LittleEndian.PutUint32(b[4:], cpu)
	ncmds, size := uint32(1), uint32(16)
	if uuid != nil {
		ncmds, size = 2, 40
	}
	binary.LittleEndian.PutUint32(b[16:], ncmds)
	binary.LittleEndian.PutUint32(b[20:], size)
	seg := make([]byte, 16)
	binary.LittleEndian.PutUint32(seg, 0x1d) // LC_ID_DYLINKER-ish: unknown to debug/macho, kept raw
	binary.LittleEndian.PutUint32(seg[4:], 16)
	b = append(b, seg...)
	if uuid != nil {
		lc := make([]byte, 24)
		binary.LittleEndian.PutUint32(lc, uint32(lcUUID))
		binary.LittleEndian.PutUint32(lc[4:], 24)
		copy(lc[8:], uuid)
		b = append(b, lc...)
	}
	return b
}

// fakeMachOText builds a 64-bit image whose only load command is an
// LC_SEGMENT_64 named __TEXT at vmaddr (what the sidecar reads the load
// address from).
func fakeMachOText(cpu uint32, vmaddr uint64) []byte {
	b := make([]byte, 32)
	binary.LittleEndian.PutUint32(b, 0xfeedfacf)
	binary.LittleEndian.PutUint32(b[4:], cpu)
	binary.LittleEndian.PutUint32(b[16:], 1)
	binary.LittleEndian.PutUint32(b[20:], 72)
	seg := make([]byte, 72)
	binary.LittleEndian.PutUint32(seg, 0x19) // LC_SEGMENT_64
	binary.LittleEndian.PutUint32(seg[4:], 72)
	copy(seg[8:], "__TEXT")
	binary.LittleEndian.PutUint64(seg[24:], vmaddr)
	binary.LittleEndian.PutUint64(seg[32:], 0x1000)
	return append(b, seg...)
}

func fat(slices ...[]byte) []byte {
	hdr := make([]byte, 8+20*len(slices))
	binary.BigEndian.PutUint32(hdr, 0xcafebabe)
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(slices)))
	out := hdr
	off := len(hdr)
	for i, s := range slices {
		e := 8 + i*20
		binary.BigEndian.PutUint32(out[e:], binary.LittleEndian.Uint32(s[4:])) // cputype
		binary.BigEndian.PutUint32(out[e+8:], uint32(off))
		binary.BigEndian.PutUint32(out[e+12:], uint32(len(s)))
		off += len(s)
	}
	for _, s := range slices {
		out = append(out, s...)
	}
	return out
}

var (
	uuidA = []byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}
	uuidB = []byte{0xaa, 0xaa, 0xaa, 0xaa, 0xbb, 0xbb, 0xcc, 0xcc, 0xdd, 0xdd, 0xee, 0xee, 0xee, 0xee, 0xee, 0xee}
)

func TestLoadAddress(t *testing.T) {
	dir := t.TempDir()
	thin := filepath.Join(dir, "thin")
	os.WriteFile(thin, fakeMachOText(0x0100000c, 0x100000000), 0o644)
	if li := loadAddress(thin); li.base != 0x100000000 || li.arch != "" {
		t.Errorf("thin = %+v", li)
	}
	both := filepath.Join(dir, "fat")
	os.WriteFile(both, fat(fakeMachOText(0x01000007, 0x1000), fakeMachOText(0x0100000c, 0x100000000)), 0o644)
	if li := loadAddress(both); li.base != 0x100000000 || li.arch != "arm64" {
		t.Errorf("fat picks arm64: %+v", li)
	}
	other := filepath.Join(dir, "elf")
	os.WriteFile(other, []byte("\x7fELF not really"), 0o644)
	if li := loadAddress(other); li.base != 0 {
		t.Errorf("non-Mach-O = %+v", li)
	}
}

func TestMachOUUID(t *testing.T) {
	x86, arm := uint32(0x01000007), uint32(0x0100000c)
	if id, ok := MachOUUID(fakeMachO(x86, uuidA)); !ok || id != "12345678-9abc-def0-1122-334455667788" {
		t.Fatalf("thin: %q %v", id, ok)
	}
	if _, ok := MachOUUID(fakeMachO(x86, nil)); ok {
		t.Fatal("no LC_UUID should not resolve")
	}
	if _, ok := MachOUUID([]byte("not a binary at all")); ok {
		t.Fatal("garbage should not resolve")
	}
	// Fat: arm64 slice preferred over an earlier x86_64 one.
	if id, ok := MachOUUID(fat(fakeMachO(x86, uuidA), fakeMachO(arm, uuidB))); !ok || id != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatalf("fat arm64 preference: %q %v", id, ok)
	}
	// A fat header whose slice points back at the whole file must not loop.
	self := make([]byte, 28)
	binary.BigEndian.PutUint32(self, 0xcafebabe)
	binary.BigEndian.PutUint32(self[4:], 1)
	binary.BigEndian.PutUint32(self[8+12:], 28)
	if _, ok := MachOUUID(self); ok {
		t.Fatal("self-referencing fat header should not resolve")
	}
}
