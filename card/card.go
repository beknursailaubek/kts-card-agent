// Package card выгружает карту водителя тахографа в файл .ddd (Annex 1C, Appendix 2 и 7).
// Раскладка TLV повторяет образцы: MF → DF Tachograph → DF Tachograph_G2 (+ Gen2v2), 050E с подписью,
// 0002/0005 повторены с тегом Gen2. Read на карту ничего не пишет; LastCardDownload в 050E обновляет только WriteDownloadDate.
package card

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// Card — канал APDU к карте: на Windows это *scard.Card, в тестах — имитация.
type Card interface {
	Transmit(apdu []byte) ([]byte, error)
}

var (
	ErrNotTacho  = errors.New("Это не карта тахографа")
	ErrNotDriver = errors.New("Это не карта водителя")
	// ErrIntegrity — карта отдала EF не целиком, с лишними данными или с подписью недопустимой длины: файл не собирается.
	ErrIntegrity = errors.New("Карта прочитана не полностью")
)

// SWError — карта отклонила команду.
type SWError struct {
	Cmd []byte
	SW  uint16
}

func (e *SWError) Error() string {
	return fmt.Sprintf("карта отклонила команду %X (SW %04X)", e.Cmd[:4], e.SW)
}

// Info — тип и поколение карты; Type — EquipmentType из EF Application_Identification (1 — водитель).
type Info struct {
	Generation int // 1 или 2
	V2         bool
	Type       byte
}

func (i Info) String() string {
	switch {
	case i.Generation == 1:
		return "Gen1"
	case i.V2:
		return "Gen2v2"
	}
	return "Gen2"
}

// Options — необязательные обратные вызовы. Trace получает команду и SW, но не данные ответа (в них персональные данные).
type Options struct {
	Trace    func(cmd []byte, sw uint16)
	OnDetect func(Info)
	Progress func(done, total int) // байты данных EF (подписи не считаются); у сертификатов Gen2 total уточняется после чтения
}

var (
	aidG1 = []byte{0xFF, 0x54, 0x41, 0x43, 0x48, 0x4F}
	aidG2 = []byte{0xFF, 0x53, 0x4D, 0x52, 0x44, 0x54}
	efsMF = []uint16{0x0002, 0x0005}
	// порядок EF — как в образцах (он же порядок таблиц TCS_148/152); 0525–0530 — Gen2v2, на Gen2v1 их нет (6A82 → пропуск)
	efsG1 = []uint16{0x0501, 0xC100, 0xC108, 0x0520, 0x050E, 0x0521, 0x0502, 0x0503, 0x0504, 0x0505, 0x0506, 0x0507, 0x0508, 0x0522}
	efsG2 = []uint16{0x0501, 0xC100, 0xC101, 0xC108, 0xC109, 0x0520, 0x050E, 0x0521, 0x0502, 0x0503, 0x0504, 0x0505, 0x0506, 0x0507, 0x0508, 0x0522,
		0x0523, 0x0524, 0x0525, 0x0526, 0x0527, 0x0528, 0x0529, 0x0530}
)

const (
	swOK          = 0x9000
	swNotFound    = 0x6A82
	readChunk     = 0xFF
	maxEvenOffset = 0x7FFF // дальше — READ BINARY с нечётным INS (B1)
	maxOddChunk   = 0xF0   // ответ B1 — тег 53 + длина + данные, всё в пределах 255 байт
	certG2Size    = 204    // оценка длины сертификата Gen2 для total, пока заголовок не прочитан
)

type session struct {
	c           Card
	o           Options
	out         bytes.Buffer
	done, total int
	last        []byte // последняя команда чтения — для SWError
}

// Read определяет карту и выгружает её. Карта должна быть только что сброшена (текущий DF — MF, TCS_18),
// а вызов — идти внутри SCardBeginTransaction: выбор DF другой программой стирает хэш (TCS_121).
// Для карты не водителя возвращает ErrNotDriver, прочитав только EF MF и первые 3 байта 0501.
func Read(c Card, o Options) (Info, []byte, error) {
	s := &session{c: c, o: o}
	// EF MF — до выбора приложения: SELECT MF в стандарте нет, вернуться в MF можно только сбросом
	mf := map[uint16][]byte{}
	for _, fid := range efsMF {
		sw, err := s.selectEF(fid)
		if err != nil {
			return Info{}, nil, err
		}
		if sw == swNotFound { // EF ICC/IC необязательны (DDP_035)
			continue
		}
		if sw != swOK {
			return Info{}, nil, &SWError{selectCmd(fid), sw}
		}
		size, _ := ExpectedSize(fid, 0, AppIdent{}, nil)
		if mf[fid], err = s.readEF(fid, size); err != nil {
			return Info{}, nil, err
		}
	}

	info, err := s.detect()
	if err != nil {
		return info, nil, err
	}
	if o.OnDetect != nil {
		o.OnDetect(info)
	}

	pl, err := s.prologue(info)
	if err != nil {
		return info, nil, err
	}
	copies := info.Generation // 0002/0005 повторяются с тегом Gen2
	for _, d := range mf {
		s.total += len(d) * copies
	}
	for _, g := range []struct {
		fids []uint16
		tag  byte
	}{{efsG1, 0}, {efsG2, 2}} {
		if g.tag == 2 && info.Generation != 2 {
			break
		}
		for _, fid := range g.fids {
			if pl.has[g.tag][fid] {
				n, _, err := sizeOf(fid, g.tag, pl.ai[g.tag])
				if err != nil {
					return info, nil, err
				}
				s.total += n
			}
		}
	}
	putMF := func(tag byte) {
		for _, fid := range efsMF {
			if d, ok := mf[fid]; ok {
				s.put(fid, tag, d)
			}
		}
	}
	putMF(0)
	if err := s.app(aidG1, efsG1, 0, pl.ai[0]); err != nil {
		return info, nil, err
	}
	if info.Generation == 2 {
		putMF(2) // повтор 0002/0005 с тегом Gen2 — так во всех образцах Gen2, стандарт этого не требует
		if err := s.app(aidG2, efsG2, 2, pl.ai[2]); err != nil {
			return info, nil, err
		}
	}
	if err := s.complete(); err != nil {
		return info, nil, err
	}
	return info, s.out.Bytes(), nil
}

// complete — итог чтения: прочитанные байты данных обязаны совпасть с ожидаемым total (спека §8).
func (s *session) complete() error {
	if s.done != s.total {
		return fmt.Errorf("%w: прочитано %d байт из ожидаемых %d", ErrIntegrity, s.done, s.total)
	}
	return nil
}

// plan — размеры структур (0501/0525) и список имеющихся EF каждого приложения: ключ — тег данных (0 или 2).
type plan struct {
	ai  map[byte]AppIdent
	has map[byte]map[uint16]bool
}

// prologue читает 0501 (и 0525) обоих приложений целиком и проверяет, какие EF есть на карте; PERFORM HASH не посылает.
func (s *session) prologue(info Info) (*plan, error) {
	pl := &plan{ai: map[byte]AppIdent{}, has: map[byte]map[uint16]bool{}}
	apps := []struct {
		aid  []byte
		tag  byte
		fids []uint16
	}{{aidG1, 0, efsG1}, {aidG2, 2, efsG2}}
	for _, a := range apps {
		if a.tag == 2 && info.Generation != 2 {
			break
		}
		if sw, err := s.selectAID(a.aid); err != nil {
			return nil, err
		} else if sw != swOK {
			return nil, &SWError{selectAIDCmd(a.aid), sw}
		}
		pl.has[a.tag] = map[uint16]bool{}
		for _, fid := range a.fids {
			sw, err := s.selectEF(fid)
			if err != nil {
				return nil, err
			}
			if sw != swOK && sw != swNotFound {
				return nil, &SWError{selectCmd(fid), sw}
			}
			pl.has[a.tag][fid] = sw == swOK
		}
		var b0501, b0525 []byte
		for _, fid := range []uint16{0x0501, 0x0525} {
			if !pl.has[a.tag][fid] || (fid == 0x0525 && a.tag != 2) {
				continue
			}
			if sw, err := s.selectEF(fid); err != nil || sw != swOK {
				return nil, errors.Join(err, fmt.Errorf("EF %04X: SELECT вернул %04X", fid, sw))
			}
			size, _ := ExpectedSize(fid, a.tag, AppIdent{}, nil)
			d, err := s.readEF(fid, size)
			if err != nil {
				return nil, err
			}
			if fid == 0x0501 {
				b0501 = d
			} else {
				b0525 = d
			}
		}
		if b0501 == nil {
			return nil, fmt.Errorf("на карте нет обязательного EF 0501")
		}
		ai, err := ParseAppIdent(int(a.tag/2)+1, b0501, b0525)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrIntegrity, err)
		}
		pl.ai[a.tag] = ai
	}
	return pl, nil
}

// sizeOf — ожидаемый размер EF; сертификат Gen2 без заголовка DER заранее неизвестен: known=false, оценка 204.
func sizeOf(fid uint16, tag byte, ai AppIdent) (n int, known bool, err error) {
	if n, ok := ExpectedSize(fid, tag, ai, nil); ok {
		return n, true, nil
	}
	if tag == 2 && (fid == 0xC100 || fid == 0xC101 || fid == 0xC108 || fid == 0xC109) {
		return certG2Size, false, nil
	}
	return 0, false, fmt.Errorf("нет формулы размера EF %04X", fid)
}

// Detect определяет тип и поколение карты без персональных данных: SELECT AID, первые 3 байта 0501, у Gen2 — SELECT 0525.
// Для карты не водителя возвращает Info с Type и ErrNotDriver.
func Detect(c Card) (Info, error) {
	s := &session{c: c}
	return s.detect()
}

func (s *session) detect() (Info, error) {
	info := Info{Generation: 2}
	sw, err := s.selectAID(aidG2)
	if err != nil {
		return info, err
	}
	if sw != swOK {
		info.Generation = 1
		if sw, err = s.selectAID(aidG1); err != nil {
			return info, err
		}
		if sw != swOK {
			return info, ErrNotTacho
		}
	}
	if sw, err = s.selectEF(0x0501); err != nil {
		return info, err
	}
	if sw != swOK {
		return info, ErrNotTacho
	}
	cmd := []byte{0x00, 0xB0, 0x00, 0x00, 0x03}
	data, sw, err := s.tx(cmd)
	if err != nil {
		return info, err
	}
	if sw != swOK || len(data) == 0 {
		return info, &SWError{cmd, sw}
	}
	info.Type = data[0]
	if info.Type != 1 {
		return info, ErrNotDriver
	}
	if info.Generation == 2 {
		if sw, err = s.selectEF(0x0525); err != nil {
			return info, err
		}
		info.V2 = sw == swOK
	}
	return info, nil
}

// app выгружает одно приложение целиком, не переключая DF: хэш живёт до выбора другого DF (TCS_121).
func (s *session) app(aid []byte, fids []uint16, tag byte, ai AppIdent) error {
	sw, err := s.selectAID(aid)
	if err != nil {
		return err
	}
	if sw != swOK {
		return &SWError{selectAIDCmd(aid), sw}
	}
	for _, fid := range fids {
		sw, err := s.selectEF(fid)
		if err != nil {
			return err
		}
		if sw == swNotFound {
			if fid == 0x0501 || fid == 0x0520 { // без них разборщик файл не примет
				return fmt.Errorf("на карте нет обязательного EF %04X", fid)
			}
			continue
		}
		if sw != swOK {
			return &SWError{selectCmd(fid), sw}
		}
		size, known, err := sizeOf(fid, tag, ai)
		if err != nil {
			return err
		}
		signed := fid>>8 == 0x05 // подписываются все EF приложения, кроме сертификатов C1xx (DDP_035)
		if signed {
			if err := s.expect([]byte{0x80, 0x2A, 0x90, 0x00}); err != nil { // PERFORM HASH OF FILE
				return err
			}
		}
		if !known {
			size = -1 // сертификат Gen2 без заголовка DER (нет связующего сертификата): до маркера конца карты
		}
		data, err := s.readData(fid, size)
		if err != nil {
			return err
		}
		if !known {
			if n, ok := ExpectedSize(fid, tag, ai, data); ok && n != len(data) {
				return fmt.Errorf("%w: EF %04X: длина %d, по заголовку DER %d", ErrIntegrity, fid, len(data), n)
			}
			s.total += len(data) - certG2Size
		}
		s.put(fid, tag, data)
		if signed {
			cmd := []byte{0x00, 0x2A, 0x9E, 0x9A, 0x00} // PSO: COMPUTE DIGITAL SIGNATURE; Le=00, точную длину карта даст через 6Cxx
			sig, sw, err := s.tx(cmd)
			if err != nil {
				return err
			}
			if sw != swOK {
				return &SWError{cmd, sw}
			}
			if !sigLenOK(tag, len(sig)) {
				return fmt.Errorf("%w: подпись EF %04X длиной %d", ErrIntegrity, fid, len(sig))
			}
			s.put(fid, tag+1, sig)
		}
		if size >= 0 {
			if err := s.checkEnd(fid, size); err != nil {
				return err
			}
		}
	}
	return nil
}

// sigLenOK — Gen1: RSA-1024, 128 байт; Gen2: ECDSA 256/384/512/521 бит, 64, 96, 128 или 132 байта.
func sigLenOK(tag byte, n int) bool {
	if tag == 0 {
		return n == 128
	}
	return n == 64 || n == 96 || n == 128 || n == 132
}

// readAt читает n байт с позиции off: до 32 767 — READ BINARY (B0), дальше — с нечётным INS (B1, TCS_51): данные в теге 53.
func (s *session) readAt(off, n int) ([]byte, uint16, error) {
	if off <= maxEvenOffset {
		s.last = []byte{0x00, 0xB0, byte(off >> 8), byte(off), byte(n)}
		return s.tx(s.last)
	}
	le := n + 2 // Le — длина всего ответа: тег 53, длина (1 или 2 байта), данные
	if n >= 128 {
		le++
	}
	s.last = []byte{0x00, 0xB1, 0x00, 0x00, 0x04, 0x54, 0x02, byte(off >> 8), byte(off), byte(le)}
	data, sw, err := s.tx(s.last)
	if err != nil || sw != swOK {
		return data, sw, err
	}
	if len(data) < 2 || data[0] != 0x53 {
		return nil, 0, fmt.Errorf("%w: ответ B1 без тега 53", ErrIntegrity)
	}
	l, hdr := int(data[1]), 2
	if l == 0x81 && len(data) >= 3 {
		l, hdr = int(data[2]), 3
	}
	if l != len(data)-hdr {
		return nil, 0, fmt.Errorf("%w: длина данных B1 %d, ответ %d", ErrIntegrity, l, len(data)-hdr)
	}
	return data[hdr:], sw, nil
}

func clampChunk(off, n int) int {
	if off > maxEvenOffset && n > maxOddChunk {
		return maxOddChunk
	}
	if off <= maxEvenOffset && off+n > maxEvenOffset+1 { // порция B0 не заходит за 32 767-й байт, дальше — B1
		return maxEvenOffset + 1 - off
	}
	return n
}

// readEF читает выбранный EF. size >= 0 — ровно столько байт: конец раньше или данные сверх — ErrIntegrity.
// size < 0 — до маркера конца карты (6B00/6Cxx): только для сертификатов Gen2 без заголовка DER.
func (s *session) readEF(fid uint16, size int) ([]byte, error) {
	out, err := s.readData(fid, size)
	if err == nil && size >= 0 {
		err = s.checkEnd(fid, size)
	}
	return out, err
}

func (s *session) readData(fid uint16, size int) ([]byte, error) {
	if size < 0 {
		return s.readToEnd()
	}
	out := make([]byte, 0, size)
	for len(out) < size {
		n := clampChunk(len(out), min(size-len(out), readChunk))
		data, sw, err := s.readAt(len(out), n)
		if err != nil {
			return nil, err
		}
		switch {
		case sw == swOK && len(data) == n:
			out = append(out, data...)
		case sw == swOK || sw == 0x6B00 || sw == 0x6700 || sw == 0x6282:
			return nil, fmt.Errorf("%w: EF %04X закончился на %d байтах из %d", ErrIntegrity, fid, len(out)+len(data), size)
		default:
			return nil, &SWError{s.last, sw}
		}
	}
	return out, nil
}

// checkEnd — за ожидаемым концом должен быть конец файла. У подписанного EF вызывается после PSO: между хэшем и подписью только чтение данных.
func (s *session) checkEnd(fid uint16, size int) error {
	data, sw, err := s.readAt(size, 1)
	if err != nil {
		return err
	}
	switch {
	case sw == swOK && len(data) > 0:
		return fmt.Errorf("%w: EF %04X длиннее ожидаемых %d байт", ErrIntegrity, fid, size)
	case sw == swOK || sw == 0x6B00 || sw == 0x6700 || sw == 0x6A86:
		return nil
	}
	return &SWError{s.last, sw}
}

// readToEnd читает порциями до конца: размер SELECT не сообщает (P2=0C), конец — 6B00, 6Cxx или короткий ответ.
func (s *session) readToEnd() ([]byte, error) {
	var out []byte
	le := readChunk
	for {
		n := clampChunk(len(out), le)
		data, sw, err := s.readAt(len(out), n)
		if err != nil {
			return nil, err
		}
		switch {
		case sw == swOK:
			out = append(out, data...)
			if len(data) < n {
				return out, nil
			}
		case sw == 0x6B00: // смещение за концом файла
			return out, nil
		case sw == 0x6282: // конец файла раньше Le
			return append(out, data...), nil
		case sw == 0x6700 && le > 1: // Offset+Le за концом, а точную длину карта не сообщила — уменьшаем порцию
			le /= 2
		case sw == 0x6700:
			return out, nil
		default:
			return nil, &SWError{s.last, sw}
		}
	}
}

// tx отправляет APDU и разбирает SW: 6Cxx — повтор с Le=xx, 61xx — GET RESPONSE.
func (s *session) tx(cmd []byte) ([]byte, uint16, error) {
	data, sw, err := s.raw(cmd)
	if err != nil {
		return nil, 0, err
	}
	if sw>>8 == 0x6C && (len(cmd) == 5 || cmd[1] == 0xB1) { // у B1 Le — последний байт после данных
		retry := append(append([]byte{}, cmd[:len(cmd)-1]...), byte(sw))
		if data, sw, err = s.raw(retry); err != nil {
			return nil, 0, err
		}
	}
	for sw>>8 == 0x61 {
		var more []byte
		if more, sw, err = s.raw([]byte{0x00, 0xC0, 0x00, 0x00, byte(sw)}); err != nil {
			return nil, 0, err
		}
		data = append(data, more...)
	}
	return data, sw, nil
}

func (s *session) raw(cmd []byte) ([]byte, uint16, error) {
	rsp, err := s.c.Transmit(cmd)
	if err != nil {
		return nil, 0, err
	}
	if len(rsp) < 2 {
		return nil, 0, fmt.Errorf("короткий ответ карты (%d байт) на команду %X", len(rsp), cmd[:4])
	}
	n := len(rsp) - 2
	sw := binary.BigEndian.Uint16(rsp[n:])
	if s.o.Trace != nil {
		s.o.Trace(cmd, sw)
	}
	return rsp[:n], sw, nil
}

func (s *session) expect(cmd []byte) error {
	_, sw, err := s.tx(cmd)
	if err == nil && sw != swOK {
		err = &SWError{cmd, sw}
	}
	return err
}

func (s *session) selectEF(fid uint16) (uint16, error) {
	_, sw, err := s.tx(selectCmd(fid))
	return sw, err
}

func (s *session) selectAID(aid []byte) (uint16, error) {
	_, sw, err := s.tx(selectAIDCmd(aid))
	return sw, err
}

func selectCmd(fid uint16) []byte {
	return []byte{0x00, 0xA4, 0x02, 0x0C, 0x02, byte(fid >> 8), byte(fid)}
}

func selectAIDCmd(aid []byte) []byte {
	return append([]byte{0x00, 0xA4, 0x04, 0x0C, byte(len(aid))}, aid...)
}

// put дописывает TLV: FID (2 байта) + тег приложения (1) + длина (2, big-endian) + данные (DDP_040–046).
// Данные (чётный тег) идут в прогресс, подписи — нет.
func (s *session) put(fid uint16, tag byte, data []byte) {
	s.out.Write([]byte{byte(fid >> 8), byte(fid), tag, byte(len(data) >> 8), byte(len(data))})
	s.out.Write(data)
	if tag%2 == 0 {
		s.done += len(data)
		if s.o.Progress != nil {
			s.o.Progress(s.done, s.total)
		}
	}
}
