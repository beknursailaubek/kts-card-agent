package agent

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"kaztachosystem/card-agent/card"
)

const (
	stateReading = "reading"
	stateDone    = "done"
	stateWriting = "writing"
	stateWritten = "written"
	stateFailed  = "failed"

	readyTTL   = 10 * time.Minute // от готовности файла
	writtenTTL = time.Minute      // после written
	clockSkew  = 5 * time.Minute  // больше — дату не пишем
	sweepEvery = 30 * time.Second // фоновая очистка: байты стираются не позже срока + 30 с
)

// job — одна задача чтения; поля, кроме stop, меняются под server.mu.
type job struct {
	id, origin, reader, mode string

	state          string
	done, total    int
	err, warning   string
	file, icc      []byte
	info           card.Info
	ready, written time.Time
	stop           atomic.Bool // DELETE во время чтения: следующая команда карте не уходит
}

func (j *job) expired(now time.Time) bool {
	return j.state != stateWriting &&
		(!j.ready.IsZero() && now.Sub(j.ready) > readyTTL || !j.written.IsZero() && now.Sub(j.written) > writtenTTL)
}

// drop закрывает текущую задачу (mu взят): чтение прерывается, байты отпускаются.
func (s *server) drop() {
	if s.job != nil {
		s.job.stop.Store(true)
		s.job.file, s.job.icc = nil, nil
		s.job = nil
	}
}

// claim занимает кардридер под чтение или запись (mu взят).
func (s *server) claim(reader string) {
	s.busy, s.idle = reader, make(chan struct{})
}

// free освобождает кардридер (mu взят).
func (s *server) free() {
	s.busy = ""
	close(s.idle)
}

// sweep закрывает истёкшую задачу без обращения к ней (задача в writing не истекает).
func (s *server) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job != nil && s.job.expired(s.cfg.Now()) {
		s.log.Printf("задача закрыта по сроку (%s)", s.job.state)
		s.drop()
	}
}

func (s *server) sweepLoop(tick <-chan time.Time, done <-chan struct{}) {
	for {
		select {
		case <-tick:
			s.sweep()
		case <-done:
			return
		}
	}
}

// lookup — задача из пути, только для создавшего её origin (mu взят); чужая, неизвестная или истёкшая — nil.
func (s *server) lookup(r *http.Request) *job {
	j := s.job
	if j == nil || subtle.ConstantTimeCompare([]byte(j.id), []byte(r.PathValue("job"))) != 1 || j.origin != r.Header.Get("Origin") {
		return nil
	}
	if j.expired(s.cfg.Now()) {
		s.log.Printf("задача закрыта по сроку (%s)", j.state)
		s.drop()
		return nil
	}
	return j
}

func (s *server) startRead(w http.ResponseWriter, r *http.Request) {
	var req struct{ Reader, Mode string }
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Reader == "" || req.Mode != "check" && req.Mode != "download" {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	s.pmu.Lock()
	defer s.pmu.Unlock()
	s.mu.Lock()
	if s.busy != "" {
		s.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		return
	}
	s.drop() // готовая, но не закрытая задача (страницу закрыли) стирается
	s.mu.Unlock()

	readers, err := s.cfg.PCSC.Readers()
	if err != nil {
		s.log.Printf("список кардридеров: %v", err)
	}
	for _, rs := range readers {
		// пустой или занятый кардридер — задача сама закончится card_removed/reader_busy
		if rs.Name != req.Reader {
			continue
		}
		if st := s.probe(rs); st.State == "other" || st.State == "unknown" {
			writeError(w, http.StatusUnprocessableEntity, "not_driver_card")
			return
		}
	}

	b := make([]byte, 16)
	rand.Read(b)
	j := &job{id: hex.EncodeToString(b), origin: r.Header.Get("Origin"), reader: req.Reader, mode: req.Mode, state: stateReading}
	s.mu.Lock()
	s.job = j
	s.claim(j.reader)
	s.mu.Unlock()
	s.log.Printf("чтение (%s) начато", j.mode)
	go s.read(j)
	writeJSON(w, http.StatusAccepted, map[string]string{"job": j.id})
}

// stopConn прерывает чтение между командами после DELETE; Close адаптера сбрасывает карту.
type stopConn struct {
	Conn
	stop *atomic.Bool
}

var errStopped = errors.New("чтение прервано")

func (c stopConn) Transmit(a []byte) ([]byte, error) {
	if c.stop.Load() {
		return nil, errStopped
	}
	return c.Conn.Transmit(a)
}

func (s *server) read(j *job) {
	c, err := s.cfg.PCSC.Connect(j.reader, true)
	var info card.Info
	var file []byte
	if err == nil {
		info, file, err = card.Read(stopConn{c, &j.stop}, card.Options{Progress: func(done, total int) {
			s.mu.Lock()
			j.done, j.total = done, total
			s.mu.Unlock()
		}})
		c.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.free()
	if s.job != j {
		s.log.Print("чтение прервано: задача закрыта")
		return
	}
	if err != nil {
		j.state = stateFailed
		switch {
		case errors.Is(err, ErrNoCard):
			j.err = "card_removed"
		case errors.Is(err, ErrBusy):
			j.err = "reader_busy"
		default:
			j.err = "read_failed"
		}
		s.log.Printf("чтение: %s (%s)", j.state, j.err) // текст ошибки карты не пишем: в нём байты APDU
		return
	}
	j.state, j.file, j.icc, j.info, j.ready = stateDone, file, card.ICC(file), info, s.cfg.Now()
	s.log.Printf("чтение: %s, %s, %d байт", j.state, info, j.total)
}

func (s *server) getJob(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.lookup(r)
	if j == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		State   string `json:"state"`
		Done    int    `json:"done"`
		Total   int    `json:"total"`
		Error   string `json:"error,omitempty"`
		Warning string `json:"warning,omitempty"`
	}{j.state, j.done, j.total, j.err, j.warning})
}

func (s *server) getFile(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	var file []byte
	if j := s.lookup(r); j != nil && (j.state == stateDone || j.state == stateWriting || j.state == stateWritten) {
		file = j.file
	}
	s.mu.Unlock()
	if file == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(file)
}

func (s *server) deleteJob(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.lookup(r)
	switch {
	case j == nil:
		w.WriteHeader(http.StatusNotFound)
	case j.state == stateWriting:
		w.WriteHeader(http.StatusConflict)
	default:
		s.drop()
		s.log.Printf("задача закрыта (%s)", j.state)
		w.WriteHeader(http.StatusNoContent)
	}
}

// commit записывает дату выгрузки на ту же карту: только mode=download и только из done.
func (s *server) commit(w http.ResponseWriter, r *http.Request) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.lookup(r)
	if j == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if j.mode != "download" || j.state != stateDone || s.busy != "" {
		w.WriteHeader(http.StatusConflict)
		return
	}
	var req struct {
		ServerTime time.Time `json:"serverTime"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.ServerTime.IsZero() {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if d := s.cfg.Now().Sub(req.ServerTime); d > clockSkew || d < -clockSkew {
		j.warning = "clock_mismatch"
		s.log.Printf("запись: %s", j.warning)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	j.state, j.warning = stateWriting, ""
	s.claim(j.reader)
	go s.write(j)
	w.WriteHeader(http.StatusAccepted)
}

func (s *server) write(j *job) {
	c, err := s.cfg.PCSC.Connect(j.reader, true)
	if err == nil {
		err = card.WriteDownloadDate(c, j.info, j.icc, s.cfg.Now())
		c.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.free()
	switch {
	case err == nil:
		j.state, j.written = stateWritten, s.cfg.Now()
	case errors.Is(err, card.ErrCardChanged), errors.Is(err, ErrNoCard):
		j.state, j.warning = stateDone, "card_changed"
	default:
		j.state, j.warning = stateDone, "write_failed"
	}
	s.log.Printf("запись: %s %s", j.state, j.warning)
}
