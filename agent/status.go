package agent

import (
	"errors"
	"net/http"

	"kaztachosystem/card-agent/card"
)

// cardState — карта в ответе GET /v1/status: только тип и поколение, без персональных данных.
type cardState struct {
	State      string `json:"state"`
	Generation int    `json:"generation,omitempty"`
	Type       string `json:"type,omitempty"`
}

var (
	stEmpty   = cardState{State: "empty"}
	stBusy    = cardState{State: "busy"}
	stUnknown = cardState{State: "unknown"}
)

// probe — итог опроса карты, действителен до новой вставки.
type probe struct {
	insertion uint64
	card      cardState
}

var otherTypes = map[byte]string{2: "workshop", 3: "control", 4: "company"}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	type readerJSON struct {
		Name string    `json:"name"`
		Card cardState `json:"card"`
	}
	out := []readerJSON{}
	readers, err := s.cfg.PCSC.Readers()
	if err != nil {
		s.log.Printf("список кардридеров: %v", err)
	}
	for _, rs := range readers {
		out = append(out, readerJSON{rs.Name, s.probe(rs)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": s.cfg.Version, "readers": out})
}

// probe определяет карту в кардридере (pmu взят). Новую карту опрашивает один раз (card.Detect без сброса);
// пока идёт наше чтение или запись, к кардридеру не обращается.
func (s *server) probe(rs ReaderState) cardState {
	if !rs.Present {
		delete(s.probes, rs.Name)
		return stEmpty
	}
	s.mu.Lock()
	busy := s.busy == rs.Name
	s.mu.Unlock()
	if busy {
		return stBusy
	}
	if p, ok := s.probes[rs.Name]; ok && p.insertion == rs.Insertion {
		return p.card
	}
	c, err := s.cfg.PCSC.Connect(rs.Name, false)
	if err != nil {
		return connState(err) // не кэшируется: кардридер освободится или карта появится
	}
	info, err := card.Detect(c)
	c.Close()
	var st cardState
	switch {
	case err == nil:
		st = cardState{State: "driver", Generation: info.Generation}
		if info.Generation == 2 && info.V2 {
			st.Generation = 3
		}
	case errors.Is(err, card.ErrNotDriver) && otherTypes[info.Type] != "":
		st = cardState{State: "other", Type: otherTypes[info.Type]}
	case errors.Is(err, ErrNoCard), errors.Is(err, ErrBusy):
		return connState(err)
	default: // не тахограф или не отвечает: до извлечения больше ничего не посылаем
		st = stUnknown
	}
	s.probes[rs.Name] = probe{rs.Insertion, st}
	return st
}

func connState(err error) cardState {
	switch {
	case errors.Is(err, ErrNoCard):
		return stEmpty
	case errors.Is(err, ErrBusy):
		return stBusy
	}
	return stUnknown
}
