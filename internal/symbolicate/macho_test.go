package symbolicate

import (
	"encoding/binary"
	"testing"
)

func fakeMachO(magic uint32, bo binary.ByteOrder, withUUID bool) []byte {
	hdr := 28
	if magic == machoMagic64 {
		hdr = 32
	}
	b := make([]byte, hdr)
	bo.PutUint32(b, magic)
	cmds := uint32(1)
	if !withUUID {
		cmds = 0
	}
	bo.PutUint32(b[16:], cmds+1) // +1 for a dummy segment command first
	seg := make([]byte, 16)
	bo.PutUint32(seg, 0x19) // LC_SEGMENT
	bo.PutUint32(seg[4:], 16)
	b = append(b, seg...)
	if withUUID {
		lc := make([]byte, 24)
		bo.PutUint32(lc, lcUUID)
		bo.PutUint32(lc[4:], 24)
		copy(lc[8:], []byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88})
		b = append(b, lc...)
	}
	return b
}

func TestMachOUUID(t *testing.T) {
	want := "12345678-9abc-def0-1122-334455667788"
	if id, ok := MachOUUID(fakeMachO(machoMagic64, binary.LittleEndian, true)); !ok || id != want {
		t.Fatalf("64-bit LE: %q %v", id, ok)
	}
	if id, ok := MachOUUID(fakeMachO(machoMagic32, binary.BigEndian, true)); !ok || id != want {
		t.Fatalf("32-bit BE: %q %v", id, ok)
	}
	if _, ok := MachOUUID(fakeMachO(machoMagic64, binary.LittleEndian, false)); ok {
		t.Fatal("no LC_UUID should not resolve")
	}
	if _, ok := MachOUUID([]byte("not a binary at all")); ok {
		t.Fatal("garbage should not resolve")
	}
	// Fat: header + one arch entry pointing at an embedded thin image.
	thin := fakeMachO(machoMagic64, binary.LittleEndian, true)
	fat := make([]byte, 8+20)
	binary.BigEndian.PutUint32(fat, fatMagic)
	binary.BigEndian.PutUint32(fat[4:], 1)
	binary.BigEndian.PutUint32(fat[8+8:], uint32(len(fat)))
	binary.BigEndian.PutUint32(fat[8+12:], uint32(len(thin)))
	fat = append(fat, thin...)
	if id, ok := MachOUUID(fat); !ok || id != want {
		t.Fatalf("fat: %q %v", id, ok)
	}
}
