package card

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"kaztachosystem/card-agent/cardtest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func filled(n int, v byte) []byte { return bytes.Repeat([]byte{v}, n) }

// gen1Card — синтетическая карта Gen1: 0504 длиной 4+13776 по 0501.
func gen1Card() *cardtest.Card {
	return cardtest.FromDDD(cardtest.Enc([]cardtest.Block{
		{Key: "0501:0", Data: []byte{1, 0, 0, 3, 3, 0x35, 0xD0, 0, 3, 3}}, {Key: "0501:1", Data: filled(128, 4)},
		{Key: "0520:0", Data: filled(143, 7)}, {Key: "0520:1", Data: filled(128, 8)},
		{Key: "0504:0", Data: filled(13780, 9)}, {Key: "0504:1", Data: filled(128, 10)},
	}))
}

// gen2Blocks — минимальная карта Gen2: по одной записи в каждой структуре; extra дописывается после 0520:3.
func gen2Blocks(extra ...cardtest.Block) []cardtest.Block {
	return append([]cardtest.Block{
		{Key: "0501:0", Data: []byte{1, 0, 0, 1, 1, 0, 100, 0, 1, 1}}, {Key: "0501:1", Data: filled(128, 1)},
		{Key: "0520:0", Data: filled(143, 2)}, {Key: "0520:1", Data: filled(128, 3)},
		{Key: "0501:2", Data: []byte{1, 0, 0, 1, 1, 0, 100, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1}}, {Key: "0501:3", Data: filled(64, 4)},
		{Key: "0520:2", Data: filled(143, 5)}, {Key: "0520:3", Data: filled(64, 6)},
	}, extra...)
}

// readOnly: карта не получила ни одной команды записи (INS D6), её файлы не изменились.
func readOnly(t *testing.T, m *cardtest.Card, before map[string][]byte) {
	t.Helper()
	for _, a := range m.Sent {
		if len(a) > 1 && a[1] == 0xD6 {
			t.Fatalf("чтение послало UPDATE BINARY: %X", a[:4])
		}
	}
	if len(m.Files) != len(before) {
		t.Fatal("набор EF карты изменился")
	}
	for k, v := range before {
		if !bytes.Equal(m.Files[k], v) {
			t.Fatalf("EF %s изменился", k)
		}
	}
}

func snapshot(m *cardtest.Card) map[string][]byte {
	c := map[string][]byte{}
	for k, v := range m.Files {
		c[k] = append([]byte{}, v...)
	}
	return c
}

// read — единственный вход тестов в Read: до и после проверяет «без записи» (нет D6, Files не изменились).
func read(t *testing.T, m *cardtest.Card, o Options) (Info, []byte, error) {
	t.Helper()
	before := snapshot(m)
	info, got, err := Read(m, o)
	readOnly(t, m, before)
	return info, got, err
}

func mustIntegrity(t *testing.T, m *cardtest.Card) {
	t.Helper()
	_, got, err := read(t, m, Options{})
	if !errors.Is(err, ErrIntegrity) || got != nil {
		t.Fatalf("ждали ErrIntegrity и nil, получили %v, %d байт", err, len(got))
	}
}

func TestCutEF(t *testing.T) {
	m := gen1Card()
	m.Cut = map[string]int{"0504:0": 100}
	mustIntegrity(t, m)
}

func TestExtraData(t *testing.T) {
	m := gen1Card()
	m.Extra = map[string]int{"0520:0": 1}
	mustIntegrity(t, m)
}

func TestSignatureLength(t *testing.T) {
	m := gen1Card()
	m.SigLen = map[string]int{"0520:0": 64}
	mustIntegrity(t, m)

	src := cardtest.Enc(gen2Blocks())
	g2 := cardtest.FromDDD(src)
	g2.SigLen = map[string]int{"0520:2": 96}
	if _, _, err := read(t, g2, Options{}); err != nil {
		t.Fatalf("подпись Gen2 в 96 байт допустима: %v", err)
	}
	g2 = cardtest.FromDDD(src)
	g2.SigLen = map[string]int{"0520:2": 70}
	mustIntegrity(t, g2)
}

func TestLe00(t *testing.T) {
	m := cardtest.FromDDD(cardtest.Enc(gen2Blocks()))
	m.SigLen = map[string]int{"0520:2": 96}
	_, got, err := read(t, m, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if sig := cardtest.FromDDD(got).Files["0520:3"]; len(sig) != 96 {
		t.Fatalf("подпись 0520:3: %d байт, ждали 96", len(sig))
	}
	le00 := false
	for _, a := range m.Sent {
		le00 = le00 || (a[1] == 0x2A && a[2] == 0x9E && a[4] == 0)
	}
	if !le00 {
		t.Fatal("PSO не посылался с Le=00")
	}
}

func bigCard() ([]byte, *cardtest.Card) {
	v2 := []byte{1, 1, 0, 0, byte(1650 >> 8), byte(1650 & 0xFF), 0, 0, 0, 0} // LoadUnloadRecords = 1650 → 2 + 1650×20 = 33 002
	src := cardtest.Enc(gen2Blocks(
		cardtest.Block{Key: "0525:2", Data: v2}, cardtest.Block{Key: "0525:3", Data: filled(64, 7)},
		cardtest.Block{Key: "0529:2", Data: filled(33002, 8)}, cardtest.Block{Key: "0529:3", Data: filled(64, 9)},
	))
	return src, cardtest.FromDDD(src)
}

func TestBigEF(t *testing.T) {
	src, m := bigCard()
	info, got, err := read(t, m, Options{})
	if err != nil || !bytes.Equal(got, src) || info.String() != "Gen2v2" {
		t.Fatalf("err=%v, равны=%v, поколение %s", err, bytes.Equal(got, src), info)
	}
	b1 := false
	for _, a := range m.Sent {
		b1 = b1 || a[1] == 0xB1
		if a[1] == 0xB0 && (int(a[2])<<8|int(a[3]))+int(a[4]) > 32768 { // Le=0 здесь не встречается
			t.Fatalf("B0 читает за байт 32 767: %X", a)
		}
	}
	if !b1 {
		t.Fatal("в Sent нет READ BINARY с INS B1")
	}
}

// 6Cxx в ответ на B1 — повтор с Le = xx.
func TestB1Retry6C(t *testing.T) {
	src, m := bigCard()
	m.B1Hint = true
	if _, got, err := read(t, m, Options{}); err != nil || !bytes.Equal(got, src) {
		t.Fatalf("err=%v, равны=%v", err, bytes.Equal(got, src))
	}
}

// Между хэшем и подписью — только чтение данных этого EF; проба конца — после PSO.
func TestEndProbeAfterSignature(t *testing.T) {
	m := gen1Card()
	if _, _, err := read(t, m, Options{}); err != nil {
		t.Fatal(err)
	}
	hex := func(a []byte) string { return fmt.Sprintf("%X", a) }
	for i, a := range m.Sent {
		if hex(a) != "00A4020C020520" || hex(m.Sent[i+1]) != "802A9000" { // первый SELECT 0520 — проба наличия в прологе
			continue
		}
		if hex(m.Sent[i+2]) != "00B000008F" || hex(m.Sent[i+3]) != "002A9E9A00" {
			t.Fatalf("порядок после SELECT 0520: %s %s %s", hex(m.Sent[i+1]), hex(m.Sent[i+2]), hex(m.Sent[i+3]))
		}
		for _, b := range m.Sent[i+4:] {
			if b[1] == 0xB0 {
				if hex(b) != "00B0008F01" {
					t.Fatalf("после PSO ждали пробу 00B0008F01, получили %s", hex(b))
				}
				return
			}
		}
		t.Fatal("пробы конца после PSO нет")
	}
	t.Fatal("SELECT 0520 не найден")
}

func TestRemovedMidway(t *testing.T) {
	m := gen1Card()
	m.RemoveAfter = 10
	_, got, err := read(t, m, Options{})
	if !errors.Is(err, cardtest.ErrRemoved) || got != nil {
		t.Fatalf("ждали ErrRemoved и nil, получили %v, %d байт", err, len(got))
	}
}

// Сертификаты Gen2: с заголовком DER — по его длине (51 байт), без него (204 нуля) — до конца, как отдала карта.
func TestProgress(t *testing.T) {
	der := append([]byte{0x7F, 0x21, 48}, filled(48, 1)...)
	blocks := gen2Blocks(cardtest.Block{Key: "c101:2", Data: der}, cardtest.Block{Key: "c109:2", Data: filled(204, 0)})
	src := cardtest.Enc(blocks)
	var calls [][2]int
	m := cardtest.FromDDD(src)
	_, got, err := read(t, m, Options{Progress: func(done, total int) { calls = append(calls, [2]int{done, total}) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) == 0 {
		t.Fatal("Progress не вызывался")
	}
	want := 0
	for _, b := range func() []cardtest.Block { bl, _ := cardtest.ParseDDD(got); return bl }() {
		if b.Key[5] == '0' || b.Key[5] == '2' {
			want += len(b.Data)
		}
	}
	if last := calls[len(calls)-1]; last[0] != last[1] || last[1] != want {
		t.Fatalf("последний вызов %v, сумма данных %d", last, want)
	}
	for i := 1; i < len(calls); i++ {
		if calls[i][0] < calls[i-1][0] {
			t.Fatal("done уменьшился")
		}
	}

	// заголовок DER говорит 51 байт, EF — 60
	bad := cardtest.FromDDD(cardtest.Enc(gen2Blocks(cardtest.Block{Key: "c101:2", Data: append(append([]byte{}, der...), filled(9, 1)...)})))
	mustIntegrity(t, bad)
}

// Синтетическая карта Gen1: работает без образцов, проверяет пропуск отсутствующих EF и путь 6700 без длины.
func TestSyntheticGen1(t *testing.T) {
	src := cardtest.Enc([]cardtest.Block{
		{Key: "0002:0", Data: filled(25, 1)}, {Key: "0005:0", Data: filled(8, 2)},
		{Key: "0501:0", Data: []byte{1, 0, 0, 3, 3, 0x35, 0xD0, 0, 3, 3}}, {Key: "0501:1", Data: filled(128, 4)},
		{Key: "c100:0", Data: filled(194, 5)}, {Key: "c108:0", Data: filled(194, 6)},
		{Key: "0520:0", Data: filled(143, 7)}, {Key: "0520:1", Data: filled(128, 8)},
		{Key: "0504:0", Data: filled(13780, 9)}, {Key: "0504:1", Data: filled(128, 10)},
	})
	for _, noHint := range []bool{false, true} {
		m := cardtest.FromDDD(src)
		m.NoHint = noHint
		info, got, err := read(t, m, Options{})
		if err != nil || !bytes.Equal(got, src) || info.String() != "Gen1" || len(m.Bad) > 0 {
			t.Fatalf("noHint=%v: err=%v, равны=%v, поколение %s, лишние команды %v", noHint, err, bytes.Equal(got, src), info, m.Bad)
		}
	}
}

func TestSyntheticWorkshop(t *testing.T) {
	m := cardtest.FromDDD(cardtest.Enc([]cardtest.Block{{Key: "0501:0", Data: append([]byte{2, 0, 0}, filled(8, 0)...)}, {Key: "0520:0", Data: filled(10, 1)}}))
	if _, _, err := read(t, m, Options{}); !errors.Is(err, ErrNotDriver) || len(m.Bad) > 0 {
		t.Fatalf("ждали ErrNotDriver, получили %v; лишние команды %v", err, m.Bad)
	}
	if _, _, err := read(t, cardtest.FromDDD(nil), Options{}); !errors.Is(err, ErrNotTacho) {
		t.Fatalf("пустая карта: ждали ErrNotTacho, получили %v", err)
	}
}

// Настоящие файлы карт: имитация отдаёт их блоки, выгрузка должна вернуть тот же файл побайтно.
// Имена файлов и данные в вывод не попадают — только порядковые номера (персональные данные).
func TestSamples(t *testing.T) {
	envs := map[string]string{}
	for _, v := range []string{"KTS_DEMO_DIR", "KTS_KZ_SAMPLES_DIR"} {
		if d := os.Getenv(v); d != "" {
			envs[v] = d
		}
	}
	if len(envs) == 0 {
		if os.Getenv("KTS_REQUIRE_SAMPLES") == "1" {
			t.Fatal("KTS_REQUIRE_SAMPLES=1, но KTS_DEMO_DIR и KTS_KZ_SAMPLES_DIR пусты")
		}
		t.Skip("образцы не заданы: укажите KTS_DEMO_DIR и/или KTS_KZ_SAMPLES_DIR — каталоги с файлами .ddd карт")
	}
	outDir := os.Getenv("KTS_OUT_DIR") // сюда кладём по одному прочитанному файлу на поколение для проверки разборщиком
	stats := map[string]int{}
	n := 0
	for _, dir := range envs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			blocks, ok := cardtest.ParseDDD(src)
			if !ok {
				return nil
			}
			m := cardtest.FromDDD(src)
			if m.Files["0501:0"] == nil {
				return nil
			}
			n++
			m.NoHint = n%5 == 0
			// часть программ хранит 050E без подписи; настоящая карта подпишет любой EF 05xx — имитации даём заглушку
			for k := range m.Files {
				if sigKey := k[:5] + map[byte]string{'0': "1", '2': "3"}[k[5]]; k[:2] == "05" && len(sigKey) == 6 && m.Files[sigKey] == nil {
					m.Files[sigKey] = filled(128, 0)
				}
			}
			var last [2]int
			info, got, err := read(t, m, Options{Progress: func(done, total int) { last = [2]int{done, total} }})
			if len(m.Bad) > 0 {
				t.Errorf("#%d: лишние команды %v", n, m.Bad)
			}
			if m.Files["0501:0"][0] != 1 {
				if !errors.Is(err, ErrNotDriver) {
					t.Errorf("#%d: карта не водителя, ждали ErrNotDriver, получили %v", n, err)
				}
				stats["не водитель → «Это не карта водителя»"]++
				return nil
			}
			if err != nil {
				t.Errorf("#%d: %v", n, err)
				return nil
			}
			if last[0] != last[1] || last[1] == 0 {
				t.Errorf("#%d: последний Progress %v", n, last)
			}
			want := "Gen1"
			if m.Files["0525:2"] != nil {
				want = "Gen2v2"
			} else if m.Files["0501:2"] != nil {
				want = "Gen2"
			}
			if info.String() != want {
				t.Errorf("#%d: поколение %s, в файле %s", n, info, want)
			}
			if bytes.Equal(got, src) {
				stats[want+": побайтно равен"]++
				if outDir != "" {
					if _, err := os.Stat(filepath.Join(outDir, want+".ddd")); err != nil {
						return os.WriteFile(filepath.Join(outDir, want+".ddd"), got, 0o600)
					}
				}
				return nil
			}
			// другая раскладка (C100/C108 перед 0501, 050E без подписи): каждый блок исходника должен совпасть
			gotFiles := cardtest.FromDDD(got).Files
			for _, b := range blocks {
				if !bytes.Equal(gotFiles[b.Key], b.Data) {
					t.Errorf("#%d: блок %s отличается от исходного", n, b.Key)
				}
			}
			stats[want+": другая раскладка, блоки равны"]++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	keys := make([]string, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("%-45s %d", k, stats[k])
	}
	if len(envs) == 2 {
		byteEq, blocksEq := 0, 0
		for k, v := range stats {
			if strings.HasSuffix(k, "побайтно равен") {
				byteEq += v
			} else if strings.HasSuffix(k, "блоки равны") {
				blocksEq += v
			}
		}
		if byteEq != 195 || blocksEq != 6 || stats["не водитель → «Это не карта водителя»"] != 3 {
			t.Errorf("ждали 195 побайтно, 6 по блокам, 3 мастерских; получили %d, %d, %d", byteEq, blocksEq, stats["не водитель → «Это не карта водителя»"])
		}
	}
	if envs["KTS_KZ_SAMPLES_DIR"] != "" && stats["Gen1: побайтно равен"] == 0 {
		t.Error("нет ни одной карты Gen1, прочитанной побайтно")
	}
	if envs["KTS_DEMO_DIR"] != "" && stats["Gen2v2: побайтно равен"] == 0 {
		t.Error("нет ни одной карты Gen2v2, прочитанной побайтно")
	}
}

func TestCompleteDoneTotal(t *testing.T) {
	s := &session{done: 10, total: 10}
	if err := s.complete(); err != nil {
		t.Fatal(err)
	}
	s.total = 11
	if err := s.complete(); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("ждали ErrIntegrity, получили %v", err)
	}
}
