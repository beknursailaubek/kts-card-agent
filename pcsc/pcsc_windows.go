//go:build windows

package pcsc

import (
	"errors"
	"sync"
	"time"

	"github.com/ebfe/scard"

	"kaztachosystem/card-agent/agent"
)

// Adapter — agent.PCSC на ebfe/scard. Контекст PC/SC создаётся заново, если служба перезапустилась.
type Adapter struct {
	mu       sync.Mutex
	ctx      *scard.Context
	watching map[string]bool
	ins      map[string]uint64
	present  map[string]bool
}

func New() *Adapter {
	return &Adapter{watching: map[string]bool{}, ins: map[string]uint64{}, present: map[string]bool{}}
}

// wrap переводит ошибку scard в ошибку агента (nil для «нет кардридеров/службы»).
func wrap(err error) error {
	var se scard.Error
	if errors.As(err, &se) {
		return ErrorOf(uint32(se))
	}
	return err
}

// wrap2 — как wrap, но ошибка без сопоставления остаётся исходной (в т.ч. при кодах «нет службы»).
func wrap2(err error) error {
	if err == nil {
		return nil
	}
	if e := wrap(err); e != nil {
		return e
	}
	return err
}

func (a *Adapter) context() (*scard.Context, error) {
	if a.ctx != nil {
		if _, err := a.ctx.ListReaders(); err == nil || !isStale(err) {
			return a.ctx, nil
		}
		_ = a.ctx.Release()
		a.ctx = nil
	}
	c, err := scard.EstablishContext()
	if err != nil {
		return nil, err
	}
	a.ctx = c
	return c, nil
}

func isStale(err error) bool {
	var se scard.Error
	return errors.As(err, &se) && (se == scard.ErrInvalidHandle || se == scard.ErrServiceStopped || se == scard.ErrNoService || se == scard.ErrCommError)
}

func (a *Adapter) Readers() ([]agent.ReaderState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ctx, err := a.context()
	if err != nil {
		return nil, wrap(err)
	}
	names, err := ctx.ListReaders()
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]agent.ReaderState, 0, len(names))
	for _, n := range names {
		if !a.watching[n] {
			a.watching[n] = true
			go a.watch(n)
		}
		out = append(out, agent.ReaderState{Name: n, Present: a.present[n], Insertion: a.ins[n]})
	}
	return out, nil
}

// watch следит за одним кардридером отдельным контекстом; уходит, когда кардридер пропал.
func (a *Adapter) watch(name string) {
	defer func() { a.mu.Lock(); delete(a.watching, name); delete(a.present, name); a.mu.Unlock() }()
	ctx, err := scard.EstablishContext()
	if err != nil {
		return
	}
	defer ctx.Release()
	st := []scard.ReaderState{{Reader: name, CurrentState: scard.StateUnaware}}
	for {
		err := ctx.GetStatusChange(st, time.Second)
		if err != nil && !errors.Is(err, scard.ErrTimeout) {
			if errors.Is(err, scard.ErrUnknownReader) || errors.Is(err, scard.ErrReaderUnavailable) || isStale(err) {
				return
			}
			time.Sleep(time.Second)
			continue
		}
		if err == nil {
			p := st[0].EventState&scard.StatePresent != 0
			a.mu.Lock()
			if p && !a.present[name] { // и карта, уже стоявшая к первому опросу: её надо проверить один раз
				a.ins[name]++
			}
			a.present[name] = p
			a.mu.Unlock()
			st[0].CurrentState = st[0].EventState
		}
	}
}

type conn struct {
	c     *scard.Card
	reset bool
}

func (a *Adapter) Connect(reader string, reset bool) (agent.Conn, error) {
	a.mu.Lock()
	ctx, err := a.context()
	a.mu.Unlock()
	if err != nil {
		return nil, wrap2(err)
	}
	c, err := ctx.Connect(reader, scard.ShareShared, scard.ProtocolAny)
	if err != nil {
		return nil, wrap2(err)
	}
	// сброс: после ATR текущий каталог — MF (TCS_18), иначе EF 0002/0005 не выбрать
	if reset {
		if err := c.Reconnect(scard.ShareShared, scard.ProtocolAny, scard.ResetCard); err != nil {
			_ = c.Disconnect(scard.ResetCard)
			return nil, wrap2(err)
		}
	}
	if err := c.BeginTransaction(); err != nil {
		_ = c.Disconnect(scard.LeaveCard)
		return nil, wrap2(err)
	}
	return &conn{c, reset}, nil
}

func (c *conn) Transmit(apdu []byte) ([]byte, error) {
	r, err := c.c.Transmit(apdu)
	return r, wrap2(err)
}

func (c *conn) Close() error {
	_ = c.c.EndTransaction(scard.LeaveCard)
	if c.reset {
		return c.c.Disconnect(scard.ResetCard)
	}
	return c.c.Disconnect(scard.LeaveCard)
}
