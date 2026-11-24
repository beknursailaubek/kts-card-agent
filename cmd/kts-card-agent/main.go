//go:build windows

// kts-card-agent — программа считывания карт водителя: ставится сама (без прав администратора), слушает 127.0.0.1:47340.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"kaztachosystem/card-agent/agent"
	"kaztachosystem/card-agent/install"
	"kaztachosystem/card-agent/logfile"
	"kaztachosystem/card-agent/pcsc"
)

var version = "0.0.0-dev"

func main() {
	uninstall := flag.Bool("uninstall", false, "удалить программу")
	flag.Parse()

	exe, dir, logDir, err := install.Paths(os.Getenv("LOCALAPPDATA"))
	if err != nil {
		install.Message("Не удалось определить папку установки: " + err.Error())
		return
	}
	if *uninstall {
		install.Shutdown()
		kept, err := install.Uninstall(dir)
		if err != nil {
			install.Message("Удаление выполнено не полностью: " + err.Error())
		} else if kept {
			install.Message("Программа удалена. Папку " + dir + " можно удалить вручную.")
		}
		return
	}
	self, err := os.Executable()
	if err != nil {
		install.Message("Не удалось определить путь программы: " + err.Error())
		return
	}
	if install.ModeOf(self, exe) == install.ModeInstall {
		if !install.Ask(install.ConfirmText(version)) {
			return
		}
		install.Shutdown()
		if err := install.Install(self, exe, version); err != nil {
			install.Message("Установка не удалась: " + err.Error())
			return
		}
		install.Message("Программа установлена и работает. Вернитесь в браузер.")
		return
	}
	run(logDir)
}

func run(logDir string) {
	if ok, err := install.Lock(); err != nil || !ok {
		return
	}
	w, err := logfile.Open(logDir, 1<<20)
	if err != nil {
		return
	}
	defer w.Close()
	lg := log.New(w, "", log.LstdFlags)

	ln, err := net.Listen("tcp", "127.0.0.1:47340")
	if err != nil {
		lg.Println("порт 47340 занят:", err)
		return
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	srv := &http.Server{}
	var once sync.Once
	srv.Handler = agent.New(agent.Config{
		Version: version, Origins: agent.Origins, PCSC: pcsc.New(), Now: time.Now, Log: lg, Done: done,
		OnShutdown: func() {
			once.Do(func() {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					_ = srv.Shutdown(ctx)
					close(stopped)
				}()
			})
		},
	})
	lg.Println("запущена, версия", version)
	err = srv.Serve(ln)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		lg.Println("сервер остановлен:", err)
	}
	if errors.Is(err, http.ErrServerClosed) {
		<-stopped // Serve возвращается сразу при Shutdown, а он ещё ждёт идущие запросы
	}
	close(done)
}
