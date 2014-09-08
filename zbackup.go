package main

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"syscall"
	"zbackup/zfs"

	"github.com/docopt/docopt-go"
	"github.com/op/go-logging"
	"github.com/theairkit/runcmd"
)

var (
	path       = "/etc/zbackup/zbackup.conf"
	pidfile    = "/var/run/zbackup.pid"
	pidfileErr = "pidfile already exists: "
	format     = "%{time:15:04:05.000000} %{pid} %{level:.8s} %{message}"
	log        = logging.MustGetLogger("zbackup")
)

func main() {
	usage := `
Usage:
  zbackup
  zbackup [-htc filename -p pidfile -v loglevel]

Options:
  -h             this help
  -t             test configuration and exit
  -c filename    set configuration file (default: /etc/zbackup/zbackup.conf)
  -p pidfile     set pidfile (default: /var/run/zbackup.pid)
  -v loglevel    set loglevel: normal,debug (default: normal)`

	var c Config
	arguments, _ := docopt.Parse(usage, nil, true, "1.0", false)
	loglevel := logging.INFO
	logBackend := logging.NewLogBackend(os.Stderr, "", 0)
	logging.SetBackend(logBackend)
	logging.SetFormatter(logging.MustStringFormatter(format))
	logging.SetLevel(loglevel, log.Module)

	if arguments["-c"] != nil {
		path = arguments["-c"].(string)
	}
	if arguments["-p"] != nil {
		pidfile = arguments["-p"].(string)
	}
	if arguments["-v"] != nil {
		switch arguments["-v"].(string) {
		case "info":
			loglevel = logging.INFO
		case "debug":
			loglevel = logging.DEBUG
		default:
			log.Error("Unknown loglevel, using loglevel: info")
		}
	}
	logging.SetLevel(loglevel, log.Module)

	if err := loadConfig(path, &c); err != nil {
		log.Error("error parsing config: %s", err.Error())
		return
	}
	fmt.Println(c.User, c.Host, c.Key, pidfile, path)
	log.Info("config ok")
	if arguments["-t"].(bool) {
		return
	}

	if _, err := os.Stat(pidfile); err == nil {
		log.Error(err.Error())
		return
	}
	pid, err := os.Create(pidfile)
	if err != nil {
		log.Error(err.Error())
		return
	}
	pid.WriteString(strconv.Itoa(syscall.Getpid()))

	wg := sync.WaitGroup{}
	mt := make(chan struct{}, c.MaxIoThreads)
	lRunner := zfs.NewZfs(runcmd.NewLocalRunner())
	for i := range c.Backup {
		fsList, err := lRunner.ListFs(c.Backup[i].Local, zfs.FS, c.Backup[i].Recursive)
		if err != nil {
			log.Error(err.Error())
			continue
		}
		for _, fs := range fsList {
			wg.Add(1)
			mt <- struct{}{}
			go func(gi int, l, r, e string) {
				log.Info("[%d]: starting backup: %s --> %s/%s (expire: %s)", gi, l, c.Host, r, e)
				if err := backup(gi, l, r, e, c.User, c.Host, c.Key); err != nil {
					log.Error("[%d]: %s", gi, err.Error())
				} else {
					log.Info("[%d]: finishing backup", gi)
				}
				<-mt
				wg.Done()
			}(i, fs, c.Backup[i].Remote, c.Backup[i].ExpireHours)
		}
	}
	wg.Wait()

	if err := os.Remove(pidfile); err != nil {
		log.Error(err.Error())
	}
}