// Package cardtest — имитация карты тахографа по блокам настоящего .ddd для тестов.
package cardtest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
)

var (
	aidG1 = []byte{0xFF, 0x54, 0x41, 0x43, 0x48, 0x4F}
	aidG2 = []byte{0xFF, 0x53, 0x4D, 0x52, 0x44, 0x54}
)

// Block — TLV файла .ddd: ключ "<fid hex>:<тег>".
type Block struct {
	Key  string
	Data []byte
}

// ParseDDD разбирает файл карты на TLV; ok=false — не файл карты (ТС, обрезан, мусор).
func ParseDDD(b []byte) (blocks []Block, ok bool) {
	for len(b) > 0 {
		if len(b) < 5 || b[2] > 3 {
			return nil, false
		}
		n := int(binary.BigEndian.Uint16(b[3:]))
		if len(b) < 5+n {
			return nil, false
		}
		blocks = append(blocks, Block{fmt.Sprintf("%04x:%d", binary.BigEndian.Uint16(b), b[2]), b[5 : 5+n]})
		b = b[5+n:]
	}
	return blocks, len(blocks) > 0
}

// ErrRemoved — карту вынули: ответ имитации на любую команду после Card.RemoveAfter.
var ErrRemoved = errors.New("cardtest: карта извлечена")

// Card отвечает на команды выгрузки по блокам настоящего .ddd, как карта по Appendix 2.
type Card struct {
	Files   map[string][]byte // первый блок с ключом, как в разборщике
	df      byte              // 0 — MF, 1 — DF Tachograph, 2 — DF Tachograph_G2
	ef      string            // ключ выбранного EF
	hashed  string            // EF, для которого посчитан хэш
	pending []byte            // данные для GET RESPONSE
	NoHint  bool              // на Offset+Le за концом отвечать 6700 без точной длины
	Sent    [][]byte          // все принятые APDU
	Bad     []string          // неизвестные имитации команды (запись D6 исполняется; её отсутствие при чтении проверяет Sent)

	Cut         map[string]int // EF отдаётся до N байт, дальше 6B00
	Extra       map[string]int // к EF дописано N лишних байт
	SigLen      map[string]int // подпись EF (ключ — EF данных) другой длины
	RemoveAfter int            // после N-й команды все ответы — ErrRemoved
	B1Hint      bool           // на первый B1 ответить 6Cxx с точной длиной ответа
	b1Hinted    bool
	FailWrite   int // на N-ю команду UPDATE BINARY (D6) ответить 6581, Files не менять
	writes      int
}

// file — содержимое EF с учётом Cut и Extra.
func (m *Card) file(key string) []byte {
	f := m.Files[key]
	if n, ok := m.Cut[key]; ok && n < len(f) {
		f = f[:n]
	}
	if n := m.Extra[key]; n > 0 {
		f = append(append([]byte{}, f...), bytes.Repeat([]byte{0xEE}, n)...)
	}
	return f
}

// FromDDD — карта из файла .ddd (не разобрался — пустая карта). Sent копит все принятые команды.
func FromDDD(ddd []byte) *Card {
	blocks, _ := ParseDDD(ddd)
	m := &Card{Files: map[string][]byte{}}
	for _, b := range blocks {
		if _, dup := m.Files[b.Key]; !dup {
			m.Files[b.Key] = b.Data
		}
	}
	return m
}

func sw(v uint16, data ...byte) []byte { return append(append([]byte{}, data...), byte(v>>8), byte(v)) }

func (m *Card) Transmit(a []byte) ([]byte, error) {
	m.Sent = append(m.Sent, append([]byte{}, a...))
	if m.RemoveAfter > 0 && len(m.Sent) > m.RemoveAfter {
		return nil, ErrRemoved
	}
	if len(a) < 4 {
		return sw(0x6700), nil
	}
	hdr := binary.BigEndian.Uint32(a)
	le := func() int {
		if len(a) != 5 {
			return -1
		}
		if a[4] == 0 {
			return 256
		}
		return int(a[4])
	}
	switch {
	case hdr == 0x00A4040C && len(a) == 11: // SELECT AID
		m.ef, m.hashed = "", ""
		switch {
		case bytes.Equal(a[5:], aidG1) && m.Files["0501:0"] != nil:
			m.df = 1
		case bytes.Equal(a[5:], aidG2) && m.Files["0501:2"] != nil:
			m.df = 2
		default:
			return sw(0x6A82), nil
		}
		return sw(0x9000), nil

	case hdr == 0x00A4020C && len(a) == 7: // SELECT EF под текущим DF
		fid := binary.BigEndian.Uint16(a[5:])
		inMF := fid == 0x0002 || fid == 0x0005
		tag := map[byte]byte{0: 0, 1: 0, 2: 2}[m.df]
		key := fmt.Sprintf("%04x:%d", fid, tag)
		if (m.df == 0) != inMF || m.Files[key] == nil {
			return sw(0x6A82), nil
		}
		m.ef = key
		return sw(0x9000), nil

	case a[0] == 0x00 && a[1] == 0xB0 && a[2]&0x80 == 0: // READ BINARY по смещению
		if m.ef == "" || le() < 0 {
			return sw(0x6986), nil
		}
		f := m.file(m.ef)
		off := int(a[2])<<8 | int(a[3])
		switch {
		case off >= len(f):
			return sw(0x6B00), nil
		case off+le() > len(f) && m.NoHint:
			return sw(0x6700), nil
		case off+le() > len(f):
			return sw(0x6C00 | uint16(len(f)-off)&0xFF), nil
		}
		return sw(0x9000, f[off:off+le()]...), nil

	case a[0] == 0x00 && a[1] == 0xB1 && len(a) == 10 && a[4] == 4 && a[5] == 0x54 && a[6] == 2: // READ BINARY с нечётным INS: данные в теге 53
		if m.ef == "" {
			return sw(0x6986), nil
		}
		f := m.file(m.ef)
		off, le := int(a[7])<<8|int(a[8]), int(a[9])
		if off >= len(f) {
			return sw(0x6B00), nil
		}
		hdr, n := []byte{0x53}, le-2
		if n >= 128 {
			hdr, n = []byte{0x53, 0x81}, le-3
		}
		n = min(n, len(f)-off)
		if m.B1Hint && !m.b1Hinted {
			m.b1Hinted = true
			return sw(0x6C00 | uint16(len(hdr)+1+n)), nil
		}
		return sw(0x9000, append(append(hdr, byte(n)), f[off:off+n]...)...), nil

	case a[0] == 0x00 && a[1] == 0xD6 && a[2]&0x80 == 0 && len(a) > 5 && len(a) == 5+int(a[4]): // UPDATE BINARY по смещению
		m.writes++
		if m.ef == "" {
			return sw(0x6986), nil
		}
		if m.writes == m.FailWrite {
			return sw(0x6581), nil
		}
		f, off := m.Files[m.ef], int(a[2])<<8|int(a[3])
		if off+len(a[5:]) > len(f) {
			return sw(0x6B00), nil
		}
		m.Files[m.ef] = append(append(append([]byte{}, f[:off]...), a[5:]...), f[off+len(a[5:]):]...)
		return sw(0x9000), nil

	case hdr == 0x802A9000 && len(a) == 4: // PERFORM HASH OF FILE
		if m.ef == "" {
			return sw(0x6986), nil
		}
		m.hashed = m.ef
		return sw(0x9000), nil

	case hdr == 0x002A9E9A: // PSO: COMPUTE DIGITAL SIGNATURE — подпись последнего выбранного EF
		if m.hashed == "" || m.hashed != m.ef {
			return sw(0x6985), nil
		}
		sig := m.Files[m.ef[:5]+map[byte]string{'0': "1", '2': "3"}[m.ef[5]]] // подпись данных :0 — :1, данных :2 — :3
		if sig == nil {
			return sw(0x6A88), nil
		}
		if n, ok := m.SigLen[m.ef]; ok {
			sig = append(append([]byte{}, sig...), bytes.Repeat([]byte{7}, n)...)[:n]
		}
		if le() != len(sig) { // как T=0: сначала точная длина, потом данные через GET RESPONSE
			return sw(0x6C00 | uint16(len(sig))&0xFF), nil
		}
		m.pending = sig
		return sw(0x6100 | uint16(len(sig))&0xFF), nil

	case hdr == 0x00C00000 && m.pending != nil: // GET RESPONSE
		p := m.pending
		m.pending = nil
		return sw(0x9000, p...), nil
	}
	m.Bad = append(m.Bad, fmt.Sprintf("%X", a[:4]))
	return sw(0x6D00), nil
}

// Enc собирает файл .ddd из блоков.
func Enc(blocks []Block) []byte {
	var b bytes.Buffer
	for _, bl := range blocks {
		fid, _ := strconv.ParseUint(bl.Key[:4], 16, 16)
		b.Write([]byte{byte(fid >> 8), byte(fid), bl.Key[5] - '0', byte(len(bl.Data) >> 8), byte(len(bl.Data))})
		b.Write(bl.Data)
	}
	return b.Bytes()
}
