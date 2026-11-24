//go:build windows

package install

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	base        = `Software\Microsoft\Windows\CurrentVersion\`
	shutdownURL = "http://127.0.0.1:47340/v1/shutdown"
)

// Shutdown просит запущенную программу выйти (без Origin) и ждёт до 5 с, пока порт освободится.
func Shutdown() {
	cl := &http.Client{Timeout: 12 * time.Second} // обработчик ждёт конца записи на карту до 10 с
	resp, err := cl.Post(shutdownURL, "application/json", bytes.NewReader(nil))
	if err != nil {
		return // не запущена
	}
	resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := cl.Post(shutdownURL, "application/json", bytes.NewReader(nil)); err != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Start запускает установленную копию обычным процессом (окна у неё нет: собрана с -H=windowsgui).
func Start(exe string) error {
	cmd := exec.Command(exe)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// Install копирует src в exe поверх старой версии, пишет реестр HKCU и запускает установленную программу.
func Install(src, exe, version string) error {
	if err := os.MkdirAll(exe[:strings.LastIndex(exe, `\`)], 0o755); err != nil {
		return err
	}
	if err := copyFile(src, exe); err != nil {
		return err
	}
	for k, v := range RegistryValues(exe, version) {
		i := strings.LastIndex(k, `\`)
		path, name := k[:i], k[i+1:]
		if strings.HasPrefix(k, `Run\`) {
			path, name = `Run`, k[len(`Run\`):]
		}
		key, _, err := registry.CreateKey(registry.CURRENT_USER, base+path, registry.SET_VALUE)
		if err != nil {
			return err
		}
		if name == "NoModify" || name == "NoRepair" {
			err = key.SetDWordValue(name, 1)
		} else {
			err = key.SetStringValue(name, v)
		}
		key.Close()
		if err != nil {
			return err
		}
	}
	return Start(exe)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Uninstall снимает автозапуск и запись удаления, переименовывает свой exe в %TEMP% и удаляет папку установки.
// dir берётся только из Paths, не из os.Executable(). keptDir — папка осталась, её можно удалить вручную.
func Uninstall(dir string) (keptDir bool, err error) {
	if err := CheckInstallDir(dir); err != nil {
		return false, err
	}
	var errs []error
	if k, err := registry.OpenKey(registry.CURRENT_USER, base+`Run`, registry.SET_VALUE); err == nil {
		if err := k.DeleteValue(regName); err != nil && !errors.Is(err, registry.ErrNotExist) {
			errs = append(errs, err)
		}
		k.Close()
	} else if !errors.Is(err, registry.ErrNotExist) {
		errs = append(errs, err)
	}
	if err := registry.DeleteKey(registry.CURRENT_USER, base+`Uninstall\`+regName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		errs = append(errs, err)
	}
	// работающий exe удалить нельзя, а переименовать на том же диске Windows даёт
	// наличие exe проверяем явно: ошибка Rename «нет пути» может быть и про отсутствующий %TEMP%
	exe := dir + `\` + ExeName
	if _, err := os.Stat(exe); !os.IsNotExist(err) {
		old, err := OldExePath(os.TempDir(), time.Now())
		if err == nil {
			err = os.Rename(exe, old)
		}
		if err != nil {
			return true, errors.Join(errs...)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return true, errors.Join(append(errs, err)...)
	}
	return false, errors.Join(errs...)
}

// Ask — окно «Да/Нет»; true только при «Да», ошибка вызова — нет.
func Ask(text string) bool {
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString(AppName)
	ret, err := windows.MessageBox(0, t, c, windows.MB_YESNO|windows.MB_ICONQUESTION)
	return err == nil && AnswerYes(ret)
}

// Message — окно сообщения (программа собрана без консоли).
func Message(text string) {
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString(AppName)
	_, _ = windows.MessageBox(0, t, c, windows.MB_OK|windows.MB_ICONINFORMATION)
}

// Lock — именованный мьютекс сеанса; false — программа уже запущена.
func Lock() (bool, error) {
	n, _ := windows.UTF16PtrFromString(`Local\` + regName)
	_, err := windows.CreateMutex(nil, false, n)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("мьютекс: %w", err)
	}
	return true, nil // дескриптор живёт до конца процесса
}
