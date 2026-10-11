package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/asdf57/stigmergy/internal/resource"
	"github.com/asdf57/stigmergy/internal/store"
)

type partitionReference struct {
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
}

type partitionSpec struct {
	ServerRef struct {
		Name string `json:"name"`
		UID  string `json:"uid"`
	} `json:"serverRef"`
	DiskSelector struct {
		Serial string `json:"serial,omitempty"`
		WWN    string `json:"wwn,omitempty"`
	} `json:"diskSelector"`
	Number   int `json:"number"`
	Capacity struct {
		Storage      string `json:"storage,omitempty"`
		UseRemaining bool   `json:"useRemaining,omitempty"`
	} `json:"capacity"`
	Role       string `json:"role"`
	Filesystem string `json:"filesystem"`
	MountPoint string `json:"mountPoint,omitempty"`
}

type partitionPlan struct {
	PartitionRef partitionReference `json:"partitionRef"`
	Spec         partitionSpec      `json:"spec"`
	StartMiB     int64              `json:"startMiB"`
	EndMiB       int64              `json:"endMiB"`
}

type storagePlan struct {
	Partitions []partitionPlan `json:"partitions"`
}

// Storage quantities deliberately support whole Mi/Gi/Ti only. Overflow and
// fractional values are rejected rather than silently rounding disk allocation.
func capacityMiB(quantity string) (int64, error) {
	if len(quantity) < 3 {
		return 0, fmt.Errorf("capacity requires a positive whole Mi, Gi or Ti quantity")
	}
	multiplier := int64(0)
	switch quantity[len(quantity)-2:] {
	case "Mi":
		multiplier = 1
	case "Gi":
		multiplier = 1024
	case "Ti":
		multiplier = 1024 * 1024
	}
	count, err := strconv.ParseInt(quantity[:len(quantity)-2], 10, 64)
	if err != nil || count <= 0 || multiplier == 0 || count > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("invalid storage capacity %q", quantity)
	}
	return count * multiplier, nil
}

func decodePartition(spec map[string]any) (partitionSpec, error) {
	var result partitionSpec
	encoded, err := json.Marshal(spec)
	if err != nil {
		return result, err
	}
	err = json.Unmarshal(encoded, &result)
	return result, err
}

// Individual resource validation is independent of discovery or installation.
// Cross-resource conflicts are checked against the explicitly reviewed run set.
func validatePartition(spec partitionSpec) error {
	if spec.ServerRef.Name == "" {
		return fmt.Errorf("serverRef requires a name")
	}
	if spec.DiskSelector.Serial == "" && spec.DiskSelector.WWN == "" {
		return fmt.Errorf("diskSelector requires serial or WWN")
	}
	if spec.Number < 1 || spec.Number > 128 {
		return fmt.Errorf("partition number must be between 1 and 128")
	}
	if (spec.Capacity.Storage != "") == spec.Capacity.UseRemaining {
		return fmt.Errorf("choose fixed capacity.storage or capacity.useRemaining")
	}
	if spec.Capacity.Storage != "" {
		if _, err := capacityMiB(spec.Capacity.Storage); err != nil {
			return err
		}
	}
	switch spec.Role {
	case "root":
		if spec.Filesystem != "ext4" || spec.MountPoint != "/" {
			return fmt.Errorf("root requires ext4 mounted at /")
		}
	case "efi":
		if spec.Filesystem != "vfat" || spec.MountPoint != "/boot/efi" {
			return fmt.Errorf("EFI requires vfat mounted at /boot/efi")
		}
	case "swap":
		if spec.Filesystem != "swap" || spec.MountPoint != "" {
			return fmt.Errorf("swap must have no mountPoint")
		}
	case "data":
		if spec.Filesystem != "ext4" || spec.MountPoint == "" || spec.MountPoint == "/" || spec.MountPoint == "/boot" || spec.MountPoint == "/boot/efi" {
			return fmt.Errorf("data requires ext4 at a non-boot mountPoint")
		}
		for _, reserved := range []string{"/dev", "/proc", "/sys", "/run", "/boot/efi"} {
			if spec.MountPoint == reserved || strings.HasPrefix(spec.MountPoint, reserved+"/") {
				return fmt.Errorf("data mountPoint overlaps a reserved system mount")
			}
		}
	default:
		return fmt.Errorf("unsupported partition role")
	}
	if spec.MountPoint != "" && (!strings.HasPrefix(spec.MountPoint, "/") || path.Clean(spec.MountPoint) != spec.MountPoint || strings.ContainsAny(spec.MountPoint, " \t\r\n\\")) {
		return fmt.Errorf("mountPoint must be a clean absolute path without whitespace")
	}
	return nil
}

// Pin only the resources explicitly confirmed by the user. Subsequent edits or
// deletion describe future state; they cannot change this immutable disk plan.
func (s *Server) resolveStoragePlan(ctx context.Context, run, server, machine resource.Resource, disk map[string]any) (storagePlan, []resource.Resource, error) {
	var request struct {
		PartitionRefs []partitionReference `json:"partitionRefs"`
	}
	encoded, err := json.Marshal(run.Spec["storage"])
	if err != nil {
		return storagePlan{}, nil, err
	}
	if err = json.Unmarshal(encoded, &request); err != nil {
		return storagePlan{}, nil, err
	}
	if len(request.PartitionRefs) < 2 || len(request.PartitionRefs) > 64 {
		return storagePlan{}, nil, fmt.Errorf("review 2 to 64 DiskPartition resources")
	}
	plan := storagePlan{}
	dependencies := []resource.Resource{machine}
	names, numbers, mounts := map[string]bool{}, map[int]bool{}, map[string]bool{}
	roots, efis := 0, 0
	inventory, _ := object(machine.Status["inventory"])["storage"].([]any)
	for _, ref := range request.PartitionRefs {
		if names[ref.Name] {
			return plan, nil, fmt.Errorf("duplicate partition reference")
		}
		names[ref.Name] = true
		item, readErr := s.store.Get(ctx, "DiskPartition", ref.Name)
		if readErr != nil {
			return plan, nil, readErr
		}
		if item.Metadata.UID != ref.UID || item.Metadata.Generation != ref.Generation || item.Metadata.DeletionTimestamp != nil {
			return plan, nil, fmt.Errorf("%w: partition %s changed", store.ErrConflict, ref.Name)
		}
		spec, decodeErr := decodePartition(item.Spec)
		if decodeErr != nil {
			return plan, nil, decodeErr
		}
		if err = validatePartition(spec); err != nil {
			return plan, nil, err
		}
		if spec.ServerRef.Name != server.Metadata.Name || (spec.ServerRef.UID != "" && spec.ServerRef.UID != server.Metadata.UID) {
			return plan, nil, fmt.Errorf("partition belongs to another Server")
		}
		spec.ServerRef.UID = server.Metadata.UID
		matches := 0
		for _, raw := range inventory {
			candidate := object(raw)
			if spec.DiskSelector.Serial != "" && candidate["serial"] != spec.DiskSelector.Serial {
				continue
			}
			wwn, _ := candidate["wwn"].(string)
			if spec.DiskSelector.WWN != "" && !strings.EqualFold(wwn, spec.DiskSelector.WWN) {
				continue
			}
			matches++
			if diskDeviceID(candidate) != diskDeviceID(disk) {
				return plan, nil, fmt.Errorf("partition selector does not match the approved disk")
			}
		}
		if matches != 1 {
			return plan, nil, fmt.Errorf("partition selector is missing or ambiguous")
		}
		if numbers[spec.Number] || (spec.MountPoint != "" && mounts[spec.MountPoint]) {
			return plan, nil, fmt.Errorf("duplicate partition number or mountPoint")
		}
		numbers[spec.Number] = true
		mounts[spec.MountPoint] = true
		if spec.Role == "root" {
			roots++
		}
		if spec.Role == "efi" {
			efis++
		}
		plan.Partitions = append(plan.Partitions, partitionPlan{PartitionRef: ref, Spec: spec})
		dependencies = append(dependencies, item)
	}
	if roots != 1 || efis != 1 {
		return plan, nil, fmt.Errorf("UEFI installation requires exactly one root and one EFI partition")
	}
	sort.Slice(plan.Partitions, func(i, j int) bool { return plan.Partitions[i].Spec.Number < plan.Partitions[j].Spec.Number })
	sizeJSON, _ := json.Marshal(disk["size"])
	diskBytes, err := strconv.ParseInt(string(sizeJSON), 10, 64)
	if err != nil {
		return plan, nil, err
	}
	// Reserve the first MiB and the final MiB for alignment and GPT metadata.
	limit := diskBytes/(1024*1024) - 1
	start := int64(1)
	for index := range plan.Partitions {
		part := &plan.Partitions[index]
		part.StartMiB = start
		if part.Spec.Capacity.UseRemaining {
			if index != len(plan.Partitions)-1 {
				return plan, nil, fmt.Errorf("only the final partition can use remaining capacity")
			}
			part.EndMiB = limit
		} else {
			size, parseErr := capacityMiB(part.Spec.Capacity.Storage)
			if parseErr != nil {
				return plan, nil, parseErr
			}
			if size > limit-start {
				return plan, nil, fmt.Errorf("partition capacities exceed disk capacity")
			}
			part.EndMiB = start + size
		}
		if part.EndMiB <= start || part.EndMiB > limit {
			return plan, nil, fmt.Errorf("layout does not fit disk")
		}
		if part.Spec.Role == "efi" && part.EndMiB-start < 260 {
			return plan, nil, fmt.Errorf("EFI partition requires at least 260Mi")
		}
		if part.Spec.Role == "root" && part.EndMiB-start < 1024 {
			return plan, nil, fmt.Errorf("root partition requires at least 1Gi")
		}
		start = part.EndMiB
	}
	return plan, dependencies, nil
}
