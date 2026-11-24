package card

import (
	"encoding/binary"
	"fmt"
)

// AppIdent — размеры структур карты из EF Application_Identification (0501, у Gen2v2 ещё 0525).
type AppIdent struct {
	Gen                                                                                 int
	EventsPerType, FaultsPerType, ActivityLength, VehicleRecords, PlaceRecords          int
	GNSSRecords, ConditionRecords, VehicleUnitRecords, BorderRecords, LoadUnloadRecords int
}

// ParseAppIdent разбирает 0501 (10 байт у Gen1, 17 у Gen2) и 0525 (10 байт, nil — нет); раскладка — packages/tacho/src/card/identity.ts.
func ParseAppIdent(gen int, b0501, b0525 []byte) (AppIdent, error) {
	ai := AppIdent{Gen: gen}
	u16 := func(b []byte, i int) int { return int(binary.BigEndian.Uint16(b[i:])) }
	switch {
	case gen == 1 && len(b0501) == 10:
		ai.PlaceRecords = int(b0501[9])
	case gen == 2 && len(b0501) == 17:
		ai.PlaceRecords, ai.GNSSRecords, ai.ConditionRecords, ai.VehicleUnitRecords = u16(b0501, 9), u16(b0501, 11), u16(b0501, 13), u16(b0501, 15)
	default:
		return ai, fmt.Errorf("0501: размер %d не подходит для Gen%d", len(b0501), gen)
	}
	ai.EventsPerType, ai.FaultsPerType, ai.ActivityLength, ai.VehicleRecords = int(b0501[3]), int(b0501[4]), u16(b0501, 5), u16(b0501, 7)
	if b0525 != nil {
		if len(b0525) != 10 {
			return ai, fmt.Errorf("0525: размер %d, ждали 10", len(b0525))
		}
		ai.BorderRecords, ai.LoadUnloadRecords = u16(b0525, 2), u16(b0525, 4)
	}
	return ai, nil
}

// ExpectedSize — размер данных EF по формулам Appendix 2; tag 0 — Gen1, 2 — Gen2. head — начало EF, нужно сертификатам Gen2 (длина DER).
func ExpectedSize(fid uint16, tag byte, ai AppIdent, head []byte) (int, bool) {
	switch fid {
	case 0x0002:
		return 25, true
	case 0x0005:
		return 8, true
	case 0x0507:
		return 19, true
	case 0x0508:
		return 46, true
	case 0x050E:
		return 4, true
	case 0x0521:
		return 53, true
	case 0x0520:
		return 143, true
	case 0x0503:
		return 2 * ai.FaultsPerType * 24, true
	case 0x0504:
		return 4 + ai.ActivityLength, true
	}
	g2 := tag == 2
	pick := func(g1, g2v int) (int, bool) {
		if g2 {
			return g2v, true
		}
		return g1, true
	}
	switch fid {
	case 0x0501:
		return pick(10, 17)
	case 0x0502:
		return pick(6*ai.EventsPerType*24, 11*ai.EventsPerType*24)
	case 0x0505:
		return pick(2+ai.VehicleRecords*31, 2+ai.VehicleRecords*48)
	case 0x0506:
		return pick(1+ai.PlaceRecords*10, 2+ai.PlaceRecords*21)
	case 0x0522:
		return pick(280, 2+ai.ConditionRecords*5)
	case 0xC100, 0xC108:
		if !g2 {
			return 194, true
		}
	case 0xC101, 0xC109:
		if !g2 {
			return 0, false
		}
	}
	if !g2 {
		return 0, false
	}
	switch fid {
	case 0xC100, 0xC101, 0xC108, 0xC109:
		// DER: 7F21 + длина (короткая или 81/82 + байты)
		if len(head) < 4 || head[0] != 0x7F || head[1] != 0x21 {
			return 0, false
		}
		switch n := int(head[2]); {
		case n < 0x80:
			return 3 + n, true
		case n == 0x81 && len(head) >= 4:
			return 4 + int(head[3]), true
		case n == 0x82 && len(head) >= 5:
			return 5 + int(binary.BigEndian.Uint16(head[3:])), true
		}
	case 0x0523:
		return 2 + ai.VehicleUnitRecords*10, true
	case 0x0524:
		return 2 + ai.GNSSRecords*18, true
	case 0x0525:
		return 10, true
	case 0x0526:
		return 2 + ai.PlaceRecords*5, true
	case 0x0527, 0x0530:
		return 2 + ai.GNSSRecords*5, true
	case 0x0528:
		return 2 + ai.BorderRecords*17, true
	case 0x0529:
		return 2 + ai.LoadUnloadRecords*20, true
	}
	return 0, false
}

// ICC возвращает блок 0002:0 (идентификатор чипа) из файла .ddd или nil.
func ICC(ddd []byte) []byte {
	for len(ddd) >= 5 {
		n := int(binary.BigEndian.Uint16(ddd[3:]))
		if len(ddd) < 5+n {
			return nil
		}
		if ddd[0] == 0 && ddd[1] == 2 && ddd[2] == 0 {
			return ddd[5 : 5+n]
		}
		ddd = ddd[5+n:]
	}
	return nil
}
