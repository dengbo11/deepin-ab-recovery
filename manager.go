// SPDX-FileCopyrightText: 2018 - 2022 UnionTech Software Technology Co., Ltd.
//
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/godbus/dbus"
	"golang.org/x/xerrors"
	"github.com/linuxdeepin/go-lib/dbusutil"
)

const (
	dbusPath        = "/com/deepin/ABRecovery"
	dbusInterface   = "com.deepin.ABRecovery"
	dbusServiceName = dbusInterface

	jobKindBackup        = "backup"
	jobKindRestore       = "restore"
	abBackupFinishedFile = "/tmp/ab-backup-finished"
)

var msgRollBack = Tr("Roll back to %s (%s)")

// ^ 相同的源字符串也定义在文件 misc/11_deepin_ab_recovery 中

//go:generate dbusutil-gen -type Manager manager.go
//go:generate dbusutil-gen em -type Manager

type Manager struct {
	service       *dbusutil.Service
	PropsMu       sync.RWMutex
	BackingUp     bool
	Restoring     bool
	ConfigValid   bool
	BackupVersion string
	BackupTime    int64
	HasBackedUp   bool

	cfg Config

	//nolint
	signals *struct {
		JobEnd struct {
			kind    string
			success bool
			errMsg  string
		}
	}
}

func newManager(service *dbusutil.Service) *Manager {
	m := &Manager{
		service: service,
	}
	m.HasBackedUp = backupFinishedFileExist(abBackupFinishedFile)
	//var cfg Config
	err := loadConfig(configFile, &m.cfg)
	if err != nil {
		logger.Warning("failed to load config:", err)
	}
	logger.Debug("current:", m.cfg.Current)
	logger.Debug("backup:", m.cfg.Backup)

	err = m.cfg.check()
	if err != nil {
		logger.Warning(err)
	}
	m.ConfigValid = err == nil

	if m.ConfigValid {
		if m.cfg.Time != nil {
			m.BackupTime = m.cfg.Time.Unix()
		}
		m.BackupVersion = m.cfg.Version
	}

	return m
}

func (m *Manager) GetInterfaceName() string {
	return dbusInterface
}

func (m *Manager) canBackup() (bool, error) {
	if globalNoGrubMkconfig {
		if isArchMips() {
			// pass
		} else if isArchSw() {
			// pass
		} else {
			return false, nil
		}
	}

	if !m.ConfigValid {
		return false, nil
	}

	rootUuid, err := getRootUuid()
	if err != nil {
		return false, err
	}
	return rootUuid == m.cfg.Current, nil
}

func (m *Manager) CanBackup() (can bool, busErr *dbus.Error) {
	can, err := m.canBackup()
	return can, dbusutil.ToError(err)
}

func (m *Manager) canRestore() (bool, error) {
	if globalNoGrubMkconfig {
		if isArchMips() {
			// pass
		} else if isArchSw() {
			// pass
		} else {
			return false, nil
		}
	}

	if !m.ConfigValid {
		return false, nil
	}
	rootUuid, err := getRootUuid()
	if err != nil {
		return false, err
	}
	return rootUuid == m.cfg.Backup, nil
}

func (m *Manager) CanRestore() (can bool, busErr *dbus.Error) {
	can, err := m.canRestore()
	return can, dbusutil.ToError(err)
}

func (m *Manager) startBackup(envVars []string) error {
	can, err := m.canBackup()
	if err != nil {
		return err
	}

	if !can {
		return errors.New("backup cannot be performed")
	}

	m.PropsMu.Lock()
	if m.BackingUp {
		m.PropsMu.Unlock()
		return nil
	}

	m.BackingUp = true
	m.PropsMu.Unlock()
	err = m.emitPropChangedBackingUp(true)
	if err != nil {
		logger.Warning(err)
	}

	go func() {
		err := m.backup(envVars)
		if err != nil {
			logger.Warning("failed to backup:", err)
		}
		m.emitSignalJobEnd(jobKindBackup, err)

		m.PropsMu.Lock()
		m.setPropBackingUp(false)
		if err == nil {
			backupTime := m.cfg.Time.Unix()
			m.setPropBackupTime(backupTime)
			m.setPropBackupVersion(m.cfg.Version)
			creatFile(abBackupFinishedFile)
			m.setPropHasBackedUp(true)
		}
		m.PropsMu.Unlock()

	}()

	return nil
}

func (m *Manager) StartBackup(sender dbus.Sender) *dbus.Error {
	envVars, err := getLocaleEnvVarsWithSender(m.service, sender)
	if err != nil {
		return dbusutil.ToError(err)
	}
	err = m.startBackup(envVars)
	return dbusutil.ToError(err)
}

func (m *Manager) startRestore(envVars []string) error {
	can, err := m.canRestore()
	if err != nil {
		return err
	}

	if !can {
		return errors.New("restore cannot be performed")
	}

	m.PropsMu.Lock()
	if m.Restoring {
		m.PropsMu.Unlock()
		return nil
	}

	m.Restoring = true
	m.PropsMu.Unlock()
	err = m.emitPropChangedRestoring(true)
	if err != nil {
		logger.Warning(err)
	}

	go func() {
		err := m.restore(envVars)
		if err != nil {
			logger.Warning("failed to restore:", err)
		}
		m.emitSignalJobEnd(jobKindRestore, err)

		m.PropsMu.Lock()
		m.Restoring = false
		m.PropsMu.Unlock()

		err = m.emitPropChangedRestoring(false)
		if err != nil {
			logger.Warning(err)
		}
	}()

	return nil
}

func (m *Manager) StartRestore(sender dbus.Sender) *dbus.Error {
	envVars, err := getLocaleEnvVarsWithSender(m.service, sender)
	if err != nil {
		return dbusutil.ToError(err)
	}
	err = m.startRestore(envVars)
	return dbusutil.ToError(err)
}

func inhibitShutdownDo(why string, fn func() error) error {
	bootRo, err := isMountedRo("/boot")
	if err != nil {
		return xerrors.Errorf("isMountedRo: %w", bootRo)
	}
	if bootRo {
		err = exec.Command("mount", "/boot", "-o", "rw,remount").Run()
		if err != nil {
			return xerrors.Errorf("remount /boot rw: %w", err)
		}
		defer func() {
			// 把 /boot 恢复为只读
			err := exec.Command("mount", "/boot", "-o", "ro,remount").Run()
			if err != nil {
				logger.Warning("failed to remount /boot ro:")
			}
		}()
	}

	fd, iErr := inhibit("shutdown", dbusInterface, why)
	if iErr != nil {
		logger.Warning("failed to inhibit:", iErr)
	}
	err = fn()

	if iErr == nil {
		err := syscall.Close(int(fd))
		if err != nil {
			logger.Warningf("failed to close fd %d: %v", int(fd), err)
		}
	}
	return err
}

func (m *Manager) backup(envVars []string) error {
	return inhibitShutdownDo(Tr("Backing up the system"), func() error {
		return backup(&m.cfg, envVars)
	})
}

func (m *Manager) restore(envVars []string) error {
	return inhibitShutdownDo(Tr("Restoring the system"), func() error {
		return restore(&m.cfg, envVars)
	})
}

func Tr(text string) string {
	return text
}

func (m *Manager) emitSignalJobEnd(kind string, err error) {
	switch kind {
	case jobKindBackup, jobKindRestore:
		// pass
	default:
		panic("invalid kind " + kind)
	}
	var errMsg string
	if err != nil {
		errMsg = err.Error()
	}
	success := err == nil
	emitErr := m.service.Emit(m, "JobEnd", kind, success, errMsg)
	if emitErr != nil {
		logger.Warning(err)
	}
}

func (m *Manager) canQuit() bool {
	m.PropsMu.Lock()
	can := !m.BackingUp && !m.Restoring
	m.PropsMu.Unlock()
	return can
}

func creatFile(path string) {
	f, err := os.Create(path)
	if err != nil {
		logger.Warning(err)
		return
	}
	defer func() {
		_ = f.Close()
	}()
}

func backupFinishedFileExist(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	sysInfo := info.Sys().(*syscall.Stat_t)
	return sysInfo.Gid == 0 && sysInfo.Uid == 0
}
