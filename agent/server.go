// Package agent — локальный HTTP-сервер программы: статус кардридеров, чтение карты в файл, запись даты выгрузки.
// Граница доверия: отвечает только на Host 127.0.0.1/localhost:47340 и только origin из Config.Origins.
package agent

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"slices"
	"sync"
	"time"

	"kaztachosystem/card-agent/card"
)

// ReaderState — кардридер по данным PC/SC без команд карте; Insertion растёт при каждой вставке карты.
type ReaderState struct {
	Name      string
	Present   bool
	Insertion uint64
}

// Conn — соединение с картой в транзакции PC/SC.
type Conn interface {
	card.Card
	Close() error
}

// PCSC — адаптер PC/SC. Connect с reset=true сбрасывает карту и открывает транзакцию (чтение, запись; Close сбрасывает карту),
// с reset=false — транзакция без сброса (опрос; Close оставляет карту как есть).
type PCSC interface {
	Readers() ([]ReaderState, error)
	Connect(reader string, reset bool) (Conn, error)
}

var (
	ErrBusy   = errors.New("кардридер занят")
	ErrNoCard = errors.New("в кардридере нет карты")
)

type Config struct {
	Version string
	Origins []string
	PCSC    PCSC
	Now     func() time.Time
	// OnShutdown вызывается из обработчика POST /v1/shutdown до ответа 204: остановку сервера запускать в фоне.
	OnShutdown func()
	Log        *log.Logger // nil — без журнала
	// Done останавливает фоновую очистку задач при закрытии; nil — очистка до конца процесса.
	Done <-chan struct{}
}

var allowedHosts = []string{"127.0.0.1:47340", "localhost:47340"}

type server struct {
	cfg Config
	log *log.Logger

	pmu    sync.Mutex // опрос карт и захват кардридера под чтение/запись — по одному
	probes map[string]probe

	mu   sync.Mutex
	job  *job
	busy string        // кардридер, с которым идёт наше чтение или запись
	idle chan struct{} // закрывается, когда busy освобождается
}

// New возвращает обработчик и запускает фоновую очистку: байты карты стираются по сроку, даже если к задаче больше не обращаются.
func New(c Config) http.Handler {
	s, h := newServer(c)
	go func() {
		t := time.NewTicker(sweepEvery)
		defer t.Stop()
		s.sweepLoop(t.C, c.Done)
	}()
	return h
}

func newServer(c Config) (*server, http.Handler) {
	s := &server{cfg: c, log: c.Log, probes: map[string]probe{}}
	if s.log == nil {
		s.log = log.New(io.Discard, "", 0)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", s.status)
	mux.HandleFunc("POST /v1/read", s.startRead)
	mux.HandleFunc("GET /v1/jobs/{job}", s.getJob)
	mux.HandleFunc("GET /v1/jobs/{job}/file", s.getFile)
	mux.HandleFunc("POST /v1/jobs/{job}/commit", s.commit)
	mux.HandleFunc("DELETE /v1/jobs/{job}", s.deleteJob)
	mux.HandleFunc("POST /v1/shutdown", s.shutdown)
	mux.HandleFunc("OPTIONS /", func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE")
		h.Set("Access-Control-Allow-Headers", "Content-Type")
		h.Set("Access-Control-Allow-Private-Network", "true")
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
	})
	return s, s.gate(mux)
}

// gate пропускает запрос только с разрешённым Host и ровно одним разрешённым Origin;
// без Origin — только POST /v1/shutdown (его шлёт установщик, браузер на кросс-доменный POST Origin присылает всегда).
// Отказ — 403 без тела и без заголовков CORS. Журнал — метод, шаблон маршрута (без job) и код.
func (s *server) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{ResponseWriter: w, code: http.StatusOK}
		origin := r.Header.Values("Origin")
		switch {
		case !slices.Contains(allowedHosts, r.Host):
			rec.WriteHeader(http.StatusForbidden)
		case r.URL.Path == "/v1/shutdown":
			if r.Method != http.MethodPost || len(origin) != 0 {
				rec.WriteHeader(http.StatusForbidden)
				break
			}
			next.ServeHTTP(rec, r)
		case len(origin) != 1 || !slices.Contains(s.cfg.Origins, origin[0]):
			rec.WriteHeader(http.StatusForbidden)
		default:
			w.Header().Set("Access-Control-Allow-Origin", origin[0])
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Cache-Control", "no-store")
			next.ServeHTTP(rec, r)
		}
		route := r.Pattern
		if route == "" {
			route = r.Method + " -"
		}
		s.log.Printf("%s → %d", route, rec.code)
	})
}

type recorder struct {
	http.ResponseWriter
	code int
}

func (r *recorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, e string) {
	writeJSON(w, code, map[string]string{"error": e})
}

// decodeBody читает JSON-тело не длиннее 1 КБ.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return false
	}
	return true
}

// shutdown ждёт конца записи на карту (не дольше 10 с), затем вызывает OnShutdown.
func (s *server) shutdown(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	var wait chan struct{}
	if s.job != nil && s.job.state == stateWriting {
		wait = s.idle
	}
	s.mu.Unlock()
	if wait != nil {
		select {
		case <-wait:
		case <-time.After(10 * time.Second):
			s.log.Print("остановка: запись на карту не закончилась за 10 с")
		}
	}
	if s.cfg.OnShutdown != nil {
		s.cfg.OnShutdown()
	}
	w.WriteHeader(http.StatusNoContent)
}
