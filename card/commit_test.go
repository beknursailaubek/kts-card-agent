package card

import (
	"bytes"
	"errors"
	"fmt"
	"kaztachosystem/card-agent/cardtest"
	"strings"
	"testing"
	"time"
)

var (
	iccID    = filled(25, 0x11)
	writeAt  = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	writeVal = []byte{0x6A, 0xFE, 0xC8, 0xA0} // 1795082400, big-endian
	oldDate  = []byte{0x60, 0, 0, 0}
)

func gen2WriteCard() *cardtest.Card {
	return cardtest.FromDDD(cardtest.Enc(gen2Blocks(
		cardtest.Block{Key: "0002:0", Data: iccID},
		cardtest.Block{Key: "050e:0", Data: oldDate}, cardtest.Block{Key: "050e:1", Data: filled(128, 9)},
		cardtest.Block{Key: "050e:2", Data: oldDate}, cardtest.Block{Key: "050e:3", Data: filled(64, 9)},
	)))
}

func sent(m *cardtest.Card) []string {
	var out []string
	for _, a := range m.Sent {
		out = append(out, fmt.Sprintf("%X", a))
	}
	return out
}

const (
	cmdICC = "00A4020C020002 00B0000019 00B0001901 "
	cmdG1  = "00A4040C06FF544143484F 00A4020C02050E 00D60000046AFEC8A0 "
	cmdG2  = "00A4040C06FF534D524454 00A4020C02050E 00D60000046AFEC8A0 "
)

func mustSent(t *testing.T, m *cardtest.Card, want string) {
	t.Helper()
	if got := strings.Join(sent(m), " "); got != strings.TrimSpace(want) {
		t.Fatalf("команды:\n%s\nждали:\n%s", got, want)
	}
}

func writeGen2(t *testing.T, info Info) {
	m := gen2WriteCard()
	if err := WriteDownloadDate(m, info, iccID, writeAt); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"050e:0", "050e:2"} {
		if !bytes.Equal(m.Files[k], writeVal) {
			t.Fatalf("%s = %X, ждали %X", k, m.Files[k], writeVal)
		}
	}
	mustSent(t, m, cmdICC+cmdG1+cmdG2)
}

func TestWriteGen2(t *testing.T)   { writeGen2(t, Info{Generation: 2, Type: 1}) }
func TestWriteGen2v2(t *testing.T) { writeGen2(t, Info{Generation: 2, V2: true, Type: 1}) }

func TestWriteGen1(t *testing.T) {
	m := cardtest.FromDDD(cardtest.Enc([]cardtest.Block{
		{Key: "0002:0", Data: iccID},
		{Key: "0501:0", Data: []byte{1, 0, 0, 3, 3, 0x35, 0xD0, 0, 3, 3}}, {Key: "0501:1", Data: filled(128, 4)},
		{Key: "050e:0", Data: oldDate}, {Key: "050e:1", Data: filled(128, 9)},
	}))
	if err := WriteDownloadDate(m, Info{Generation: 1, Type: 1}, iccID, writeAt); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(m.Files["050e:0"], writeVal) {
		t.Fatalf("050e:0 = %X", m.Files["050e:0"])
	}
	mustSent(t, m, cmdICC+cmdG1)
}

func TestCardChanged(t *testing.T) {
	for name, m := range map[string]*cardtest.Card{
		"другой ICC":   gen2WriteCard(),
		"карту вынули": func() *cardtest.Card { m := gen2WriteCard(); m.RemoveAfter = 1; return m }(),
		"нет EF ICC":   cardtest.FromDDD(cardtest.Enc(gen2Blocks(cardtest.Block{Key: "050e:0", Data: oldDate}))),
		"ICC короче":   func() *cardtest.Card { m := gen2WriteCard(); m.Cut = map[string]int{"0002:0": 24}; return m }(),
	} {
		icc := iccID
		if name == "другой ICC" {
			icc = filled(25, 0x12)
		}
		before := snapshot(m)
		err := WriteDownloadDate(m, Info{Generation: 2, Type: 1}, icc, writeAt)
		if !errors.Is(err, ErrCardChanged) {
			t.Errorf("%s: ждали ErrCardChanged, получили %v", name, err)
		}
		readOnly(t, m, before)
	}
}

func TestSecondWriteFails(t *testing.T) {
	m := gen2WriteCard()
	m.FailWrite = 2
	err := WriteDownloadDate(m, Info{Generation: 2, Type: 1}, iccID, writeAt)
	if !errors.Is(err, ErrWrite) {
		t.Fatalf("ждали ErrWrite, получили %v", err)
	}
	if !bytes.Equal(m.Files["050e:0"], writeVal) || !bytes.Equal(m.Files["050e:2"], oldDate) {
		t.Fatalf("после сбоя: 050e:0 = %X, 050e:2 = %X", m.Files["050e:0"], m.Files["050e:2"])
	}
	// повтор после сброса карты (новая сессия, сбоя нет): та же дата ещё раз — безвредно
	m = &cardtest.Card{Files: m.Files}
	if err := WriteDownloadDate(m, Info{Generation: 2, Type: 1}, iccID, writeAt); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"050e:0", "050e:2"} {
		if !bytes.Equal(m.Files[k], writeVal) {
			t.Fatalf("%s = %X, ждали %X", k, m.Files[k], writeVal)
		}
	}
}
