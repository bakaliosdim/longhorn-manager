package csi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"k8s.io/mount-utils"

	utilexec "k8s.io/utils/exec"

	"github.com/longhorn/longhorn-manager/types"

	longhornclient "github.com/longhorn/longhorn-manager/client"
	longhorn "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
)

const (
	// defaultStaleReplicaTimeout set to 48 hours (2880 minutes)
	defaultStaleReplicaTimeout                         = 2880
	defaultStorageClassDisableRevisionCounterParameter = true

	defaultForceUmountTimeout = 30 * time.Second

	tempTestMountPointValidStatusFile = ".longhorn-volume-mount-point-test.tmp"
)

// NewForcedParamsExec creates a osExecutor that allows for adding additional params to later occurring Run calls
func NewForcedParamsExec(cmdParamMapping map[string]string) utilexec.Interface {
	return &forcedParamsOsExec{
		exec:            utilexec.New(),
		cmdParamMapping: cmdParamMapping,
	}
}

type forcedParamsOsExec struct {
	exec            utilexec.Interface
	cmdParamMapping map[string]string
}

type volumeFilesystemStatistics struct {
	availableBytes int64
	totalBytes     int64
	usedBytes      int64

	availableInodes int64
	totalInodes     int64
	usedInodes      int64
}

func (e *forcedParamsOsExec) Command(cmd string, args ...string) utilexec.Cmd {
	var params []string
	if value := e.cmdParamMapping[cmd]; value != "" {
		// we prepend the user params, since options are conventionally before the final args
		// command [-option(s)] [argument(s)]
		params = append(params, strings.Split(value, " ")...)
	}
	params = append(params, args...)
	return e.exec.Command(cmd, params...)
}

func (e *forcedParamsOsExec) CommandContext(ctx context.Context, cmd string, args ...string) utilexec.Cmd {
	return e.exec.CommandContext(ctx, cmd, args...)
}

func (e *forcedParamsOsExec) LookPath(file string) (string, error) {
	return e.exec.LookPath(file)
}

func updateVolumeParamsForBackingImage(volumeParameters map[string]string, backingImageParameters map[string]string) {
	BackingImageInfoFields := []string{
		longhorn.BackingImageParameterName,
		longhorn.BackingImageParameterDataSourceType,
		longhorn.BackingImageParameterChecksum,
	}
	for _, v := range BackingImageInfoFields {
		volumeParameters[v] = backingImageParameters[v]
		delete(backingImageParameters, v)
	}
	backingImageParametersStr, _ := json.Marshal(backingImageParameters)
	volumeParameters[longhorn.BackingImageParameterDataSourceParameters] = string(backingImageParametersStr)
}

func getVolumeOptions(volumeID string, volOptions map[string]string) (*longhornclient.Volume, error) {
	vol := &longhornclient.Volume{}

	if staleReplicaTimeout, ok := volOptions["staleReplicaTimeout"]; ok {
		srt, err := strconv.Atoi(staleReplicaTimeout)
		if err != nil {
			return nil, errors.Wrap(err, "invalid parameter staleReplicaTimeout")
		}
		vol.StaleReplicaTimeout = int64(srt)
	}
	if vol.StaleReplicaTimeout <= 0 {
		vol.StaleReplicaTimeout = defaultStaleReplicaTimeout
	}

	if share, ok := volOptions["share"]; ok {
		isShared, err := strconv.ParseBool(share)
		if err != nil {
			return nil, errors.Wrap(err, "invalid parameter share")
		}

		if isShared {
			vol.AccessMode = string(longhorn.AccessModeReadWriteMany)
		} else {
			vol.AccessMode = string(longhorn.AccessModeReadWriteOnce)
		}
	}

	if migratable, ok := volOptions["migratable"]; ok {
		isMigratable, err := strconv.ParseBool(migratable)
		if err != nil {
			return nil, errors.Wrap(err, "invalid parameter migratable")
		}

		if isMigratable && vol.AccessMode != string(longhorn.AccessModeReadWriteMany) {
			logrus.Infof("Cannot mark volume %v as migratable, "+
				"since access mode is not RWX proceeding with RWO non migratable volume creation", volumeID)
			volOptions["migratable"] = strconv.FormatBool(false)
			isMigratable = false
		}
		vol.Migratable = isMigratable
	}

	if encrypted, ok := volOptions["encrypted"]; ok {
		isEncrypted, err := strconv.ParseBool(encrypted)
		if err != nil {
			return nil, errors.Wrap(err, "invalid parameter encrypted")
		}
		vol.Encrypted = isEncrypted
	}

	if numberOfReplicas, ok := volOptions["numberOfReplicas"]; ok {
		nor, err := strconv.Atoi(numberOfReplicas)
		if err != nil || nor < 0 {
			return nil, errors.Wrap(err, "invalid parameter numberOfReplicas")
		}
		vol.NumberOfReplicas = int64(nor)
	}

	if replicaAutoBalance, ok := volOptions["replicaAutoBalance"]; ok {
		err := types.ValidateReplicaAutoBalance(longhorn.ReplicaAutoBalance(replicaAutoBalance))
		if err != nil {
			return nil, errors.Wrap(err, "invalid parameter replicaAutoBalance")
		}
		vol.ReplicaAutoBalance = replicaAutoBalance
	}

	if locality, ok := volOptions["dataLocality"]; ok {
		if err := types.ValidateDataLocality(longhorn.DataLocality(locality)); err != nil {
			return nil, errors.Wrap(err, "invalid parameter dataLocality")
		}
		vol.DataLocality = locality
	}

	if revisionCounterDisabled, ok := volOptions["disableRevisionCounter"]; ok {
		revCounterDisabled, err := strconv.ParseBool(revisionCounterDisabled)
		if err != nil {
			return nil, errors.Wrap(err, "invalid parameter disableRevisionCounter")
		}
		vol.RevisionCounterDisabled = revCounterDisabled
	} else {
		vol.RevisionCounterDisabled = defaultStorageClassDisableRevisionCounterParameter
	}

	if unmapMarkSnapChainRemoved, ok := volOptions["unmapMarkSnapChainRemoved"]; ok {
		if err := types.ValidateUnmapMarkSnapChainRemoved(longhorn.DataEngineType(vol.DataEngine), longhorn.UnmapMarkSnapChainRemoved(unmapMarkSnapChainRemoved)); err != nil {
			return nil, errors.Wrap(err, "invalid parameter unmapMarkSnapChainRemoved")
		}
		vol.UnmapMarkSnapChainRemoved = unmapMarkSnapChainRemoved
	}

	if replicaSoftAntiAffinity, ok := volOptions["replicaSoftAntiAffinity"]; ok {
		if err := types.ValidateReplicaSoftAntiAffinity(longhorn.ReplicaSoftAntiAffinity(replicaSoftAntiAffinity)); err != nil {
			return nil, errors.Wrap(err, "invalid parameter replicaSoftAntiAffinity")
		}
		vol.ReplicaSoftAntiAffinity = replicaSoftAntiAffinity
	}

	if replicaZoneSoftAntiAffinity, ok := volOptions["replicaZoneSoftAntiAffinity"]; ok {
		if err := types.ValidateReplicaZoneSoftAntiAffinity(longhorn.ReplicaZoneSoftAntiAffinity(replicaZoneSoftAntiAffinity)); err != nil {
			return nil, errors.Wrap(err, "invalid parameter replicaZoneSoftAntiAffinity")
		}
		vol.ReplicaZoneSoftAntiAffinity = replicaZoneSoftAntiAffinity
	}

	if replicaDiskSoftAntiAffinity, ok := volOptions["replicaDiskSoftAntiAffinity"]; ok {
		if err := types.ValidateReplicaDiskSoftAntiAffinity(longhorn.ReplicaDiskSoftAntiAffinity(replicaDiskSoftAntiAffinity)); err != nil {
			return nil, errors.Wrap(err, "invalid parameter replicaDiskSoftAntiAffinity")
		}
		vol.ReplicaDiskSoftAntiAffinity = replicaDiskSoftAntiAffinity
	}

	if fromBackup, ok := volOptions["fromBackup"]; ok {
		vol.FromBackup = fromBackup
	}

	if backupTargetName, ok := volOptions["backupTargetName"]; ok {
		vol.BackupTargetName = backupTargetName
	}

	if dataSource, ok := volOptions["dataSource"]; ok {
		vol.DataSource = dataSource
	}

	if backingImage, ok := volOptions[longhorn.BackingImageParameterName]; ok {
		vol.BackingImage = backingImage
	}

	recurringJobSelector := []longhornclient.VolumeRecurringJob{}
	if jsonRecurringJobSelector, ok := volOptions["recurringJobSelector"]; ok {
		err := json.Unmarshal([]byte(jsonRecurringJobSelector), &recurringJobSelector)
		if err != nil {
			return nil, errors.Wrap(err, "invalid json format of recurringJobSelector")
		}
		vol.RecurringJobSelector = recurringJobSelector
	}

	if diskSelector, ok := volOptions["diskSelector"]; ok {
		vol.DiskSelector = strings.Split(diskSelector, ",")
	}

	if nodeSelector, ok := volOptions["nodeSelector"]; ok {
		vol.NodeSelector = strings.Split(nodeSelector, ",")
	}

	vol.DataEngine = string(longhorn.DataEngineTypeV1)
	if driver, ok := volOptions["dataEngine"]; ok {
		vol.DataEngine = driver
	}

	if frontend, ok := volOptions["frontend"]; ok {
		vol.Frontend = frontend
	}

	if freezeFilesystemForSnapshot, ok := volOptions["freezeFilesystemForSnapshot"]; ok {
		if err := types.ValidateFreezeFilesystemForSnapshot(longhorn.FreezeFilesystemForSnapshot(freezeFilesystemForSnapshot)); err != nil {
			return nil, errors.Wrap(err, "invalid parameter freezeFilesystemForSnapshot")
		}
		vol.FreezeFilesystemForSnapshot = freezeFilesystemForSnapshot
	}

	return vol, nil
}

func syncMountPointDirectory(targetPath string) error {
	d, err := os.OpenFile(targetPath, os.O_SYNC, 0750)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := d.Close(); closeErr != nil {
			logrus.WithError(closeErr).Warnf("Failed to close mount point %v", targetPath)
		}
	}()

	if _, err := d.Readdirnames(1); err != nil {
		if !errors.Is(err, io.EOF) {
			return err
		}
	}

	// it would not always return `Input/Output Error` or `read-only file system` errors if we only use ReadDir() or Readdirnames() without an I/O operation
	// an I/O operation will make the targetPath mount point invalid immediately
	tempFile := path.Join(targetPath, tempTestMountPointValidStatusFile)
	f, err := os.OpenFile(tempFile, os.O_CREATE|os.O_RDWR|os.O_SYNC|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	defer func() {
		if err := f.Close(); err != nil {
			logrus.WithError(err).Warnf("Failed to close %v", tempFile)
		}
		if err := os.Remove(tempFile); err != nil {
			logrus.WithError(err).Warnf("Failed to remove %v", tempFile)
		}
	}()

	if _, err := f.WriteString(tempFile); err != nil {
		return err
	}

	return nil
}

// ensureMountPoint evaluates whether a path is a valid mountPoint
// in case the path does not exists it will create a path and return false
// in case where the mount point exists but is corrupt, the mount point will be cleaned up and a error is returned
func ensureMountPoint(path string, mounter mount.Interface) (bool, error) {
	logrus.Infof("Trying to ensure mount point %v", path)
	isMnt, err := mounter.IsMountPoint(path)
	if os.IsNotExist(err) {
		return false, os.MkdirAll(path, 0750)
	}

	IsCorruptedMnt := mount.IsCorruptedMnt(err)
	if !IsCorruptedMnt {
		logrus.Infof("Mount point %v try opening and syncing dir to make sure it's healthy", path)
		if err := syncMountPointDirectory(path); err != nil {
			logrus.WithError(err).Warnf("Mount point %v was identified as corrupt by opening and syncing", path)
			IsCorruptedMnt = true
		}
	}

	if IsCorruptedMnt {
		unmountErr := unmount(path, mounter)
		if unmountErr != nil {
			return false, fmt.Errorf("failed to unmount corrupt mount point %v umount error: %v eval error: %v",
				path, unmountErr, err)
		}

		logrus.Infof("Unmounted existing corrupt mount point %v", path)
		return false, nil
	}
	return isMnt, err
}

// ensureDirectory checks if a folder exists at the specified path.
// If not, it creates the folder and returns true, otherwise returns false.
// If the path exists but is not a folder, it returns an error.
func ensureDirectory(path string) (bool, error) {
	fileInfo, err := os.Stat(path)

	if os.IsNotExist(err) {
		err := os.MkdirAll(path, os.ModePerm)
		if err != nil {
			return false, err
		}
		return true, nil
	} else if err != nil {
		return false, err
	}

	if fileInfo.IsDir() {
		return true, nil
	}

	return false, fmt.Errorf("path %v exists but is not a folder", path)
}

func unmount(path string, mounter mount.Interface) (err error) {
	forceUnmounter, ok := mounter.(mount.MounterForceUnmounter)
	if ok {
		logrus.Infof("Trying to force unmount potential mount point %v", path)
		err = forceUnmounter.UnmountWithForce(path, defaultForceUmountTimeout)
	} else {
		logrus.Infof("Trying to unmount potential mount point %v", path)
		err = mounter.Unmount(path)
	}
	if err == nil {
		return nil
	}

	if strings.Contains(err.Error(), "not mounted") ||
		strings.Contains(err.Error(), "no mount point specified") {
		logrus.Infof("No need for unmount not a mount point %v", path)
		return nil
	}

	return err
}

// unmountAndCleanupMountPoint ensures all mount layers for the path are unmounted and the mount directory is removed
func unmountAndCleanupMountPoint(path string, mounter mount.Interface) error {
	// we just try to unmount since the path check would get stuck for nfs mounts
	logrus.Infof("Trying to umount mount point %v", path)
	if err := unmount(path, mounter); err != nil {
		logrus.WithError(err).Warnf("Failed to unmount %v during cleanup", path)
		return err
	}

	logrus.Infof("Trying to clean up mount point %v", path)
	return mount.CleanupMountPoint(path, mounter, true)
}

// isBlockDevice return true if volumePath file is a block device, false otherwise.
func isBlockDevice(volumePath string) (bool, error) {
	var stat unix.Stat_t
	// See https://man7.org/linux/man-pages/man2/stat.2.html for details
	err := unix.Stat(volumePath, &stat)
	if err != nil {
		return false, err
	}

	// See https://man7.org/linux/man-pages/man7/inode.7.html for detail
	if (stat.Mode & unix.S_IFMT) == unix.S_IFBLK {
		return true, nil
	}

	return false, nil
}

func getDiskFormat(devicePath string) (string, error) {
	m := mount.SafeFormatAndMount{Interface: mount.New(""), Exec: utilexec.New()}
	return m.GetDiskFormat(devicePath)
}

func getFilesystemStatistics(volumePath string) (*volumeFilesystemStatistics, error) {
	var statfs unix.Statfs_t
	// See http://man7.org/linux/man-pages/man2/statfs.2.html for details.
	err := unix.Statfs(volumePath, &statfs)
	if err != nil {
		return nil, err
	}

	volStats := &volumeFilesystemStatistics{
		availableBytes: int64(statfs.Bavail) * int64(statfs.Bsize),
		totalBytes:     int64(statfs.Blocks) * int64(statfs.Bsize),
		usedBytes:      (int64(statfs.Blocks) - int64(statfs.Bfree)) * int64(statfs.Bsize),

		availableInodes: int64(statfs.Ffree),
		totalInodes:     int64(statfs.Files),
		usedInodes:      int64(statfs.Files) - int64(statfs.Ffree),
	}

	return volStats, nil
}

// makeFile creates an empty file.
// If pathname already exists, whether a file or directory, no error is returned.
func makeFile(pathname string) error {
	f, err := os.OpenFile(pathname, os.O_CREATE, os.FileMode(0644))
	if f != nil {
		err = f.Close()
		return err
	}
	if err != nil {
		if !os.IsExist(err) {
			return err
		}
	}
	return nil
}

// requiresSharedAccess checks if the volume is requested to be multi node capable
// a volume that is already in shared access mode, must be used via shared access
// even if single node access is requested.
func requiresSharedAccess(vol *longhornclient.Volume, cap *csi.VolumeCapability) bool {
	isSharedVolume := false
	if vol != nil {
		isSharedVolume = vol.AccessMode == string(longhorn.AccessModeReadWriteMany) || vol.Migratable
	}

	mode := csi.VolumeCapability_AccessMode_UNKNOWN
	if cap != nil {
		mode = cap.AccessMode.Mode
	}

	return isSharedVolume ||
		mode == csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY ||
		mode == csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER ||
		mode == csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
}

func getStageBlockVolumePath(stagingTargetPath, volumeID string) string {
	return filepath.Join(stagingTargetPath, volumeID)
}
