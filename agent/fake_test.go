package agent

import (
	"bytes"
	"errors"
	"sync"
	"time"

	"kaztachosystem/card-agent/cardtest"
)

func filled(n int, v byte) []byte { return bytes.Repeat([]byte{v}, n) }

var (
	iccID   = filled(25, 0x11)
	cardNo  = []byte("D123456789ABCDEF") // номер карты в 0520: не должен попасть ни в статус, ни в журнал
	oldDate = []byte{0x60, 0, 0, 0}
)

// gen1Driver — синтетическая карта водителя Gen1 с EF ICC и 050E (для записи даты).
func gen1Driver(icc []byte) *cardtest.Card {
	id := append(append([]byte{0x1A}, cardNo...), filled(126, 7)...)
	return cardtest.FromDDD(cardtest.Enc([]cardtest.Block{
		{Key: "0002:0", Data: icc}, {Key: "0005:0", Data: filled(8, 2)},
		{Key: "0501:0", Data: []byte{1, 0, 0, 3, 3, 0x35, 0xD0, 0, 3, 3}}, {Key: "0501:1", Data: filled(128, 4)},
		{Key: "c100:0", Data: filled(194, 5)}, {Key: "c108:0", Data: filled(194, 6)},
		{Key: "0520:0", Data: id}, {Key: "0520:1", Data: filled(128, 8)},
		{Key: "050e:0", Data: oldDate}, {Key: "050e:1", Data: filled(128, 9)},
		{Key: "0504:0", Data: filled(13780, 9)}, {Key: "0504:1", Data: filled(128, 10)},
	}))
}

func gen2v2Driver() *cardtest.Card {
	return cardtest.FromDDD(cardtest.Enc([]cardtest.Block{
		{Key: "0501:2", Data: []byte{1, 0, 0, 1, 1, 0, 100, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1}},
		{Key: "0525:2", Data: filled(10, 1)},
	}))
}

func workshop() *cardtest.Card {
	return cardtest.FromDDD(cardtest.Enc([]cardtest.Block{{Key: "0501:0", Data: append([]byte{2, 0, 0}, filled(8, 0)...)}}))
}

type connectCall struct {
	reader string
	reset  bool
}

type fakeReader struct {
	name      string
	card      *cardtest.Card
	insertion uint64
}

// fakePCSC — подделка PC/SC поверх cardtest: кардридеры по порядку, журнал Connect и команд, задержка команды с заданным INS.
// Карта в кардридере — образец: каждое Connect получает его копию в начальном состоянии (как после сброса, текущий DF — MF)
// с общими Files, поэтому запись видна в образце.
type fakePCSC struct {
	mu       sync.Mutex
	readers  []*fakeReader
	connects []connectCall
	sent     [][]byte      // команды всех соединений
	holdINS  byte          // команды с этим INS в соединениях reset=true ждут hold
	hold     chan struct{} // nil — не ждать
	once     sync.Once
}

func newFake(cards ...*cardtest.Card) *fakePCSC {
	f := &fakePCSC{}
	for i, c := range cards {
		f.readers = append(f.readers, &fakeReader{name: "R" + string(rune('1'+i)), card: c, insertion: 1})
	}
	return f
}

func (f *fakePCSC) reader(name string) *fakeReader {
	for _, r := range f.readers {
		if r.name == name {
			return r
		}
	}
	return nil
}

func (f *fakePCSC) Readers() ([]ReaderState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ReaderState
	for _, r := range f.readers {
		out = append(out, ReaderState{Name: r.name, Present: r.card != nil, Insertion: r.insertion})
	}
	return out, nil
}

func (f *fakePCSC) Connect(name string, reset bool) (Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connects = append(f.connects, connectCall{name, reset})
	r := f.reader(name)
	if r == nil || r.card == nil {
		return nil, ErrNoCard
	}
	c := *r.card
	c.Sent = nil
	return &fakeConn{f: f, card: &c, reset: reset}, nil
}

// hasD6 — уходила ли на какую-либо карту команда UPDATE BINARY.
func (f *fakePCSC) hasD6() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.sent {
		if len(a) > 1 && a[1] == 0xD6 {
			return true
		}
	}
	return false
}

// insert вставляет карту (nil — вынуть) в кардридер: Insertion растёт.
func (f *fakePCSC) insert(name string, c *cardtest.Card) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.reader(name)
	r.card, r.insertion = c, r.insertion+1
}

func (f *fakePCSC) holdOn(ins byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holdINS, f.hold = ins, make(chan struct{})
}

func (f *fakePCSC) release() {
	f.mu.Lock()
	h := f.hold
	f.mu.Unlock()
	if h != nil {
		f.once.Do(func() { close(h) })
	}
}

// count — число Connect к кардридеру с данным reset ("" — к любому).
func (f *fakePCSC) count(name string, reset bool) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.connects {
		if (name == "" || c.reader == name) && c.reset == reset {
			n++
		}
	}
	return n
}

type fakeConn struct {
	f     *fakePCSC
	card  *cardtest.Card
	reset bool
}

func (c *fakeConn) Transmit(a []byte) ([]byte, error) {
	c.f.mu.Lock()
	ins, hold := c.f.holdINS, c.f.hold
	c.f.sent = append(c.f.sent, append([]byte{}, a...))
	c.f.mu.Unlock()
	if c.reset && hold != nil && len(a) > 1 && a[1] == ins {
		<-hold
	}
	resp, err := c.card.Transmit(a)
	if errors.Is(err, cardtest.ErrRemoved) {
		err = ErrNoCard // так адаптер PC/SC сообщает об извлечённой карте
	}
	return resp, err
}

func (c *fakeConn) Close() error { return nil }

// clock — управляемые часы для Config.Now.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// syncBuf — журнал, который пишут горутины сервера и читает тест.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
