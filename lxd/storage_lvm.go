package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gorilla/websocket"

	"github.com/lxc/lxd/shared"

	log "gopkg.in/inconshreveable/log15.v2"
)

var storageLvmDefaultThinLVSize = "10GiB"
var storageLvmDefaultThinPoolName = "LXDPool"

func storageLVMCheckVolumeGroup(vgName string) error {
	output, err := exec.Command("vgdisplay", "-s", vgName).CombinedOutput()
	if err != nil {
		shared.Log.Debug("vgdisplay failed to find vg", log.Ctx{"output": string(output)})
		return fmt.Errorf("LVM volume group '%s' not found", vgName)
	}

	return nil
}

func storageLVMThinpoolExists(vgName string, poolName string) (bool, error) {
	output, err := exec.Command("vgs", "--noheadings", "-o", "lv_attr", fmt.Sprintf("%s/%s", vgName, poolName)).Output()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			waitStatus := exitError.Sys().(syscall.WaitStatus)
			if waitStatus.ExitStatus() == 5 {
				// pool LV was not found
				return false, nil
			}
		}
		return false, fmt.Errorf("Error checking for pool '%s'", poolName)
	}
	// Found LV named poolname, check type:
	attrs := strings.TrimSpace(string(output[:]))
	if strings.HasPrefix(attrs, "t") {
		return true, nil
	}

	return false, fmt.Errorf("Pool named '%s' exists but is not a thin pool.", poolName)
}

func storageLVMGetThinPoolUsers(d *Daemon) ([]string, error) {
	results := []string{}
	vgname, err := d.ConfigValueGet("storage.lvm_vg_name")
	if err != nil {
		return results, fmt.Errorf("Error getting lvm_vg_name config")
	}
	if vgname == "" {
		return results, nil
	}
	poolname, err := d.ConfigValueGet("storage.lvm_thinpool_name")
	if err != nil {
		return results, fmt.Errorf("Error getting lvm_thinpool_name config")
	}
	if poolname == "" {
		return results, nil
	}

	cNames, err := dbContainersList(d.db, cTypeRegular)
	if err != nil {
		return results, err
	}
	for _, cName := range cNames {
		var lvLinkPath string
		if strings.Contains(cName, shared.SnapshotDelimiter) {
			lvLinkPath = shared.VarPath("snapshots", fmt.Sprintf("%s.lv", cName))
		} else {
			lvLinkPath = shared.VarPath("containers", fmt.Sprintf("%s.lv", cName))
		}

		if shared.PathExists(lvLinkPath) {
			results = append(results, cName)
		}
	}

	imageNames, err := dbImagesGet(d.db, false)
	if err != nil {
		return results, err
	}

	for _, imageName := range imageNames {
		imageLinkPath := shared.VarPath("images", fmt.Sprintf("%s.lv", imageName))
		if shared.PathExists(imageLinkPath) {
			results = append(results, imageName)
		}
	}

	return results, nil
}

func storageLVMSetThinPoolNameConfig(d *Daemon, poolname string) error {
	users, err := storageLVMGetThinPoolUsers(d)
	if err != nil {
		return fmt.Errorf("Error checking if a pool is already in use: %v", err)
	}
	if len(users) > 0 {
		return fmt.Errorf("Can not change LVM config. Images or containers are still using LVs: %v", users)
	}

	vgname, err := d.ConfigValueGet("storage.lvm_vg_name")
	if err != nil {
		return fmt.Errorf("Error getting lvm_vg_name config: %v", err)
	}

	if poolname != "" {
		if vgname == "" {
			return fmt.Errorf("Can not set lvm_thinpool_name without lvm_vg_name set.")
		}

		poolExists, err := storageLVMThinpoolExists(vgname, poolname)
		if err != nil {
			return fmt.Errorf("Error checking for thin pool '%s' in '%s': %v", poolname, vgname, err)
		}
		if !poolExists {
			return fmt.Errorf("Pool '%s' does not exist in Volume Group '%s'", poolname, vgname)
		}
	}

	err = d.ConfigValueSet("storage.lvm_thinpool_name", poolname)
	if err != nil {
		return err
	}

	return nil
}

func storageLVMSetVolumeGroupNameConfig(d *Daemon, vgname string) error {
	users, err := storageLVMGetThinPoolUsers(d)
	if err != nil {
		return fmt.Errorf("Error checking if a pool is already in use: %v", err)
	}
	if len(users) > 0 {
		return fmt.Errorf("Can not change LVM config. Images or containers are still using LVs: %v", users)
	}

	if vgname != "" {
		err = storageLVMCheckVolumeGroup(vgname)
		if err != nil {
			return err
		}
	}

	err = d.ConfigValueSet("storage.lvm_vg_name", vgname)
	if err != nil {
		return err
	}

	return nil
}

func containerNameToLVName(containerName string) string {
	lvName := strings.Replace(containerName, "-", "--", -1)
	return strings.Replace(lvName, shared.SnapshotDelimiter, "-", -1)
}

type storageLvm struct {
	d      *Daemon
	vgName string

	storageShared
}

func (s *storageLvm) Init(config map[string]interface{}) (storage, error) {
	s.sType = storageTypeLvm
	s.sTypeName = storageTypeToString(s.sType)
	if err := s.initShared(); err != nil {
		return s, err
	}

	output, err := exec.Command("lvm", "version").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("Error getting LVM version: %v\noutput:'%s'", err, string(output))
	}
	lines := strings.Split(string(output), "\n")

	s.sTypeVersion = ""
	for idx, line := range lines {
		fields := strings.SplitAfterN(line, ":", 2)
		if len(fields) < 2 {
			continue
		}
		if idx > 0 {
			s.sTypeVersion += " / "
		}
		s.sTypeVersion += strings.TrimSpace(fields[1])
	}

	if config["vgName"] == nil {
		vgName, err := s.d.ConfigValueGet("storage.lvm_vg_name")
		if err != nil {
			return s, fmt.Errorf("Error checking server config: %v", err)
		}
		if vgName == "" {
			return s, fmt.Errorf("LVM isn't enabled")
		}

		if err := storageLVMCheckVolumeGroup(vgName); err != nil {
			return s, err
		}
		s.vgName = vgName
	} else {
		s.vgName = config["vgName"].(string)
	}

	return s, nil
}

func (s *storageLvm) ContainerCreate(container container) error {
	containerName := containerNameToLVName(container.Name())
	lvpath, err := s.createThinLV(containerName)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(container.Path(), 0755); err != nil {
		return err
	}

	dst := shared.VarPath("containers", fmt.Sprintf("%s.lv", container.Name()))
	err = os.Symlink(lvpath, dst)
	if err != nil {
		return err
	}

	return nil
}

func (s *storageLvm) ContainerCreateFromImage(
	container container, imageFingerprint string) error {

	imageLVFilename := shared.VarPath(
		"images", fmt.Sprintf("%s.lv", imageFingerprint))

	if !shared.PathExists(imageLVFilename) {
		if err := s.ImageCreate(imageLVFilename); err != nil {
			return err
		}
	}

	containerName := containerNameToLVName(container.Name())

	lvpath, err := s.createSnapshotLV(containerName, imageFingerprint, false)
	if err != nil {
		return err
	}

	destPath := container.Path()
	if err := os.MkdirAll(destPath, 0755); err != nil {
		return fmt.Errorf("Error creating container directory: %v", err)
	}

	dst := shared.VarPath("containers", fmt.Sprintf("%s.lv", container.Name()))
	err = os.Symlink(lvpath, dst)
	if err != nil {
		return err
	}

	output, err := exec.Command("mount", "-o", "discard", lvpath, destPath).CombinedOutput()
	if err != nil {
		s.ContainerDelete(container)
		return fmt.Errorf("Error mounting snapshot LV: %v\noutput:'%s'", err, string(output))
	}

	if !container.IsPrivileged() {
		if err = s.shiftRootfs(container); err != nil {
			err2 := syscall.Unmount(destPath, 0)
			if err2 != nil {
				return fmt.Errorf("Error in umount: '%s' while cleaning up after error in shiftRootfs: '%s'", err2, err)
			}
			s.ContainerDelete(container)
			return fmt.Errorf("Error in shiftRootfs: %v", err)
		}
	}

	err = container.TemplateApply("create")
	if err != nil {
		s.log.Error("Error in create template during ContainerCreateFromImage, continuing to unmount",
			log.Ctx{"err": err})
	}

	umounterr := syscall.Unmount(destPath, 0)
	if umounterr != nil {
		return fmt.Errorf("Error unmounting '%s' after shiftRootfs: %v", destPath, umounterr)
	}

	return err
}

func (s *storageLvm) ContainerCanRestore(container container, sourceContainer container) error {
	return nil
}

func (s *storageLvm) ContainerDelete(container container) error {
	lvName := containerNameToLVName(container.Name())
	if err := s.removeLV(lvName); err != nil {
		return err
	}

	lvLinkPath := fmt.Sprintf("%s.lv", container.Path())
	if err := os.Remove(lvLinkPath); err != nil {
		return err
	}

	cPath := container.Path()
	if err := os.RemoveAll(cPath); err != nil {
		s.log.Error("ContainerDelete: failed to remove path", log.Ctx{"cPath": cPath, "err": err})
		return fmt.Errorf("Cleaning up %s: %s", cPath, err)
	}

	return nil
}

func (s *storageLvm) ContainerCopy(container container, sourceContainer container) error {
	if s.isLVMContainer(sourceContainer) {
		if err := s.createSnapshotContainer(container, sourceContainer, false); err != nil {
			s.log.Error("Error creating snapshot LV for copy", log.Ctx{"err": err})
			return err
		}
	} else {
		s.log.Info("Copy from Non-LVM container", log.Ctx{"container": container.Name(),
			"sourceContainer": sourceContainer.Name()})
		if err := s.ContainerCreate(container); err != nil {
			s.log.Error("Error creating empty container", log.Ctx{"err": err})
			return err
		}

		if err := s.ContainerStart(container); err != nil {
			s.log.Error("Error starting/mounting container", log.Ctx{"err": err, "container": container.Name()})
			s.ContainerDelete(container)
			return err
		}

		output, err := storageRsyncCopy(
			sourceContainer.Path(),
			container.Path())
		if err != nil {
			s.log.Error("ContainerCopy: rsync failed", log.Ctx{"output": string(output)})
			s.ContainerDelete(container)
			return fmt.Errorf("rsync failed: %s", string(output))
		}

		if err := s.ContainerStop(container); err != nil {
			return err
		}
	}
	return container.TemplateApply("copy")
}

func (s *storageLvm) ContainerStart(container container) error {
	lvName := containerNameToLVName(container.Name())
	lvpath := fmt.Sprintf("/dev/%s/%s", s.vgName, lvName)
	output, err := exec.Command(
		"mount", "-o", "discard", lvpath, container.Path()).CombinedOutput()
	if err != nil {
		return fmt.Errorf(
			"Error mounting snapshot LV path='%s': %v\noutput:'%s'",
			container.Path(),
			err,
			string(output))
	}

	return nil
}

func (s *storageLvm) ContainerStop(container container) error {
	err := syscall.Unmount(container.Path(), 0)
	if err != nil {
		return fmt.Errorf(
			"failed to unmount container path '%s'.\nError: %v",
			container.Path(),
			err)
	}

	return nil
}

func (s *storageLvm) ContainerRename(
	container container, newContainerName string) error {

	oldName := containerNameToLVName(container.Name())
	newName := containerNameToLVName(newContainerName)
	output, err := s.renameLV(oldName, newName)
	if err != nil {
		s.log.Error("Failed to rename a container LV",
			log.Ctx{"oldName": oldName,
				"newName": newName,
				"err":     err,
				"output":  string(output)})

		return fmt.Errorf("Failed to rename a container LV, oldName='%s', newName='%s', err='%s'", oldName, newName, err)
	}

	// Rename the Symlink
	oldSymPath := fmt.Sprintf("%s.lv", container.Path())
	newSymPath := fmt.Sprintf("%s.lv", containerPath(newName, false))
	if err := os.Rename(oldSymPath, newSymPath); err != nil {
		s.log.Error("Rename of the symlink failed",
			log.Ctx{"oldPath": oldSymPath,
				"newPath": newSymPath,
				"err":     err})

		return err
	}

	if !container.IsSnapshot() {
		snaps, err := container.Snapshots()
		if err != nil {
			return err
		}

		for _, snap := range snaps {
			baseSnapName := filepath.Base(snap.Name())
			newSnapshotName := newName + shared.SnapshotDelimiter + baseSnapName
			err := s.ContainerRename(snap, newSnapshotName)
			if err != nil {
				return err
			}
		}
	}

	return nil

}

func (s *storageLvm) ContainerRestore(
	container container, sourceContainer container) error {
	srcName := containerNameToLVName(sourceContainer.Name())
	destName := containerNameToLVName(container.Name())

	err := s.removeLV(destName)
	if err != nil {
		return fmt.Errorf("Error removing LV about to be restored over: %v", err)
	}

	_, err = s.createSnapshotLV(destName, srcName, false)
	if err != nil {
		return fmt.Errorf("Error creating snapshot LV: %v", err)
	}

	return nil
}

func (s *storageLvm) ContainerSnapshotCreate(
	snapshotContainer container, sourceContainer container) error {
	return s.createSnapshotContainer(snapshotContainer, sourceContainer, true)
}

func (s *storageLvm) createSnapshotContainer(
	snapshotContainer container, sourceContainer container, readonly bool) error {

	srcName := containerNameToLVName(sourceContainer.Name())
	destName := containerNameToLVName(snapshotContainer.Name())
	shared.Log.Debug(
		"Creating snapshot",
		log.Ctx{"srcName": srcName, "destName": destName})

	lvpath, err := s.createSnapshotLV(destName, srcName, readonly)
	if err != nil {
		return fmt.Errorf("Error creating snapshot LV: %v", err)
	}

	destPath := snapshotContainer.Path()
	if err := os.MkdirAll(destPath, 0755); err != nil {
		return fmt.Errorf("Error creating container directory: %v", err)
	}

	dest := fmt.Sprintf("%s.lv", snapshotContainer.Path())
	err = os.Symlink(lvpath, dest)
	if err != nil {
		return err
	}

	return nil
}

func (s *storageLvm) ContainerSnapshotDelete(
	snapshotContainer container) error {

	err := s.ContainerDelete(snapshotContainer)
	if err != nil {
		return fmt.Errorf("Error deleting snapshot %s: %s", snapshotContainer.Name(), err)
	}

	oldPathParent := filepath.Dir(snapshotContainer.Path())
	if ok, _ := shared.PathIsEmpty(oldPathParent); ok {
		os.Remove(oldPathParent)
	}
	return nil
}

func (s *storageLvm) ContainerSnapshotRename(
	snapshotContainer container, newContainerName string) error {
	oldName := containerNameToLVName(snapshotContainer.Name())
	newName := containerNameToLVName(newContainerName)
	output, err := s.renameLV(oldName, newName)
	if err != nil {
		s.log.Error("Failed to rename a snapshot LV",
			log.Ctx{"oldName": oldName, "newName": newName, "err": err, "output": string(output)})
		return fmt.Errorf("Failed to rename a container LV, oldName='%s', newName='%s', err='%s'", oldName, newName, err)
	}

	oldPath := snapshotContainer.Path()
	oldSymPath := fmt.Sprintf("%s.lv", oldPath)
	newPath := containerPath(newName, true)
	newSymPath := fmt.Sprintf("%s.lv", newPath)

	if err := os.Rename(oldSymPath, newSymPath); err != nil {
		s.log.Error("Failed to rename symlink", log.Ctx{"oldSymPath": oldSymPath, "newSymPath": newSymPath, "err": err})
		return fmt.Errorf("Failed to rename symlink err='%s'", err)
	}

	if strings.Contains(snapshotContainer.Name(), "/") {
		if !shared.PathExists(filepath.Dir(newPath)) {
			os.MkdirAll(filepath.Dir(newPath), 0700)
		}
	}

	if strings.Contains(snapshotContainer.Name(), "/") {
		if ok, _ := shared.PathIsEmpty(filepath.Dir(oldPath)); ok {
			os.Remove(filepath.Dir(oldPath))
		}
	}

	return nil
}

func (s *storageLvm) ContainerSnapshotStart(container container) error {
	srcName := containerNameToLVName(container.Name())
	destName := containerNameToLVName(container.Name() + "/rw")

	shared.Log.Debug(
		"Creating snapshot",
		log.Ctx{"srcName": srcName, "destName": destName})

	lvpath, err := s.createSnapshotLV(destName, srcName, false)
	if err != nil {
		return fmt.Errorf("Error creating snapshot LV: %v", err)
	}

	destPath := container.Path()
	if !shared.PathExists(destPath) {
		if err := os.MkdirAll(destPath, 0755); err != nil {
			return fmt.Errorf("Error creating container directory: %v", err)
		}
	}

	output, err := exec.Command(
		"mount", "-o", "discard", lvpath, container.Path()).CombinedOutput()
	if err != nil {
		return fmt.Errorf(
			"Error mounting snapshot LV path='%s': %v\noutput:'%s'",
			container.Path(),
			err,
			string(output))
	}

	return nil
}

func (s *storageLvm) ContainerSnapshotStop(container container) error {
	err := s.ContainerStop(container)
	if err != nil {
		return err
	}

	lvName := containerNameToLVName(container.Name() + "/rw")
	if err := s.removeLV(lvName); err != nil {
		return err
	}

	return nil
}

func (s *storageLvm) ContainerSnapshotCreateEmpty(snapshotContainer container) error {
	return s.ContainerCreate(snapshotContainer)
}

func (s *storageLvm) ImageCreate(fingerprint string) error {
	finalName := shared.VarPath("images", fingerprint)

	lvpath, err := s.createThinLV(fingerprint)
	if err != nil {
		s.log.Error("LVMCreateThinLV", log.Ctx{"err": err})
		return fmt.Errorf("Error Creating LVM LV for new image: %v", err)
	}

	dst := shared.VarPath("images", fmt.Sprintf("%s.lv", fingerprint))
	err = os.Symlink(lvpath, dst)
	if err != nil {
		return err
	}

	tempLVMountPoint, err := ioutil.TempDir(shared.VarPath("images"), "tmp_lv_mnt")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(tempLVMountPoint); err != nil {
			s.log.Error("Deleting temporary LVM mount point", log.Ctx{"err": err})
		}
	}()

	output, err := exec.Command(
		"mount",
		"-o", "discard",
		lvpath,
		tempLVMountPoint).CombinedOutput()

	if err != nil {
		shared.Logf("Error mounting image LV for untarring: '%s'", string(output))
		return fmt.Errorf("Error mounting image LV: %v", err)

	}

	untarErr := untarImage(finalName, tempLVMountPoint)

	err = syscall.Unmount(tempLVMountPoint, 0)
	if err != nil {
		s.log.Warn("could not unmount LV. Will not remove",
			log.Ctx{"lvpath": lvpath, "mountpoint": tempLVMountPoint, "err": err})
		if untarErr == nil {
			return err
		}

		return fmt.Errorf(
			"Error unmounting '%s' during cleanup of error %v",
			tempLVMountPoint, untarErr)
	}

	return untarErr
}

func (s *storageLvm) ImageDelete(fingerprint string) error {
	err := s.removeLV(fingerprint)
	if err != nil {
		return err
	}

	lvsymlink := fmt.Sprintf(
		"%s.lv", shared.VarPath("images", fingerprint))
	err = os.Remove(lvsymlink)
	if err != nil {
		return fmt.Errorf(
			"Failed to remove symlink to deleted image LV: '%s': %v", lvsymlink, err)
	}

	return nil
}

func (s *storageLvm) createDefaultThinPool() (string, error) {
	output, err := exec.Command(
		"lvcreate",
		"--poolmetadatasize", "1G",
		"-l", "100%FREE",
		"--thinpool",
		fmt.Sprintf("%s/%s", s.vgName, storageLvmDefaultThinPoolName)).CombinedOutput()

	if err != nil {
		s.log.Debug(
			"Could not create thin pool",
			log.Ctx{
				"name":   storageLvmDefaultThinPoolName,
				"err":    err,
				"output": string(output)})

		return "", fmt.Errorf(
			"Could not create LVM thin pool named %s", storageLvmDefaultThinPoolName)
	}
	return storageLvmDefaultThinPoolName, nil
}

func (s *storageLvm) createThinLV(lvname string) (string, error) {
	poolname, err := s.d.ConfigValueGet("storage.lvm_thinpool_name")
	if err != nil {
		return "", fmt.Errorf("Error checking server config, err=%v", err)
	}

	if poolname == "" {
		poolname, err = s.createDefaultThinPool()
		if err != nil {
			return "", fmt.Errorf("Error creating LVM thin pool: %v", err)
		}
		err = storageLVMSetThinPoolNameConfig(s.d, poolname)
		if err != nil {
			s.log.Error("Setting thin pool name", log.Ctx{"err": err})
			return "", fmt.Errorf("Error setting LVM thin pool config: %v", err)
		}
	}

	output, err := exec.Command(
		"lvcreate",
		"--thin",
		"-n", lvname,
		"--virtualsize", storageLvmDefaultThinLVSize,
		fmt.Sprintf("%s/%s", s.vgName, poolname)).CombinedOutput()

	if err != nil {
		s.log.Debug("Could not create LV", log.Ctx{"lvname": lvname, "output": string(output)})
		return "", fmt.Errorf("Could not create thin LV named %s", lvname)
	}

	lvpath := fmt.Sprintf("/dev/%s/%s", s.vgName, lvname)
	output, err = exec.Command(
		"mkfs.ext4",
		"-E", "nodiscard,lazy_itable_init=0,lazy_journal_init=0",
		lvpath).CombinedOutput()

	if err != nil {
		s.log.Error("mkfs.ext4", log.Ctx{"output": string(output)})
		return "", fmt.Errorf("Error making filesystem on image LV: %v", err)
	}

	return lvpath, nil
}

func (s *storageLvm) removeLV(lvname string) error {
	output, err := exec.Command(
		"lvremove", "-f", fmt.Sprintf("%s/%s", s.vgName, lvname)).CombinedOutput()
	if err != nil {
		s.log.Debug("Could not remove LV", log.Ctx{"lvname": lvname, "output": string(output)})
		return fmt.Errorf("Could not remove LV named %s", lvname)
	}
	return nil
}

func (s *storageLvm) createSnapshotLV(lvname string, origlvname string, readonly bool) (string, error) {
	output, err := exec.Command(
		"lvcreate",
		"-kn",
		"-n", lvname,
		"-s", fmt.Sprintf("/dev/%s/%s", s.vgName, origlvname)).CombinedOutput()
	if err != nil {
		s.log.Debug("Could not create LV snapshot", log.Ctx{"lvname": lvname, "origlvname": origlvname, "output": string(output)})
		return "", fmt.Errorf("Could not create snapshot LV named %s", lvname)
	}

	snapshotFullName := fmt.Sprintf("/dev/%s/%s", s.vgName, lvname)

	if readonly {
		output, err = exec.Command("lvchange", "-ay", "-pr", snapshotFullName).CombinedOutput()
	} else {
		output, err = exec.Command("lvchange", "-ay", snapshotFullName).CombinedOutput()
	}

	if err != nil {
		return "", fmt.Errorf("Could not activate new snapshot '%s': %v\noutput:%s", lvname, err, string(output))
	}

	return snapshotFullName, nil
}

func (s *storageLvm) isLVMContainer(container container) bool {
	return shared.PathExists(fmt.Sprintf("%s.lv", container.Path()))
}

func (s *storageLvm) renameLV(oldName string, newName string) (string, error) {
	output, err := exec.Command("lvrename", s.vgName, oldName, newName).CombinedOutput()
	return string(output), err
}

func (s *storageLvm) MigrationType() MigrationFSType {
	return MigrationFSType_RSYNC
}

func (s *storageLvm) MigrationSource(container container) ([]MigrationStorageSource, error) {
	return rsyncMigrationSource(container)
}

func (s *storageLvm) MigrationSink(container container, snapshots []container, conn *websocket.Conn) error {
	return rsyncMigrationSink(container, snapshots, conn)
}
