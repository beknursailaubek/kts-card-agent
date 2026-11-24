package card

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"kaztachosystem/card-agent/cardtest"
)

func TestExpectedSizeTable(t *testing.T) {
	ai := AppIdent{Gen: 1, EventsPerType: 12, FaultsPerType: 24, ActivityLength: 13776, VehicleRecords: 200, PlaceRecords: 112}
	for fid, want := range map[uint16]int{0x0502: 1728, 0x0503: 1152, 0x0504: 13780, 0x0505: 6202, 0x0506: 1121, 0x0522: 280, 0x0520: 143, 0x050E: 4, 0xC100: 194} {
		if got, ok := ExpectedSize(fid, 0, ai, nil); !ok || got != want {
			t.Errorf("%04X: получили %d (известен=%v), ждали %d", fid, got, ok, want)
		}
	}
	if _, ok := ExpectedSize(0x0525, 0, ai, nil); ok {
		t.Error("0525 в Gen1 не существует")
	}
	if got, ok := ExpectedSize(0xC100, 2, AppIdent{Gen: 2}, []byte{0x7F, 0x21, 0x81, 0xC8, 0x7F}); !ok || got != 204 {
		t.Errorf("сертификат Gen2: %d %v", got, ok)
	}
}

func TestDetectWorkshop(t *testing.T) {
	m := cardtest.FromDDD(cardtest.Enc([]cardtest.Block{{Key: "0501:0", Data: append([]byte{2, 0, 0}, filled(8, 0)...)}}))
	info, err := Detect(m)
	if !errors.Is(err, ErrNotDriver) || info.Type != 2 {
		t.Fatalf("тип %d, ошибка %v", info.Type, err)
	}
	want := [][]byte{
		{0x00, 0xA4, 0x04, 0x0C, 0x06, 0xFF, 0x53, 0x4D, 0x52, 0x44, 0x54}, // SELECT AID Gen2 (на Gen1-карте — 6A82)
		{0x00, 0xA4, 0x04, 0x0C, 0x06, 0xFF, 0x54, 0x41, 0x43, 0x48, 0x4F}, // SELECT AID Gen1
		{0x00, 0xA4, 0x02, 0x0C, 0x02, 0x05, 0x01},                         // SELECT 0501
		{0x00, 0xB0, 0x00, 0x00, 0x03},                                     // READ BINARY, 3 байта
	}
	if len(m.Sent) != len(want) {
		t.Fatalf("команд %d, ждали %d: %X", len(m.Sent), len(want), m.Sent)
	}
	for i := range want {
		if !bytes.Equal(m.Sent[i], want[i]) {
			t.Errorf("команда %d: %X, ждали %X", i, m.Sent[i], want[i])
		}
	}
}

// Размеры блоков настоящих карт водителя должны сходиться с формулами по их же Application_Identification.
func TestExpectedSizeSamples(t *testing.T) {
	var dirs []string
	for _, v := range []string{"KTS_DEMO_DIR", "KTS_KZ_SAMPLES_DIR"} {
		if d := os.Getenv(v); d != "" {
			dirs = append(dirs, d)
		}
	}
	if len(dirs) == 0 {
		if os.Getenv("KTS_REQUIRE_SAMPLES") == "1" {
			t.Fatal("KTS_REQUIRE_SAMPLES=1, но KTS_DEMO_DIR и KTS_KZ_SAMPLES_DIR пусты")
		}
		t.Skip("образцы не заданы")
	}
	cards, zeroC109, checked := 0, 0, map[string]int{}
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			blocks, ok := cardtest.ParseDDD(src)
			files := cardtest.FromDDD(src).Files
			if !ok || files["0501:0"] == nil || files["0501:0"][0] != 1 {
				return nil
			}
			cards++
			ai1, err := ParseAppIdent(1, files["0501:0"], nil)
			if err != nil {
				t.Errorf("#%d: Gen1: %v", cards, err)
				return nil
			}
			ai2 := AppIdent{}
			if files["0501:2"] != nil {
				if ai2, err = ParseAppIdent(2, files["0501:2"], files["0525:2"]); err != nil {
					t.Errorf("#%d: Gen2: %v", cards, err)
					return nil
				}
			}
			for _, b := range blocks {
				tag := b.Key[5] - '0'
				if tag != 0 && tag != 2 {
					continue
				}
				var fid uint64
				for _, c := range b.Key[:4] {
					fid = fid<<4 | uint64(hexVal(byte(c)))
				}
				ai := ai1
				if tag == 2 {
					ai = ai2
				}
				want, known := ExpectedSize(uint16(fid), tag, ai, b.Data)
				if !known && b.Key == "c109:2" && isZero(b.Data) {
					// в образцах C109 (CA-link) — 204 нулевых байта без DER-заголовка: размер по заголовку не вывести
					zeroC109++
				} else if !known {
					t.Errorf("#%d: размер %s не известен", cards, b.Key)
				} else if want != len(b.Data) {
					t.Errorf("#%d: %s: формула %d, в файле %d", cards, b.Key, want, len(b.Data))
				} else {
					checked[b.Key]++
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if cards == 0 {
		t.Fatal("в образцах нет карт водителя")
	}
	t.Logf("c109:2 из нулей (размер без формулы): %d", zeroC109)
	t.Logf("карт водителя: %d, проверено блоков по ключам: %d", cards, len(checked))
}

func hexVal(c byte) byte {
	if c >= 'a' {
		return c - 'a' + 10
	}
	return c - '0'
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
