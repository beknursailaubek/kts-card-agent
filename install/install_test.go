package install

import (
	"strings"
	"testing"
	"time"
)

const local = `C:\Users\u\AppData\Local`

func TestPaths(t *testing.T) {
	exe, dir, logDir, err := Paths(local)
	if err != nil {
		t.Fatal(err)
	}
	if exe != local+`\Programs\KazTachoSystem\kts-card-agent.exe` || dir != local+`\Programs\KazTachoSystem` || logDir != local+`\KazTachoSystem` {
		t.Fatal(exe, dir, logDir)
	}
	for _, bad := range []string{"", "relative", `\\srv\share`} {
		if _, _, _, err := Paths(bad); err == nil {
			t.Fatalf("Paths(%q) без ошибки", bad)
		}
	}
}

func TestModeOf(t *testing.T) {
	exe, _, _, _ := Paths(local)
	if ModeOf(`C:\Users\u\Downloads\kts-card-agent.exe`, exe) != ModeInstall {
		t.Fatal("ожидался ModeInstall")
	}
	if ModeOf(strings.ToUpper(exe), exe) != ModeRun {
		t.Fatal("ожидался ModeRun")
	}
}

func TestRegistryValues(t *testing.T) {
	exe, _, _, _ := Paths(local)
	v := RegistryValues(exe, "1.0.0")
	want := map[string]string{
		`Run\KazTachoSystemCardAgent`:                       `"` + exe + `"`,
		`Uninstall\KazTachoSystemCardAgent\DisplayName`:     "KazTachoSystem — считывание карт",
		`Uninstall\KazTachoSystemCardAgent\DisplayVersion`:  "1.0.0",
		`Uninstall\KazTachoSystemCardAgent\Publisher`:       "KazTachoSystem",
		`Uninstall\KazTachoSystemCardAgent\UninstallString`: `"` + exe + `" --uninstall`,
		`Uninstall\KazTachoSystemCardAgent\InstallLocation`: strings.TrimSuffix(exe, `\kts-card-agent.exe`),
		`Uninstall\KazTachoSystemCardAgent\NoModify`:        "1",
		`Uninstall\KazTachoSystemCardAgent\NoRepair`:        "1",
	}
	if len(v) != len(want) {
		t.Fatal(v)
	}
	for k, w := range want {
		if v[k] != w {
			t.Errorf("%s = %q, ожидалось %q", k, v[k], w)
		}
		if strings.Contains(v[k], "Seven") {
			t.Errorf("%s содержит Seven", k)
		}
	}
}

func TestConfirmText(t *testing.T) {
	got := ConfirmText("1.1.0")
	want := "Установить программу считывания карт KazTachoSystem (версия 1.1.0)?\n\n" +
		"Программа будет запускаться вместе с Windows и работать в фоне — через неё система видит кардридер.\n" +
		"Удалить её можно в «Параметры» → «Приложения» → «KazTachoSystem — считывание карт»."
	if got != want {
		t.Fatalf("ConfirmText = %q", got)
	}
}

func TestAnswerYes(t *testing.T) {
	for ret, want := range map[int32]bool{6: true, 7: false, 2: false, 0: false} {
		if AnswerYes(ret) != want {
			t.Errorf("AnswerYes(%d) != %v", ret, want)
		}
	}
}

func TestOldExePath(t *testing.T) {
	now := time.Unix(1795000000, 0)
	got, err := OldExePath(`C:\Users\u\AppData\Local\Temp\`, now)
	if err != nil || got != `C:\Users\u\AppData\Local\Temp\kts-card-agent-1795000000.old` {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range []string{"", `Temp`, `\\server\share`} {
		if _, err := OldExePath(bad, now); err == nil {
			t.Errorf("OldExePath(%q) без ошибки", bad)
		}
	}
}

func TestCheckInstallDir(t *testing.T) {
	_, dir, _, _ := Paths(`C:\Users\u\AppData\Local`)
	if err := CheckInstallDir(dir); err != nil {
		t.Fatalf("своя папка: %v", err)
	}
	for _, bad := range []string{`C:\Users\u\AppData\Local`, `C:\`, `C:\Users\u\AppData\Local\Programs`, ""} {
		if CheckInstallDir(bad) == nil {
			t.Errorf("CheckInstallDir(%q) без ошибки", bad)
		}
	}
}
