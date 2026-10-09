package isobuild

import (
	"strings"
	"testing"
)

func TestSharedBootRecipes(t *testing.T) {
	arch, err := BootArguments("arch", "https://images.example/build/arch/x86_64/airootfs.sfs")
	if err != nil || !strings.Contains(strings.Join(arch, " "), "archiso_http_srv=https://images.example/build/ ip=dhcp") {
		t.Fatal(arch, err)
	}
	debian, err := BootArguments("debian", "https://images.example/build/live/filesystem.squashfs")
	joined := strings.Join(debian, " ")
	if err != nil || !strings.Contains(joined, "fetch=https://images.example/build/live/filesystem.squashfs") || strings.Contains(joined, "ip=dhcp") {
		t.Fatal(debian, err)
	}
	for _, args := range [][]string{arch, debian} {
		if !strings.Contains(strings.Join(args, " "), "BOOTIF=01-@BOOT_MAC@") {
			t.Fatal("missing shared MAC", args)
		}
	}
	if _, err := BootArguments("arch", "https://images.example/wrong"); err == nil {
		t.Fatal("bad Arch rootfs accepted")
	}
	if _, err := BootArguments("unknown", "https://images.example/root"); err == nil {
		t.Fatal("unsupported image accepted")
	}
}
