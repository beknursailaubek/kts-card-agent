package pcsc

import (
	"errors"
	"testing"

	"kaztachosystem/card-agent/agent"
)

func TestErrorOf(t *testing.T) {
	cases := []struct {
		code uint32
		want error
	}{
		{0x8010000B, agent.ErrBusy},
		{0x80100069, agent.ErrNoCard},
		{0x8010000C, agent.ErrNoCard},
		{0x80100068, agent.ErrNoCard},
	}
	for _, c := range cases {
		if err := ErrorOf(c.code); !errors.Is(err, c.want) {
			t.Errorf("%#x: %v", c.code, err)
		}
	}
	for _, code := range []uint32{0x8010002E, 0x8010001D, 0x8010001E} {
		if ErrorOf(code) != nil || !NoReaders(code) {
			t.Errorf("%#x: ожидался nil", code)
		}
	}
	if err := ErrorOf(0x80100001); err == nil || errors.Is(err, agent.ErrBusy) {
		t.Errorf("прочее: %v", err)
	}
}
