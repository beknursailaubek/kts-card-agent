# kts-card-agent

A small Windows program that reads a tachograph driver card through a PC/SC card reader and hands the
resulting file to the KazTachoSystem web page.

## What it does

- Reads the driver card (PC/SC) and returns the card file only when the user presses the button on the
  KazTachoSystem page.
- Listens on `127.0.0.1:47340` only, and answers only the web addresses listed in
  [`agent/origins_release.go`](agent/origins_release.go).
- When a carrier downloads the card, it writes the download date to the card.
- Does not write personal card data to its log.

## Install

Run `kts-card-agent.exe` downloaded from the KazTachoSystem page. It asks for confirmation first, then installs
into the user profile (`%LOCALAPPDATA%`), without administrator rights, and starts automatically at sign-in.

## Uninstall

Run `kts-card-agent.exe -uninstall`, or use Settings > Apps and remove "KazTachoSystem — считывание карт".

## Build

```sh
./build.sh
```

Produces `dist/kts-card-agent.exe` and `dist/kts-card-agent.json`. The version is in the `VERSION` file.
A release is built by GitHub Actions when a `v<VERSION>` tag is pushed.

## Tests

```sh
go vet ./... && GOOS=windows go vet ./...
go test ./...
```

Tests on real card files are skipped unless these environment variables point to directories with sample
cards (there are no defaults): `KTS_DEMO_DIR`, `KTS_KZ_SAMPLES_DIR`. Set `KTS_REQUIRE_SAMPLES=1` to fail instead
of skip. Sample cards contain personal data and are not stored in this repository.

## Code signing policy

Free code signing provided by [SignPath.io](https://signpath.io), certificate by [SignPath Foundation](https://signpath.org)

Authors, Reviewers, Approvers: [@beknursailaubek](https://github.com/beknursailaubek)

This program will not transfer any information to other networked systems unless specifically requested by the user or the person installing or operating it.

## License

[MIT](LICENSE). Third-party modules:

- [`github.com/ebfe/scard`](https://github.com/ebfe/scard) — BSD 2-Clause license
- [`golang.org/x/sys`](https://pkg.go.dev/golang.org/x/sys) — BSD 3-Clause license

## По-русски

Программа для Windows читает карту водителя тахографа через PC/SC-кардридер и по нажатию пользователя передаёт
файл странице KazTachoSystem. Устанавливается в профиль пользователя без прав администратора, перед установкой
спрашивает подтверждение; удаляется командой `kts-card-agent.exe -uninstall` или в «Параметры» → «Приложения».
