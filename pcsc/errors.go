// Package pcsc — адаптер PC/SC (Windows) и сопоставление кодов ошибок.
package pcsc

import (
	"fmt"

	"kaztachosystem/card-agent/agent"
)

// NoReaders — служба или кардридеры отсутствуют: для списка кардридеров это пустой список, не ошибка.
func NoReaders(code uint32) bool {
	return code == 0x8010002E || code == 0x8010001D || code == 0x8010001E
}

// ErrorOf сопоставляет код PC/SC ошибке агента; nil — «нет кардридеров/службы».
func ErrorOf(code uint32) error {
	switch code {
	case 0x8010000B:
		return agent.ErrBusy
	case 0x80100069, 0x8010000C, 0x80100068:
		return agent.ErrNoCard
	}
	if NoReaders(code) {
		return nil
	}
	return fmt.Errorf("PC/SC 0x%08X", code)
}
