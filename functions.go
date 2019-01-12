package main

import (
	"errors"
	"os"
	"strings"
	"time"

	"github.com/HolyGhost07/runcmd"
	"github.com/HolyGhost07/zfs"
)

type Backuper struct {
	srcZfs   *zfs.Zfs
	dstZfs   *zfs.Zfs
	config   *Config
	snapSuff string
}

type BackupTask struct {
	id       int
	src      string
	dst      string
	dstroot  string
	expire   string
	srcZfs   *zfs.Zfs
	dstZfs   *zfs.Zfs
	snapSuff string
}

var (
	errPrefix     = "'remote_prefix' and 'recursive' are mutually exclusive; skip this [[backup]] section"
	errPrefixMask = "'remote_prefix' and 'regexp' are mutually exclusive; skip this [[backup]] section"
	errRegex      = "'regexp' and 'recursive=true' are mutually exclusive; skip this [[backup]] section"
	warnPrefixFs  = "'remote_prefix' set; fs with this name on remote may be overwritten"
	warnExpire    = "expire_hours not set, will not delete old backups"
	snapExist     = "already exists, wait next minute and run again"
	timeFormat    = "2006-01-02T15:04"
	snapCurr      = "zbackup_curr_"
	snapNew       = "zbackup_new_"
	PROPERTY      = "zbackup:"
	h, _          = os.Hostname()
)

func NewBackuper(c *Config, snapSuff string) (*Backuper, error) {
	srcZfs, err := zfs.NewZfs(runcmd.NewLocalRunner())
	if err != nil {
		return nil, err
	}
	dstZfs, err := zfs.NewZfs(runcmd.NewRemoteKeyAuthRunner(c.User, c.Host, c.Key))
	if err != nil {
		return nil, err
	}
	return &Backuper{srcZfs, dstZfs, c, snapSuff}, nil
}

func (backuper *Backuper) setupTasks() []BackupTask {
	tasks := make([]BackupTask, 0)
	taskid := 0
	config := backuper.config.Backup

	for _, backup := range config {
		if backup.RemotePrefix != "" && backup.Recursive {
			log.Errorf("%s: %s", backup.Local, errPrefix)
			continue
		}
		if backup.Recursive && strings.HasSuffix(backup.Local, "*") {
			log.Errorf("%s: %s", backup.Local, errRegex)
			continue
		}

		fsList, err := backuper.srcZfs.List(backup.Local, zfs.FS, backup.Recursive)
		if err != nil {
			log.Errorf("error get filesystems: %s", err.Error())
			continue
		}
		for _, src := range fsList {
			dst := backup.RemoteRoot + "/" + h + "-" + strings.Replace(src, "/", "-", -1)
			if backup.RemotePrefix != "" {
				dst = backup.RemoteRoot + "/" + backup.RemotePrefix
			}
			tasks = append(tasks, BackupTask{
				taskid,
				src,
				dst,
				backup.RemoteRoot,
				backup.Expire,
				backuper.srcZfs,
				backuper.dstZfs,
				backuper.snapSuff,
			})
			taskid++
		}
	}
	return tasks
}

func (task *BackupTask) doBackup() error {
	snapPostfix := time.Now().Format(timeFormat)
	id := task.id
	src := task.src
	dst := task.dst
	snapCurr += task.snapSuff
	snapNew += task.snapSuff

	// Check if snapshot with timestamp-based name already exists:
	log.Debugf("[%d]: check %s exists", id, dst+"@"+snapPostfix)
	if exist, err := task.dstZfs.ExistSnap(dst, snapPostfix); err != nil || exist {
		if err != nil {
			return err
		}
		return errors.New(dst + "@" + snapPostfix + " " + snapExist)
	}

	// Check, if backup for the first time or not:
	// @zbackup_curr not exist: create it and send
	// @zbackup_curr exist: create @zbackup_new, and send delta between them
	log.Debugf("[%d]: check %s exists", id, src+"@"+snapCurr)
	if exist, err := task.srcZfs.ExistSnap(src, snapCurr); err != nil || !exist {
		if err != nil {
			return err
		}
		snapNew = ""
	}

	// Backup:
	if err := task.backupHelper(snapNew); err != nil {
		return err
	}

	// Cleanup:
	return task.cleanExpired()
}

func (task *BackupTask) backupHelper(snapNew string) error {
	snapPostfix := time.Now().Format(timeFormat)
	id := task.id
	src := task.src
	dst := task.dst

	snap := snapCurr
	if snapNew != "" {
		snap = snapNew
	}

	log.Debugf("[%d]: create snapshot: %s...", id, snap)
	if err := task.srcZfs.CreateSnap(src, snap); err != nil {
		return err
	}

	log.Debugf("[%d]: check, if %s exists...", id, task.dstroot)
	exist, err := task.dstZfs.ExistFs(task.dstroot)
	if err != nil {
		return err
	}
	if !exist {
		task.dstZfs.CreateFs(task.dstroot)
	}

	log.Debugf("[%d]: start recieve snapshot on remote...", id)
	cmdRecv, err := task.dstZfs.RecvSnap(dst, snapPostfix)
	if err != nil {
		return err
	}

	log.Debugf("[%d]: copy snapshot from local to remote...", id)
	err = task.srcZfs.SendSnap(src, snapCurr, snapNew, cmdRecv)
	if err != nil {
		return err
	}

	if snapNew != "" {
		log.Debug("[%d]: rotate snapshots (destroy @curr, move @new to @curr)...", id)
		if err := task.srcZfs.Destroy(src + "@" + snapCurr); err != nil {
			return err
		}
		if err := task.srcZfs.RenameSnap(src, snapNew, snapCurr); err != nil {
			return err
		}
	}

	log.Debugf("[%d]: set remote %s 'readonly'...", id, dst)
	if err := task.dstZfs.SetProperty(dst, "readonly", "on"); err != nil {
		return err
	}

	log.Debugf("[%d]: set remote %s 'zbackup:=true'...", id, dst+snapPostfix)
	return task.dstZfs.SetProperty(dst+"@"+snapPostfix, PROPERTY, "true")
}

func (task *BackupTask) cleanExpired() error {
	id := task.id
	dst := task.dst
	expire := task.expire

	log.Debugf("[%d]: cleaning expired snapshots, expire: %s", id, expire)
	if expire == "" {
		log.Infof("[%d]: expire is not set, exit", id)
		return nil
	}

	recent, err := task.dstZfs.RecentSnap(dst, PROPERTY)
	if err != nil {
		return err
	}

	log.Debugf("[%d]: get list remote snapshots...", id)
	list, err := task.dstZfs.ListFsSnap(dst)
	if err != nil {
		return err
	}
	if len(list) == 1 {
		log.Infof("[%d] only one snapshot, nothing to delete", id)
		return nil
	}
	for _, snap := range list {
		out, err := task.dstZfs.Property(snap, "zbackup:")
		if err != nil {
			return err
		}
		if out != "true" {
			log.Debugf("[%d]: %s is not created by zbackup, skipping", id, snap)
			continue
		}
		if task.expire == "lastone" {
			if snap != recent {
				log.Debugf("[%d]: %s will be destroy (not recent)", id, snap)
				if err := task.dstZfs.Destroy(snap); err != nil {
					log.Errorf("[%d]: error destroying %s: %s", id, snap, err.Error())
				}
			}
			continue
		}

		poolDate, _ := time.ParseInLocation(
			timeFormat,
			strings.Split(snap, "@")[1],
			time.Local,
		)
		expire, _ := time.ParseDuration(expire)
		if time.Since(poolDate) > expire {
			log.Debugf("[%d]: %s will be destroy (>%s)", id, snap, expire)

			if err := task.dstZfs.Destroy(snap); err != nil {
				log.Errorf("[%d]: error destroying %s: %s", id, snap, err.Error())
				continue
			}
		} else {
			log.Debugf("[%d]: %s not exipred, skipping", id, snap)
		}
	}
	return nil
}
