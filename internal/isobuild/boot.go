package isobuild

import (
	"fmt"
	"strings"
)

// BootArguments is the single image recipe used by iPXE and live kexec handoff.
// The consumer substitutes the verified MAC into @BOOT_MAC@.
func BootArguments(distribution, rootfs string) ([]string, error) {
	switch distribution {
	case "arch":
		const suffix = "arch/x86_64/airootfs.sfs"
		if !strings.HasSuffix(rootfs, suffix) {
			return nil, fmt.Errorf("invalid Arch netboot rootfs path")
		}
		return []string{"archisobasedir=arch", "archiso_http_srv=" + strings.TrimSuffix(rootfs, suffix), "ip=dhcp", "net.ifnames=0", "BOOTIF=01-@BOOT_MAC@"}, nil
	case "debian":
		return []string{"boot=live", "components", "BOOTIF=01-@BOOT_MAC@", "fetch=" + rootfs}, nil
	default:
		return nil, fmt.Errorf("unsupported ISO netboot recipe")
	}
}
