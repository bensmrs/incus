package drivers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	internalInstance "github.com/lxc/incus/v6/internal/instance"
	"github.com/lxc/incus/v6/internal/linux"
	"github.com/lxc/incus/v6/internal/server/locking"
	"github.com/lxc/incus/v6/internal/server/operations"
	"github.com/lxc/incus/v6/internal/version"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/lxc/incus/v6/shared/logger"
	"github.com/lxc/incus/v6/shared/revert"
	"github.com/lxc/incus/v6/shared/subprocess"
	"github.com/lxc/incus/v6/shared/units"
	"github.com/lxc/incus/v6/shared/util"
)

// lvmBlockVolSuffix suffix used for block content type volumes.
const lvmBlockVolSuffix = ".block"

// lvmISOVolSuffix suffix used for iso content type volumes.
const lvmISOVolSuffix = ".iso"

// lvmSnapshotSeparator separator character used between volume name and snapshot name in logical volume names.
const lvmSnapshotSeparator = "-"

// lvmEscapedHyphen used to escape hyphens in volume names to avoid conflicts with lvmSnapshotSeparator.
const lvmEscapedHyphen = "--"

// lvmThinpoolDefaultName is the default name for the thinpool volume.
const lvmThinpoolDefaultName = "IncusThinPool"

type lvmSourceType int

const (
	lvmSourceTypeUnknown lvmSourceType = iota
	lvmSourceTypeDefault
	lvmSourceTypePhysicalDevice
	lvmSourceTypeVolumeGroup
)

// usesThinpool indicates whether the config specifies to use a thin pool or not.
func (d *lvm) usesThinpool() bool {
	// No thin pool on clustered LVM.
	if d.clustered {
		return false
	}

	// Default is to use a thinpool.
	return util.IsTrueOrEmpty(d.config["lvm.use_thinpool"])
}

// thinpoolName returns the thinpool volume to use.
func (d *lvm) thinpoolName() string {
	if d.config["lvm.thinpool_name"] != "" {
		return d.config["lvm.thinpool_name"]
	}

	return lvmThinpoolDefaultName
}

// openLoopFile opens a loop device and returns the device path.
func (d *lvm) openLoopFile(source string) (string, error) {
	if source == "" {
		return "", errors.New("No source property found for the storage pool")
	}

	if filepath.IsAbs(source) && !linux.IsBlockdevPath(source) {
		unlock, err := locking.Lock(context.TODO(), OperationLockName("openLoopFile", d.name, "", "", ""))
		if err != nil {
			return "", err
		}

		defer unlock()

		loopDeviceName, err := loopDeviceSetup(source)
		if err != nil {
			return "", err
		}

		return loopDeviceName, nil
	}

	return "", errors.New("Source is not loop file")
}

// isLVMNotFoundExitError checks whether the supplied error is an exit error from an LVM command
// meaning that the object was not found. Returns true if it is (exit status 5) false if not.
func (d *lvm) isLVMNotFoundExitError(err error) bool {
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		if exitError.ExitCode() == 5 {
			return true
		}
	}

	return false
}

// pysicalVolumeExists checks if an LVM Physical Volume exists.
func (d *lvm) pysicalVolumeExists(pvName string) (bool, error) {
	_, err := subprocess.RunCommand("pvs", "--noheadings", "-o", "pv_name", pvName)
	if err != nil {
		if d.isLVMNotFoundExitError(err) {
			return false, nil
		}

		return false, fmt.Errorf("Error checking for LVM physical volume %q: %w", pvName, err)
	}

	return true, nil
}

// volumeGroupExists checks if an LVM Volume Group exists and returns any tags on that volume group.
func (d *lvm) volumeGroupExists(vgName string) (bool, []string, error) {
	output, err := subprocess.RunCommand("vgs", "--noheadings", "-o", "vg_tags", vgName)
	if err != nil {
		if d.isLVMNotFoundExitError(err) {
			return false, nil, nil
		}

		return false, nil, fmt.Errorf("Error checking for LVM volume group %q: %w", vgName, err)
	}

	output = strings.TrimSpace(output)
	tags := strings.SplitN(output, ",", -1)

	return true, tags, nil
}

// volumeGroupExtentSize gets the volume group's physical extent size in bytes.
func (d *lvm) volumeGroupExtentSize(vgName string) (int64, error) {
	// Look for cached value.
	lvmExtentSizeMu.Lock()
	defer lvmExtentSizeMu.Unlock()

	if lvmExtentSize == nil {
		lvmExtentSize = map[string]int64{}
	} else if lvmExtentSize[d.name] > 0 {
		return lvmExtentSize[d.name], nil
	}

	output, err := subprocess.TryRunCommand("vgs", "--noheadings", "--nosuffix", "--units", "b", "-o", "vg_extent_size", vgName)
	if err != nil {
		if d.isLVMNotFoundExitError(err) {
			return -1, api.StatusErrorf(http.StatusNotFound, "LVM volume group not found")
		}

		return -1, err
	}

	output = strings.TrimSpace(output)
	val, err := strconv.ParseInt(output, 10, 64)
	if err != nil {
		return -1, err
	}

	lvmExtentSize[d.name] = val

	return val, nil
}

// countLogicalVolumes gets the count of volumes (both normal and thin) in a volume group.
func (d *lvm) countLogicalVolumes(vgName string) (int, error) {
	output, err := subprocess.TryRunCommand("vgs", "--noheadings", "-o", "lv_count", vgName)
	if err != nil {
		if d.isLVMNotFoundExitError(err) {
			return -1, api.StatusErrorf(http.StatusNotFound, "LVM volume group not found")
		}

		return -1, fmt.Errorf("Error counting logical volumes in LVM volume group %q: %w", vgName, err)
	}

	output = strings.TrimSpace(output)
	return strconv.Atoi(output)
}

// countThinVolumes gets the count of thin volumes in a thin pool.
func (d *lvm) countThinVolumes(vgName, poolName string) (int, error) {
	output, err := subprocess.TryRunCommand("lvs", "--noheadings", "-o", "thin_count", fmt.Sprintf("%s/%s", vgName, poolName))
	if err != nil {
		if d.isLVMNotFoundExitError(err) {
			return -1, api.StatusErrorf(http.StatusNotFound, "LVM volume group not found")
		}

		return -1, fmt.Errorf("Error counting thin volumes in LVM volume group %q: %w", vgName, err)
	}

	output = strings.TrimSpace(output)
	return strconv.Atoi(output)
}

// thinpoolExists checks whether the specified thinpool exists in a volume group.
func (d *lvm) thinpoolExists(vgName string, poolName string) (bool, error) {
	output, err := subprocess.RunCommand("lvs", "--noheadings", "-o", "lv_attr", fmt.Sprintf("%s/%s", vgName, poolName))
	if err != nil {
		if d.isLVMNotFoundExitError(err) {
			return false, nil
		}

		return false, fmt.Errorf("Error checking for LVM thin pool %q: %w", poolName, err)
	}

	// Found LV named poolname, check type:
	attrs := strings.TrimSpace(string(output[:]))
	if strings.HasPrefix(attrs, "t") {
		return true, nil
	}

	return false, fmt.Errorf("LVM volume named %q exists but is not a thin pool", poolName)
}

// logicalVolumeExists checks whether the specified logical volume exists.
func (d *lvm) logicalVolumeExists(volDevPath string) (bool, error) {
	_, err := subprocess.RunCommand("lvs", "--noheadings", "-o", "lv_name", volDevPath)
	if err != nil {
		if d.isLVMNotFoundExitError(err) {
			return false, nil
		}

		return false, fmt.Errorf("Error checking for LVM logical volume %q: %w", volDevPath, err)
	}

	return true, nil
}

// createDefaultThinPool creates the default thinpool in the pool's volume group.
// If thinpoolSizeBytes >0 will manually set the thinpool volume size. Otherwise it will use 100% of the free space
// in the volume group.
// If pool lvm.thinpool_metadata_size setting >0 will manually set metadata size for the thinpool, otherwise LVM
// will pick an appropriate size.
func (d *lvm) createDefaultThinPool(lvmVersion, thinPoolName string, thinpoolSizeBytes int64) error {
	isRecent, err := d.lvmVersionIsAtLeast(lvmVersion, "2.02.99")
	if err != nil {
		return fmt.Errorf("Error checking LVM version: %w", err)
	}

	lvmThinPool := fmt.Sprintf("%s/%s", d.config["lvm.vg_name"], thinPoolName)

	args := []string{
		"--yes",
		"--wipesignatures", "y",
		"--thinpool", lvmThinPool,
	}

	thinpoolMetadataSizeBytes, err := d.roundedSizeBytesString(d.config["lvm.thinpool_metadata_size"])
	if err != nil {
		return fmt.Errorf("Invalid lvm.thinpool_metadata_size: %w", err)
	}

	if thinpoolMetadataSizeBytes > 0 {
		args = append(args, "--poolmetadatasize", fmt.Sprintf("%db", thinpoolMetadataSizeBytes))
	}

	if thinpoolSizeBytes > 0 {
		args = append(args, "--size", fmt.Sprintf("%db", thinpoolSizeBytes))
	} else if isRecent {
		args = append(args, "--extents", "100%FREE")
	} else {
		args = append(args, "--size", "1G")
	}

	// Because the thin pool is created as an LVM volume, if the volume stripes option is set we need to apply
	// it to the thin pool volume, as it cannot be applied to the thin volumes themselves.
	if d.config["volume.lvm.stripes"] != "" {
		args = append(args, "--stripes", d.config["volume.lvm.stripes"])

		if d.config["volume.lvm.stripes.size"] != "" {
			stripSizeBytes, err := d.roundedSizeBytesString(d.config["volume.lvm.stripes.size"])
			if err != nil {
				return fmt.Errorf("Invalid volume stripe size %q: %w", d.config["volume.lvm.stripes.size"], err)
			}

			args = append(args, "--stripesize", fmt.Sprintf("%db", stripSizeBytes))
		}
	}

	// Create the thin pool volume.
	_, err = subprocess.TryRunCommand("lvcreate", args...)
	if err != nil {
		return fmt.Errorf("Error creating LVM thin pool named %q: %w", thinPoolName, err)
	}

	if !isRecent && thinpoolSizeBytes <= 0 {
		// Grow it to the maximum VG size (two step process required by old LVM).
		_, err = subprocess.TryRunCommand("lvextend", "--alloc", "anywhere", "-l", "100%FREE", lvmThinPool)
		if err != nil {
			return fmt.Errorf("Error growing LVM thin pool named %q: %w", thinPoolName, err)
		}
	}

	return nil
}

// lvmVersionIsAtLeast checks whether the installed version of LVM is at least the specific version.
func (d *lvm) lvmVersionIsAtLeast(sTypeVersion string, versionString string) (bool, error) {
	lvmVersionString := strings.Split(sTypeVersion, "/")[0]

	lvmVersion, err := version.Parse(lvmVersionString)
	if err != nil {
		return false, err
	}

	inVersion, err := version.Parse(versionString)
	if err != nil {
		return false, err
	}

	if lvmVersion.Compare(inVersion) < 0 {
		return false, nil
	}

	return true, nil
}

// roundedSizeString rounds the size to the nearest multiple of 512 bytes as the LVM tools require this.
func (d *lvm) roundedSizeBytesString(size string) (int64, error) {
	sizeBytes, err := units.ParseByteSizeString(size)
	if err != nil {
		return 0, err
	}

	if sizeBytes <= 0 {
		return 0, nil
	}

	// LVM tools require sizes in multiples of 512 bytes.
	const minSizeBytes = 512
	if sizeBytes < minSizeBytes {
		sizeBytes = minSizeBytes
	}

	// Round the size to closest minSizeBytes bytes.
	sizeBytes = int64(sizeBytes/minSizeBytes) * minSizeBytes

	return sizeBytes, nil
}

// createLogicalVolume creates a logical volume.
func (d *lvm) createLogicalVolume(vgName, thinPoolName string, vol Volume, makeThinLv bool) error {
	var err error

	lvSizeBytes, err := d.roundedSizeBytesString(vol.ConfigSize())
	if err != nil {
		return err
	}

	lvFullName := d.lvmFullVolumeName(vol.volType, vol.contentType, vol.name)

	args := []string{
		"--name", lvFullName,
		"--yes",
		"--wipesignatures", "y",
	}

	if makeThinLv {
		targetVg := fmt.Sprintf("%s/%s", vgName, thinPoolName)
		args = append(args,
			"--thin",
			"--virtualsize", fmt.Sprintf("%db", lvSizeBytes),
			targetVg,
		)
	} else {
		args = append(args,
			"--size", fmt.Sprintf("%db", lvSizeBytes),
			vgName,
		)

		// As we are creating a normal logical volume we can apply stripes settings if specified.
		stripes := vol.ExpandedConfig("lvm.stripes")
		if stripes != "" {
			args = append(args, "--stripes", stripes)

			stripeSize := vol.ExpandedConfig("lvm.stripes.size")
			if stripeSize != "" {
				stripSizeBytes, err := d.roundedSizeBytesString(stripeSize)
				if err != nil {
					return fmt.Errorf("Invalid volume stripe size %q: %w", stripeSize, err)
				}

				args = append(args, "--stripesize", fmt.Sprintf("%db", stripSizeBytes))
			}
		}
	}

	_, err = subprocess.TryRunCommand("lvcreate", args...)
	if err != nil {
		return fmt.Errorf("Error creating LVM logical volume %q: %w", lvFullName, err)
	}

	volPath := d.lvmPath(vgName, vol.volType, vol.contentType, vol.name)
	volDevPath, err := d.lvmDevPath(volPath)
	if err != nil {
		return err
	}

	if vol.contentType == ContentTypeFS {
		_, err = makeFSType(volDevPath, vol.ConfigBlockFilesystem(), nil)
		if err != nil {
			return fmt.Errorf("Error making filesystem on LVM logical volume: %w", err)
		}
	} else if !d.usesThinpool() {
		// Make sure we get an empty LV.
		err := linux.ClearBlock(volDevPath, 0)
		if err != nil {
			return err
		}
	}

	isRecent, err := d.lvmVersionIsAtLeast(lvmVersion, "2.02.99")
	if err != nil {
		return fmt.Errorf("Error checking LVM version: %w", err)
	}

	if isRecent {
		// Disable auto activation of volume on LVM versions that support it.
		// Must be done after volume create so that zeroing and signature wiping can take place.
		_, err := subprocess.TryRunCommand("lvchange", "--setactivationskip", "y", volPath)
		if err != nil {
			return fmt.Errorf("Failed to set activation skip on LVM logical volume %q: %w", volPath, err)
		}
	}

	d.logger.Debug("Logical volume created", logger.Ctx{"vg_name": vgName, "lv_name": lvFullName, "size": fmt.Sprintf("%db", lvSizeBytes), "fs": vol.ConfigBlockFilesystem()})
	return nil
}

// createLogicalVolumeSnapshot creates a snapshot of a logical volume.
func (d *lvm) createLogicalVolumeSnapshot(vgName string, srcVol Volume, snapVol Volume, readonly bool, makeThinLv bool) (string, error) {
	srcVolPath := d.lvmPath(vgName, srcVol.volType, srcVol.contentType, srcVol.name)
	isRecent, err := d.lvmVersionIsAtLeast(lvmVersion, "2.02.99")
	if err != nil {
		return "", fmt.Errorf("Error checking LVM version: %w", err)
	}

	snapLvName := d.lvmFullVolumeName(snapVol.volType, snapVol.contentType, snapVol.name)
	logCtx := logger.Ctx{"vg_name": vgName, "lv_name": snapLvName, "src_dev": srcVolPath, "thin": makeThinLv}
	args := []string{"-n", snapLvName, "-s", srcVolPath}

	if isRecent {
		args = append(args, "--setactivationskip", "y")
	}

	// If the source is not a thin volume the size needs to be specified.
	// Create snapshot at 100% the size of the origin to allow restoring it to the origin volume without
	// filling up the CoW snapshot volume and causing it to become invalid.
	if !makeThinLv {
		args = append(args, "-l", "100%ORIGIN")
	}

	if readonly {
		args = append(args, "-pr")
	} else {
		args = append(args, "-prw")
	}

	reverter := revert.New()
	defer reverter.Fail()

	// If clustered, we need to acquire exclusive write access for this operation.
	if d.clustered && !makeThinLv {
		parent, _, _ := api.GetParentAndSnapshotName(srcVol.Name())
		parentVol := NewVolume(d, d.Name(), srcVol.volType, srcVol.contentType, parent, srcVol.config, srcVol.poolConfig)

		release, err := d.acquireExclusive(parentVol)
		if err != nil {
			return "", err
		}

		defer release()
	}

	_, err = subprocess.TryRunCommand("lvcreate", args...)
	if err != nil {
		return "", err
	}

	d.logger.Debug("Logical volume snapshot created", logCtx)

	reverter.Add(func() {
		_ = d.removeLogicalVolume(d.lvmPath(vgName, snapVol.volType, snapVol.contentType, snapVol.name))
	})

	targetVolPath := d.lvmPath(vgName, snapVol.volType, snapVol.contentType, snapVol.name)

	reverter.Success()
	return targetVolPath, nil
}

// acquireExclusive switches a volume lock to exclusive mode.
func (d *lvm) acquireExclusive(vol Volume) (func(), error) {
	if !d.clustered {
		return func() {}, nil
	}

	volDevPath, err := d.lvmDevPath(d.lvmPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return func() {}, nil
		}

		return nil, err
	}

	lvmActivation.Lock()
	defer lvmActivation.Unlock()

	_, err = subprocess.TryRunCommand("lvchange", "--activate", "ey", "--ignoreactivationskip", volDevPath)
	if err != nil {
		return nil, fmt.Errorf("Failed to acquire exclusive lock on LVM logical volume %q: %w", volDevPath, err)
	}

	return func() {
		_, _ = subprocess.TryRunCommand("lvchange", "--activate", "sy", "--ignoreactivationskip", volDevPath)
	}, nil
}

// removeLogicalVolume removes a logical volume.
func (d *lvm) removeLogicalVolume(volDevPath string) error {
	_, err := subprocess.TryRunCommand("lvremove", "-f", volDevPath)
	if err != nil {
		return err
	}

	d.logger.Debug("Logical volume removed", logger.Ctx{"dev": volDevPath})

	return nil
}

// renameLogicalVolume renames a logical volume.
func (d *lvm) renameLogicalVolume(volDevPath string, newVolDevPath string) error {
	_, err := subprocess.TryRunCommand("lvrename", volDevPath, newVolDevPath)
	if err != nil {
		return err
	}

	d.logger.Debug("Logical volume renamed", logger.Ctx{"dev": volDevPath, "new_dev": newVolDevPath})

	return nil
}

// lvmFullVolumeName returns the logical volume's full name with volume type prefix. It also converts the supplied
// volName to a name suitable for use as a logical volume using volNameToLVName(). If an empty volType is passed
// then just the volName is returned. If an invalid volType is passed then an empty string is returned.
// If a content type of ContentTypeBlock is supplied then the volume name is suffixed with lvmBlockVolSuffix.
func (d *lvm) lvmFullVolumeName(volType VolumeType, contentType ContentType, volName string) string {
	if volType == "" {
		return volName
	}

	contentTypeSuffix := ""
	if contentType == ContentTypeBlock {
		contentTypeSuffix = lvmBlockVolSuffix
	} else if contentType == ContentTypeISO {
		contentTypeSuffix = lvmISOVolSuffix
	}

	// Escape the volume name to a name suitable for using as a logical volume.
	lvName := strings.ReplaceAll(strings.ReplaceAll(volName, "-", lvmEscapedHyphen), internalInstance.SnapshotDelimiter, lvmSnapshotSeparator)

	return fmt.Sprintf("%s_%s%s", volType, lvName, contentTypeSuffix)
}

// lvmPath returns the VG/LV path suitable to pass to LVM commands.
func (d *lvm) lvmPath(vgName string, volType VolumeType, contentType ContentType, volName string) string {
	fullVolName := d.lvmFullVolumeName(volType, contentType, volName)
	if fullVolName == "" {
		return "" // Invalid volType supplied.
	}

	return fmt.Sprintf("%s/%s", vgName, fullVolName)
}

// lvmDevPath returns the /dev path for the LV.
func (d *lvm) lvmDevPath(pathName string) (string, error) {
	// Get the block dev.
	output, err := subprocess.TryRunCommand("lvdisplay", "-c", pathName)
	if err != nil {
		return "", err
	}

	// Grab the major and minor.
	fields := strings.Split(output, ":")

	if len(fields) < 2 {
		return "", errors.New("Bad lvdisplay output")
	}

	major := strings.TrimSpace(fields[len(fields)-2])
	minor := strings.TrimSpace(fields[len(fields)-1])

	if major == "-1" || minor == "-1" {
		return "", os.ErrNotExist
	}

	target, err := os.Readlink(filepath.Join("/sys/dev/block", major+":"+minor))
	if err != nil {
		return "", err
	}

	return filepath.Join("/dev", filepath.Base(target)), nil
}

// resizeLogicalVolume resizes an LVM logical volume. This function does not resize any filesystem inside the LV.
func (d *lvm) resizeLogicalVolume(lvPath string, sizeBytes int64) error {
	isRecent, err := d.lvmVersionIsAtLeast(lvmVersion, "2.03.17")
	if err != nil {
		return fmt.Errorf("Error checking LVM version: %w", err)
	}

	args := []string{"-L", fmt.Sprintf("%db", sizeBytes), "-f", lvPath}
	if isRecent {
		args = append(args, "--fs=ignore")
	}

	_, err = subprocess.TryRunCommand("lvresize", args...)
	if err != nil {
		return err
	}

	d.logger.Debug("Logical volume resized", logger.Ctx{"dev": lvPath, "size": fmt.Sprintf("%db", sizeBytes)})
	return nil
}

// copyThinpoolVolume makes an optimised copy of a thinpool volume by using thinpool snapshots.
func (d *lvm) copyThinpoolVolume(vol, srcVol Volume, srcSnapshots []Volume, refresh bool) error {
	reverter := revert.New()
	defer reverter.Fail()

	removeVols := []string{}

	// If copying snapshots is indicated, check the source isn't itself a snapshot.
	if len(srcSnapshots) > 0 && !srcVol.IsSnapshot() {
		// Create the parent snapshot directory.
		err := createParentSnapshotDirIfMissing(d.name, vol.volType, vol.name)
		if err != nil {
			return err
		}

		for _, srcSnapshot := range srcSnapshots {
			_, snapName, _ := api.GetParentAndSnapshotName(srcSnapshot.name)
			newFullSnapName := GetSnapshotVolumeName(vol.name, snapName)
			newSnapVol := NewVolume(d, d.Name(), vol.volType, vol.contentType, newFullSnapName, vol.config, vol.poolConfig)

			volExists, err := d.HasVolume(newSnapVol)
			if err != nil {
				return err
			}

			if volExists {
				return fmt.Errorf("LVM snapshot volume already exists %q", newSnapVol.name)
			}

			newSnapVolPath := newSnapVol.MountPath()
			err = newSnapVol.EnsureMountPath()
			if err != nil {
				return err
			}

			reverter.Add(func() { _ = os.RemoveAll(newSnapVolPath) })

			// We do not modify the original snapshot so as to avoid damaging if it is corrupted for
			// some reason. If the filesystem needs to have a unique UUID generated in order to mount
			// this will be done at restore time to be safe.
			_, err = d.createLogicalVolumeSnapshot(d.config["lvm.vg_name"], srcSnapshot, newSnapVol, true, d.usesThinpool())
			if err != nil {
				return fmt.Errorf("Error creating LVM logical volume snapshot: %w", err)
			}

			reverter.Add(func() {
				_ = d.removeLogicalVolume(d.lvmPath(d.config["lvm.vg_name"], newSnapVol.volType, newSnapVol.contentType, newSnapVol.name))
			})
		}
	}

	// Handle copying the main volume.
	volExists, err := d.HasVolume(vol)
	if err != nil {
		return err
	}

	if volExists {
		if refresh {
			newVolPath := d.lvmPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
			tmpVolName := fmt.Sprintf("%s%s", vol.name, tmpVolSuffix)
			tmpVolPath := d.lvmPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, tmpVolName)

			// Rename existing volume to temporary new name so we can revert if needed.
			err := d.renameLogicalVolume(newVolPath, tmpVolPath)
			if err != nil {
				return fmt.Errorf("Error temporarily renaming original LVM logical volume: %w", err)
			}

			// Record this volume to be removed at the very end.
			removeVols = append(removeVols, tmpVolName)

			reverter.Add(func() {
				// Rename the original volume back to the original name.
				_ = d.renameLogicalVolume(tmpVolPath, newVolPath)
			})
		} else {
			return fmt.Errorf("LVM volume already exists %q", vol.name)
		}
	} else {
		volPath := vol.MountPath()
		err := vol.EnsureMountPath()
		if err != nil {
			return err
		}

		reverter.Add(func() { _ = os.RemoveAll(volPath) })
	}

	// Create snapshot of source volume as new volume.
	_, err = d.createLogicalVolumeSnapshot(d.config["lvm.vg_name"], srcVol, vol, false, d.usesThinpool())
	if err != nil {
		return fmt.Errorf("Error creating LVM logical volume snapshot: %w", err)
	}

	volPath := d.lvmPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)

	reverter.Add(func() {
		_ = d.removeLogicalVolume(volPath)
	})

	if vol.contentType == ContentTypeFS {
		// Generate a new filesystem UUID if needed (this is required because some filesystems won't allow
		// volumes with the same UUID to be mounted at the same time). This should be done before volume
		// resize as some filesystems will need to mount the filesystem to resize.
		if renegerateFilesystemUUIDNeeded(vol.ConfigBlockFilesystem()) {
			_, err = d.activateVolume(vol)
			if err != nil {
				return err
			}

			volDevPath, err := d.lvmDevPath(volPath)
			if err != nil {
				return err
			}

			d.logger.Debug("Regenerating filesystem UUID", logger.Ctx{"dev": volDevPath, "fs": vol.ConfigBlockFilesystem()})
			err = regenerateFilesystemUUID(vol.ConfigBlockFilesystem(), volDevPath)
			if err != nil {
				return err
			}
		}

		// Mount the volume and ensure the permissions are set correctly inside the mounted volume.
		err = vol.MountTask(func(_ string, _ *operations.Operation) error {
			return vol.EnsureMountPath()
		}, nil)
		if err != nil {
			return err
		}
	}

	// Resize volume to the size specified. Only uses volume "size" property and does not use pool/defaults
	// to give the caller more control over the size being used.
	err = d.SetVolumeQuota(vol, vol.config["size"], false, nil)
	if err != nil {
		return err
	}

	// Finally clean up original volumes left that were renamed with a tmpVolSuffix suffix.
	for _, removeVolName := range removeVols {
		err := d.removeLogicalVolume(d.lvmPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, removeVolName))
		if err != nil {
			return fmt.Errorf("Error removing LVM volume %q: %w", vol.name, err)
		}
	}

	reverter.Success()
	return nil
}

// logicalVolumeSize gets the size in bytes of a logical volume.
func (d *lvm) logicalVolumeSize(volDevPath string) (int64, error) {
	output, err := subprocess.RunCommand("lvs", "--noheadings", "--nosuffix", "--units", "b", "-o", "lv_size", volDevPath)
	if err != nil {
		if d.isLVMNotFoundExitError(err) {
			return -1, api.StatusErrorf(http.StatusNotFound, "LVM volume not found")
		}

		return -1, fmt.Errorf("Error getting size of LVM volume %q: %w", volDevPath, err)
	}

	output = strings.TrimSpace(output)
	return strconv.ParseInt(output, 10, 64)
}

func (d *lvm) thinPoolVolumeUsage(volDevPath string) (uint64, uint64, error) {
	args := []string{
		volDevPath,
		"--noheadings",
		"--units", "b",
		"--nosuffix",
		"--separator", ",",
		"-o", "lv_size,data_percent",
	}

	out, err := subprocess.RunCommand("lvs", args...)
	if err != nil {
		return 0, 0, err
	}

	parts := util.SplitNTrimSpace(out, ",", -1, true)
	if len(parts) < 2 {
		return 0, 0, errors.New("Unexpected output from lvs command")
	}

	total, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("Failed parsing thin volume total size (%q): %w", parts[0], err)
	}

	totalSize := total

	// Used percentage is not available if thin volume isn't activated.
	if parts[1] == "" {
		return 0, 0, ErrNotSupported
	}

	dataPerc, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return 0, 0, fmt.Errorf("Failed parsing thin volume used percentage (%q): %w", parts[1], err)
	}

	usedSize := uint64(float64(total) * (dataPerc / 100))

	return totalSize, usedSize, nil
}

// parseLogicalVolumeSnapshot parses a raw logical volume name (from lvs command) and checks whether it is a
// snapshot of the supplied parent volume. Returns unescaped parsed snapshot name if snapshot volume recognised,
// empty string if not. The parent is required due to limitations in the naming scheme that Incus has historically
// been used for naming logical volumes meaning that additional context of the parent is required to accurately
// recognise snapshot volumes that belong to the parent.
func (d *lvm) parseLogicalVolumeSnapshot(parent Volume, lvmVolName string) string {
	fullVolName := d.lvmFullVolumeName(parent.volType, parent.contentType, parent.name)

	// If block volume, remove the block suffix ready for comparison with LV list.
	if parent.IsVMBlock() || (parent.volType == VolumeTypeCustom && parent.contentType == ContentTypeBlock) {
		if !strings.HasSuffix(lvmVolName, lvmBlockVolSuffix) {
			return ""
		}

		// Remove the block suffix so that snapshot names can be compared and extracted without the suffix.
		fullVolName = strings.TrimSuffix(fullVolName, lvmBlockVolSuffix)
		lvmVolName = strings.TrimSuffix(lvmVolName, lvmBlockVolSuffix)
	}

	// Prefix we would expect for a snapshot of the parent volume.
	snapPrefix := fmt.Sprintf("%s%s", fullVolName, lvmSnapshotSeparator)

	// Prefix used when escaping "-" in volume names. Doesn't indicate a snapshot of parent.
	badPrefix := fmt.Sprintf("%s%s", fullVolName, lvmEscapedHyphen)

	// Check the volume matches the snapshot prefix, but doesn't match the prefix that indicates a similarly
	// named volume that just has escaped "-" characters in it.
	if strings.HasPrefix(lvmVolName, snapPrefix) && !strings.HasPrefix(lvmVolName, badPrefix) {
		// Remove volume name prefix (including snapshot delimiter) and unescape snapshot name.
		return strings.ReplaceAll(strings.TrimPrefix(lvmVolName, snapPrefix), lvmEscapedHyphen, "-")
	}

	return ""
}

// activateVolume activates an LVM logical volume if not already present. Returns true if activated, false if not.
func (d *lvm) activateVolume(vol Volume) (bool, error) {
	var volPath string

	if d.usesThinpool() {
		volPath = d.lvmPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
	} else {
		// Use parent for non-thinpool vols as activating the parent volume also activates its snapshots.
		parent, _, _ := api.GetParentAndSnapshotName(vol.Name())
		volPath = d.lvmPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, parent)
	}

	_, err := d.lvmDevPath(volPath)
	if err == nil {
		// Already active.
		return false, nil
	}

	if !errors.Is(err, os.ErrNotExist) {
		// Actual failure.
		return false, err
	}

	// Activate the volume.
	lvmActivation.Lock()
	defer lvmActivation.Unlock()

	if d.clustered {
		_, err := subprocess.RunCommand("lvchange", "--activate", "sy", "--ignoreactivationskip", volPath)
		if err != nil {
			return false, fmt.Errorf("Failed to activate LVM logical volume %q: %w", volPath, err)
		}
	} else {
		_, err := subprocess.RunCommand("lvchange", "--activate", "y", "--ignoreactivationskip", volPath)
		if err != nil {
			return false, fmt.Errorf("Failed to activate LVM logical volume %q: %w", volPath, err)
		}
	}

	d.logger.Debug("Activated logical volume", logger.Ctx{"volName": vol.Name(), "dev": volPath})

	return true, nil
}

// deactivateVolume deactivates an LVM logical volume if present. Returns true if deactivated, false if not.
func (d *lvm) deactivateVolume(vol Volume) (bool, error) {
	var volPath string

	if d.usesThinpool() {
		volPath = d.lvmPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, vol.name)
	} else {
		// Use parent for non-thinpool vols as deactivating the parent volume also activates its snapshots.
		parent, _, _ := api.GetParentAndSnapshotName(vol.Name())
		if vol.IsSnapshot() {
			parentVol := NewVolume(d, d.name, vol.volType, vol.contentType, parent, nil, d.config)

			// If parent is in use then skip deactivating non-thinpool snapshot volume as it will fail.
			if parentVol.MountInUse() || (parentVol.contentType == ContentTypeFS && linux.IsMountPoint(parentVol.MountPath())) {
				return false, nil
			}
		}

		volPath = d.lvmPath(d.config["lvm.vg_name"], vol.volType, vol.contentType, parent)
	}

	_, err := d.lvmDevPath(volPath)
	if errors.Is(err, os.ErrNotExist) {
		// Already deactivated.
		return false, nil
	}

	if err != nil {
		// Actual failure.
		return false, err
	}

	lvmActivation.Lock()
	defer lvmActivation.Unlock()

	// Keep trying to deactivate a few times in case the device is still being flushed.
	_, err = subprocess.TryRunCommand("lvchange", "--activate", "n", "--ignoreactivationskip", volPath)
	if err != nil {
		return false, fmt.Errorf("Failed to deactivate LVM logical volume %q: %w", volPath, err)
	}

	d.logger.Debug("Deactivated logical volume", logger.Ctx{"volName": vol.Name(), "dev": volPath})
	return true, nil
}

// getSourceType determines the source type based on the config["source"] value.
func (d *lvm) getSourceType() lvmSourceType {
	defaultSource := loopFilePath(d.name)

	if d.config["source"] == "" || d.config["source"] == defaultSource {
		return lvmSourceTypeDefault
	} else if filepath.IsAbs(d.config["source"]) {
		return lvmSourceTypePhysicalDevice
	} else if d.config["source"] != "" {
		return lvmSourceTypeVolumeGroup
	}

	return lvmSourceTypeUnknown
}
