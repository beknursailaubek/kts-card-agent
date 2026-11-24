// Package install — пути, режим запуска и записи реестра программы; чистые функции, проверяются на любой ОС.
package install

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

const (
	AppName = "KazTachoSystem"
	ExeName = "kts-card-agent.exe"
	regName = "KazTachoSystemCardAgent"
	progDir = `\Programs\` + AppName
	// DisplayName — имя программы в «Параметры» → «Приложения».
	DisplayName = "KazTachoSystem — считывание карт"
)

type Mode int

const (
	ModeInstall Mode = iota
	ModeRun
)

// Paths склеивает пути строками через «\» (не filepath: тесты идут на Linux).
func Paths(localAppData string) (exe, dir, logDir string, err error) {
	d := strings.TrimRight(localAppData, `\`)
	if !driveAbs(d) {
		return "", "", "", errors.New("LOCALAPPDATA пуст или не абсолютный путь")
	}
	dir = d + progDir
	return dir + `\` + ExeName, dir, d + `\` + AppName, nil
}

// driveAbs — абсолютный путь с буквой диска (`C:\…`), без завершающего «\».
func driveAbs(d string) bool {
	return len(d) >= 3 && d[1] == ':' && d[2] == '\\' && d[0]|0x20 >= 'a' && d[0]|0x20 <= 'z'
}

func ModeOf(exePath, installedExe string) Mode {
	if strings.EqualFold(exePath, installedExe) {
		return ModeRun
	}
	return ModeInstall
}

// RegistryValues — значения под HKCU\Software\Microsoft\Windows\CurrentVersion: ключ `Run\<имя>` — значение Run,
// `Uninstall\<ключ>\<имя>` — значение записи удаления.
func RegistryValues(exe, version string) map[string]string {
	u := `Uninstall\` + regName + `\`
	return map[string]string{
		`Run\` + regName:      `"` + exe + `"`,
		u + "DisplayName":     DisplayName,
		u + "DisplayVersion":  version,
		u + "Publisher":       AppName,
		u + "UninstallString": `"` + exe + `" --uninstall`,
		u + "InstallLocation": strings.TrimSuffix(exe, `\`+ExeName),
		u + "NoModify":        "1",
		u + "NoRepair":        "1",
	}
}

// ConfirmText — вопрос перед установкой и обновлением.
func ConfirmText(version string) string {
	return "Установить программу считывания карт " + AppName + " (версия " + version + ")?\n\n" +
		"Программа будет запускаться вместе с Windows и работать в фоне — через неё система видит кардридер.\n" +
		"Удалить её можно в «Параметры» → «Приложения» → «" + DisplayName + "»."
}

// AnswerYes — true только при IDYES (6); любой другой результат MessageBox, в том числе ошибка (0), — нет.
func AnswerYes(ret int32) bool { return ret == 6 }

// OldExePath — куда переименовать работающий exe при удалении: `<tempDir>\kts-card-agent-<unix-время>.old`.
func OldExePath(tempDir string, now time.Time) (string, error) {
	d := strings.TrimRight(tempDir, `\`)
	if !driveAbs(d) {
		return "", errors.New("TEMP пуст или не абсолютный путь")
	}
	return d + `\kts-card-agent-` + strconv.FormatInt(now.Unix(), 10) + ".old", nil
}

// CheckInstallDir — перед удалением: только папка установки программы, чужую не удаляем.
func CheckInstallDir(dir string) error {
	if !driveAbs(dir) || !strings.HasSuffix(dir, progDir) {
		return errors.New("не папка установки программы")
	}
	return nil
}
