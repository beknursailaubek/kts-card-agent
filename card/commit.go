package card

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrCardChanged — в считывателе не та карта, что была прочитана (или её не удалось опознать): на карту ничего не записано.
	ErrCardChanged = errors.New("В считывателе другая карта")
	// ErrWrite — карта не приняла запись даты выгрузки; повтор с той же датой безвреден.
	ErrWrite = errors.New("Не удалось записать дату выгрузки на карту")
)

// WriteDownloadDate записывает now в LastCardDownload (EF Card_Download 050E, TimeReal) DF Tachograph и, у Gen2, DF Tachograph_G2.
// Карта должна быть только что сброшена (текущий DF — MF) и внутри SCardBeginTransaction, как для Read.
// Перед записью сверяет EF ICC (0002) с icc из выгруженного файла: не совпало или не прочиталось — ErrCardChanged без записи.
func WriteDownloadDate(c Card, info Info, icc []byte, now time.Time) error {
	s := &session{c: c}
	if err := s.sameICC(icc); err != nil {
		return fmt.Errorf("%w: %w", ErrCardChanged, err)
	}

	cmd := binary.BigEndian.AppendUint32([]byte{0x00, 0xD6, 0x00, 0x00, 0x04}, uint32(now.Unix()))
	aids := [][]byte{aidG1}
	if info.Generation >= 2 {
		aids = append(aids, aidG2)
	}
	for _, aid := range aids {
		for _, apdu := range [][]byte{selectAIDCmd(aid), selectCmd(0x050E), cmd} {
			if err := s.expect(apdu); err != nil {
				return fmt.Errorf("%w: %w", ErrWrite, err)
			}
		}
	}
	return nil
}

// sameICC читает EF ICC (0002) из MF целиком и сравнивает с icc. Сами байты в ошибку не попадают (идентификатор чипа).
func (s *session) sameICC(icc []byte) error {
	if err := s.expect(selectCmd(0x0002)); err != nil {
		return err
	}
	size, _ := ExpectedSize(0x0002, 0, AppIdent{}, nil)
	got, err := s.readEF(0x0002, size)
	if err == nil && !bytes.Equal(got, icc) {
		err = errors.New("EF ICC не совпал с прочитанным")
	}
	return err
}
