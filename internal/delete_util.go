package internal

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wal-g/tracelog"
	"github.com/wal-g/wal-g/pkg/storages/storage"
	"github.com/wal-g/wal-g/utility"
)

func FindPermanentBackups(folder storage.Folder, metaFetcher GenericMetaFetcher) map[string]bool {
	tracelog.InfoLogger.Println("retrieving permanent objects")
	backupTimes, err := GetBackups(folder.GetSubFolder(utility.BaseBackupPath))
	if err != nil {
		return map[string]bool{}
	}

	permanentBackups := map[string]bool{}
	for _, backupTime := range backupTimes {
		meta, err := metaFetcher.Fetch(
			backupTime.BackupName, folder.GetSubFolder(utility.BaseBackupPath))
		if err != nil {
			tracelog.ErrorLogger.Printf("failed to fetch backup meta for backup %s with error %s, ignoring...",
				backupTime.BackupName, err.Error())
			continue
		}
		if meta.IsPermanent {
			permanentBackups[backupTime.BackupName] = true
		}
	}
	return permanentBackups
}

// IsPermanent is a generic function to determine if the storage object is permanent.
// It does not support permanent WALs or binlogs.
func IsPermanent(objectName string, permanentBackups map[string]bool, backupNameLength int) bool {
	if strings.HasPrefix(objectName, utility.BaseBackupPath) &&
		len(objectName) >= len(utility.BaseBackupPath)+backupNameLength {
		backup := objectName[len(utility.BaseBackupPath) : len(utility.BaseBackupPath)+backupNameLength]
		return permanentBackups[backup]
	}
	// impermanent backup or binlogs
	return false
}

func FindBackupObjects(folder storage.Folder) ([]BackupObject, error) {
	backups, err := GetBackupSentinelObjects(folder)
	if err != nil {
		return nil, err
	}

	backupObjects := make([]BackupObject, 0, len(backups))
	for _, object := range backups {
		b := NewDefaultBackupObject(object)
		backupObjects = append(backupObjects, b)
	}
	return backupObjects, nil
}

// create the BackupSelector to select the backup to delete
func CreateTargetDeleteBackupSelector(cmd *cobra.Command,
	args []string, targetUserData string, metaFetcher GenericMetaFetcher) (BackupSelector, error) {
	targetName := ""
	if len(args) > 0 {
		targetName = args[0]
	}

	backupSelector, err := NewTargetBackupSelector(targetUserData, targetName, metaFetcher)
	if err != nil {
		fmt.Println(cmd.UsageString())
		return nil, err
	}
	return backupSelector, nil
}
