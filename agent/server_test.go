package agent

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kaztachosystem/card-agent/cardtest"
)

const (
	originA = "https://kts.zerdeli.com.kz:8443"
	originB = "https://other.kts.example"
	host    = "127.0.0.1:47340"
)

type env struct {
	t        *testing.T
	h        http.Handler
	s        *server
	f        *fakePCSC
	clk      *clock
	log      *syncBuf
	shutdown atomic.Int32
	onShut   func()
}

func newEnv(t *testing.T, cards ...*cardtest.Card) *env {
	e := &env{t: t, f: newFake(cards...), clk: &clock{t: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}, log: &syncBuf{}}
	e.s, e.h = newServer(Config{
		Version: "1.0.0", Origins: []string{originA, originB}, PCSC: e.f, Now: e.clk.Now,
		OnShutdown: func() {
			if e.onShut != nil {
				e.onShut()
			}
			e.shutdown.Add(1)
		},
		Log: log.New(e.log, "", 0),
	})
	t.Cleanup(e.f.release)
	return e
}

type reqOpt func(*http.Request)

func withHost(h string) reqOpt { return func(r *http.Request) { r.Host = h } }

func (e *env) do(method, path, origin string, body any, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, rd)
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("тело %q: %v", w.Body.String(), err)
	}
	return v
}

type jobJSON struct {
	State   string `json:"state"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Error   string `json:"error"`
	Warning string `json:"warning"`
}

var jobRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// read запускает чтение кардридера от originA и возвращает job.
func (e *env) read(reader, mode string) string {
	e.t.Helper()
	w := e.do("POST", "/v1/read", originA, map[string]string{"reader": reader, "mode": mode})
	if w.Code != 202 {
		e.t.Fatalf("POST /v1/read: %d %s", w.Code, w.Body)
	}
	job := decode[struct{ Job string }](e.t, w).Job
	if !jobRe.MatchString(job) {
		e.t.Fatalf("job %q — не 32 hex-символа", job)
	}
	return job
}

// wait опрашивает задачу, пока она в переходном состоянии (reading/writing), и возвращает итог.
func (e *env) wait(job string) jobJSON {
	e.t.Helper()
	for i := 0; i < 5000; i++ {
		w := e.do("GET", "/v1/jobs/"+job, originA, nil)
		if w.Code != 200 {
			e.t.Fatalf("GET job: %d", w.Code)
		}
		j := decode[jobJSON](e.t, w)
		if j.State != "reading" && j.State != "writing" {
			return j
		}
		time.Sleep(time.Millisecond)
	}
	e.t.Fatal("задача не завершилась")
	return jobJSON{}
}

func (e *env) waitState(job, state string) {
	e.t.Helper()
	for i := 0; i < 5000; i++ {
		if j := decode[jobJSON](e.t, e.do("GET", "/v1/jobs/"+job, originA, nil)); j.State == state {
			return
		}
		time.Sleep(time.Millisecond)
	}
	e.t.Fatalf("задача не перешла в %s", state)
}

func (e *env) commit(job string, serverTime time.Time) *httptest.ResponseRecorder {
	return e.do("POST", "/v1/jobs/"+job+"/commit", originA, map[string]time.Time{"serverTime": serverTime})
}

func snapshot(m *cardtest.Card) map[string][]byte {
	c := map[string][]byte{}
	for k, v := range m.Files {
		c[k] = append([]byte{}, v...)
	}
	return c
}

func sameFiles(m *cardtest.Card, before map[string][]byte) bool {
	if len(m.Files) != len(before) {
		return false
	}
	for k, v := range before {
		if !bytes.Equal(m.Files[k], v) {
			return false
		}
	}
	return true
}

func TestHostAndOrigin(t *testing.T) {
	e := newEnv(t)
	for name, w := range map[string]*httptest.ResponseRecorder{
		"чужой Host":   e.do("GET", "/v1/status", originA, nil, withHost("evil.example:47340")),
		"чужой Origin": e.do("GET", "/v1/status", "https://evil.example", nil),
		"без Origin":   e.do("GET", "/v1/status", "", nil),
		"Origin null":  e.do("GET", "/v1/status", "null", nil),
		"Origin с /":   e.do("GET", "/v1/status", originA+"/", nil),
	} {
		if w.Code != 403 || w.Body.Len() != 0 || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: %d, тело %q, ACAO %q", name, w.Code, w.Body, w.Header().Get("Access-Control-Allow-Origin"))
		}
	}
	for _, h := range []string{"127.0.0.1:47340", "localhost:47340"} {
		w := e.do("GET", "/v1/status", originA, nil, withHost(h))
		if w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != originA || w.Header().Get("Vary") != "Origin" {
			t.Errorf("Host %s: %d, ACAO %q, Vary %q", h, w.Code, w.Header().Get("Access-Control-Allow-Origin"), w.Header().Get("Vary"))
		}
	}
}

func TestPreflight(t *testing.T) {
	e := newEnv(t)
	w := e.do("OPTIONS", "/v1/read", originA, nil)
	want := map[string]string{
		"Access-Control-Allow-Origin":          originA,
		"Access-Control-Allow-Methods":         "GET, POST, DELETE",
		"Access-Control-Allow-Headers":         "Content-Type",
		"Access-Control-Allow-Private-Network": "true",
		"Access-Control-Max-Age":               "600",
	}
	if w.Code != 204 {
		t.Fatalf("OPTIONS: %d", w.Code)
	}
	for k, v := range want {
		if got := w.Header().Get(k); got != v {
			t.Errorf("%s = %q, ждали %q", k, got, v)
		}
	}
	for _, o := range []string{"https://evil.example", ""} {
		w = e.do("OPTIONS", "/v1/read", o, nil)
		if w.Code != 403 || w.Body.Len() != 0 || w.Header().Get("Access-Control-Allow-Origin") != "" || w.Header().Get("Access-Control-Allow-Methods") != "" {
			t.Errorf("OPTIONS от %q: %d, заголовки %v", o, w.Code, w.Header())
		}
	}
}

type statusJSON struct {
	Version string `json:"version"`
	Readers []struct {
		Name string         `json:"name"`
		Card map[string]any `json:"card"`
	} `json:"readers"`
}

func (e *env) status() (statusJSON, string) {
	e.t.Helper()
	w := e.do("GET", "/v1/status", originA, nil)
	if w.Code != 200 {
		e.t.Fatalf("status: %d", w.Code)
	}
	return decode[statusJSON](e.t, w), w.Body.String()
}

func cardOf(t *testing.T, s statusJSON, reader string) string {
	t.Helper()
	for _, r := range s.Readers {
		if r.Name == reader {
			b, _ := json.Marshal(r.Card)
			return string(b)
		}
	}
	t.Fatalf("нет кардридера %s", reader)
	return ""
}

func TestStatusProbeOnce(t *testing.T) {
	// R1 — не тахограф, R2 — Gen2v2, R3 — мастерская, R4 — пусто, R5 — водитель Gen1
	e := newEnv(t, cardtest.FromDDD(nil), gen2v2Driver(), workshop(), nil, gen1Driver(iccID))
	var body string
	for i := 0; i < 3; i++ {
		s, b := e.status()
		body = b
		if s.Version != "1.0.0" || len(s.Readers) != 5 {
			t.Fatalf("status: %s", b)
		}
		for reader, want := range map[string]string{
			"R1": `{"state":"unknown"}`, "R2": `{"generation":3,"state":"driver"}`, "R3": `{"state":"other","type":"workshop"}`,
			"R4": `{"state":"empty"}`, "R5": `{"generation":1,"state":"driver"}`,
		} {
			if got := cardOf(t, s, reader); got != want {
				t.Errorf("%s: %s, ждали %s", reader, got, want)
			}
		}
	}
	for _, r := range []string{"R1", "R2", "R3", "R5"} {
		if n := e.f.count(r, false); n != 1 || e.f.count(r, true) != 0 {
			t.Errorf("%s: опросов %d (reset=false), с reset=true %d; ждали 1 и 0", r, n, e.f.count(r, true))
		}
	}
	if n := e.f.count("R4", false) + e.f.count("R4", true); n != 0 {
		t.Errorf("пустой кардридер: Connect %d раз", n)
	}
	if strings.Contains(body, string(cardNo)) || strings.Contains(strings.ToUpper(body), strings.ToUpper(hex.EncodeToString(iccID))) {
		t.Errorf("в статусе данные карты: %s", body)
	}

	// новая вставка той же карты — опрос ещё раз, но не тахограф по-прежнему unknown
	e.f.insert("R1", cardtest.FromDDD(nil))
	s, _ := e.status()
	if got := cardOf(t, s, "R1"); got != `{"state":"unknown"}` || e.f.count("R1", false) != 2 {
		t.Errorf("после вставки: %s, опросов %d", got, e.f.count("R1", false))
	}

	// карту вынули — empty без Connect
	e.f.insert("R3", nil)
	if s, _ = e.status(); cardOf(t, s, "R3") != `{"state":"empty"}` || e.f.count("R3", false) != 1 {
		t.Errorf("вынутая карта: %s", cardOf(t, s, "R3"))
	}

	// идёт чтение с R5 — busy, Connect для опроса не вызывается
	e.f.holdOn(0xB0)
	job := e.read("R5", "check")
	before := e.f.count("", false)
	if s, _ = e.status(); cardOf(t, s, "R5") != `{"state":"busy"}` || e.f.count("", false) != before {
		t.Errorf("во время чтения: %s, опросов было %d, стало %d", cardOf(t, s, "R5"), before, e.f.count("", false))
	}
	e.f.release()
	if j := e.wait(job); j.State != "done" {
		t.Fatalf("чтение: %+v", j)
	}
	if s, _ = e.status(); cardOf(t, s, "R5") != `{"generation":1,"state":"driver"}` {
		t.Errorf("после чтения: %s", cardOf(t, s, "R5"))
	}
}

func TestCheckFlow(t *testing.T) {
	m := gen1Driver(iccID)
	before := snapshot(m)
	e := newEnv(t, m)
	job := e.read("R1", "check")
	j := e.wait(job)
	if j.State != "done" || j.Done != j.Total || j.Total == 0 || j.Error != "" {
		t.Fatalf("задача: %+v", j)
	}
	f1 := e.do("GET", "/v1/jobs/"+job+"/file", originA, nil)
	f2 := e.do("GET", "/v1/jobs/"+job+"/file", originA, nil)
	if f1.Code != 200 || f1.Body.Len() == 0 || !bytes.Equal(f1.Body.Bytes(), f2.Body.Bytes()) || f1.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("файл: %d/%d, %d байт, %q", f1.Code, f2.Code, f1.Body.Len(), f1.Header().Get("Content-Type"))
	}
	if w := e.commit(job, e.clk.Now()); w.Code != 409 {
		t.Fatalf("commit в режиме check: %d", w.Code)
	}
	if e.f.hasD6() || !sameFiles(m, before) || e.f.count("", true) != 1 {
		t.Fatalf("check писал на карту: D6=%v, Connect reset=true %d", e.f.hasD6(), e.f.count("", true))
	}
	if w := e.do("DELETE", "/v1/jobs/"+job, originA, nil); w.Code != 204 {
		t.Fatalf("DELETE: %d", w.Code)
	}
	if w := e.do("GET", "/v1/jobs/"+job+"/file", originA, nil); w.Code != 404 {
		t.Fatalf("файл после DELETE: %d", w.Code)
	}
}

func TestDownloadFlow(t *testing.T) {
	m := gen1Driver(iccID)
	e := newEnv(t, m)
	job := e.read("R1", "download")
	if j := e.wait(job); j.State != "done" {
		t.Fatalf("задача: %+v", j)
	}
	file := e.do("GET", "/v1/jobs/"+job+"/file", originA, nil).Body.Bytes()
	if e.f.hasD6() {
		t.Fatal("D6 до commit")
	}
	if w := e.commit(job, e.clk.Now()); w.Code != 202 {
		t.Fatalf("commit: %d %s", w.Code, w.Body)
	}
	if j := e.wait(job); j.State != "written" || j.Warning != "" {
		t.Fatalf("после commit: %+v", j)
	}
	want := binary.BigEndian.AppendUint32(nil, uint32(e.clk.Now().Unix()))
	if !e.f.hasD6() || !bytes.Equal(m.Files["050e:0"], want) {
		t.Fatalf("050e:0 = %X, ждали %X", m.Files["050e:0"], want)
	}
	if w := e.do("GET", "/v1/jobs/"+job+"/file", originA, nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), file) {
		t.Fatalf("файл в written: %d", w.Code)
	}
	if w := e.commit(job, e.clk.Now()); w.Code != 409 {
		t.Fatalf("повторный commit после written: %d", w.Code)
	}
	e.clk.add(61 * time.Second)
	if w := e.do("GET", "/v1/jobs/"+job, originA, nil); w.Code != 404 {
		t.Fatalf("через 61 с после written: %d", w.Code)
	}
	// журнал есть, но без job, номера карты, ICC и APDU
	l := e.log.String()
	if !strings.Contains(l, "written") || strings.Contains(l, job) || strings.Contains(l, string(cardNo)) ||
		strings.Contains(strings.ToUpper(l), "1111111111") || strings.Contains(strings.ToUpper(l), "00D6") {
		t.Fatalf("журнал:\n%s", l)
	}
}

func TestWritingLocks(t *testing.T) {
	m := gen1Driver(iccID)
	e := newEnv(t, m)
	var filesAtShutdown []byte
	e.onShut = func() { filesAtShutdown = append([]byte{}, m.Files["050e:0"]...) }
	job := e.read("R1", "download")
	e.wait(job)
	e.f.holdOn(0xD6)
	if w := e.commit(job, e.clk.Now()); w.Code != 202 {
		t.Fatalf("commit: %d", w.Code)
	}
	e.waitState(job, "writing")
	if w := e.do("DELETE", "/v1/jobs/"+job, originA, nil); w.Code != 409 {
		t.Errorf("DELETE во время записи: %d", w.Code)
	}
	if w := e.commit(job, e.clk.Now()); w.Code != 409 {
		t.Errorf("второй commit во время записи: %d", w.Code)
	}
	if w := e.do("POST", "/v1/read", originA, map[string]string{"reader": "R1", "mode": "check"}); w.Code != 409 {
		t.Errorf("POST /v1/read во время записи: %d", w.Code)
	}
	done := make(chan int)
	go func() { done <- e.do("POST", "/v1/shutdown", "", nil).Code }()
	select {
	case <-done:
		t.Fatal("shutdown вернулся до конца записи")
	case <-time.After(50 * time.Millisecond):
	}
	if e.shutdown.Load() != 0 {
		t.Fatal("OnShutdown до конца записи")
	}
	e.f.release()
	if code := <-done; code != 204 {
		t.Fatalf("shutdown: %d", code)
	}
	want := binary.BigEndian.AppendUint32(nil, uint32(e.clk.Now().Unix()))
	if e.shutdown.Load() != 1 || !bytes.Equal(filesAtShutdown, want) {
		t.Fatalf("OnShutdown %d раз, дата на карте в момент вызова %X", e.shutdown.Load(), filesAtShutdown)
	}
	if j := e.wait(job); j.State != "written" {
		t.Fatalf("задача: %+v", j)
	}
}

func TestClockMismatch(t *testing.T) {
	m := gen1Driver(iccID)
	e := newEnv(t, m)
	job := e.read("R1", "download")
	e.wait(job)
	if w := e.commit(job, e.clk.Now().Add(6*time.Minute)); w.Code != 202 {
		t.Fatalf("commit: %d", w.Code)
	}
	if j := e.wait(job); j.State != "done" || j.Warning != "clock_mismatch" {
		t.Fatalf("задача: %+v", j)
	}
	if e.f.count("", true) != 1 || e.f.hasD6() {
		t.Fatalf("Connect reset=true %d раз, D6=%v", e.f.count("", true), e.f.hasD6())
	}
}

func TestCardRemovedBeforeCommit(t *testing.T) {
	e := newEnv(t, gen1Driver(iccID))
	job := e.read("R1", "download")
	e.wait(job)
	other := gen1Driver(filled(25, 0x22))
	e.f.insert("R1", other)
	if w := e.commit(job, e.clk.Now()); w.Code != 202 {
		t.Fatalf("commit: %d", w.Code)
	}
	if j := e.wait(job); j.State != "done" || j.Warning != "card_changed" {
		t.Fatalf("задача: %+v", j)
	}
	if e.f.hasD6() {
		t.Fatal("запись на другую карту")
	}
	// карту вынули совсем — тоже card_changed
	e.f.insert("R1", nil)
	e.commit(job, e.clk.Now())
	if j := e.wait(job); j.State != "done" || j.Warning != "card_changed" {
		t.Fatalf("без карты: %+v", j)
	}
}

func TestWriteFailedRetry(t *testing.T) {
	m := gen1Driver(iccID)
	e := newEnv(t, m)
	job := e.read("R1", "download")
	e.wait(job)
	m.FailWrite = 1
	e.commit(job, e.clk.Now())
	if j := e.wait(job); j.State != "done" || j.Warning != "write_failed" {
		t.Fatalf("задача: %+v", j)
	}
	m.FailWrite = 0
	if w := e.commit(job, e.clk.Now()); w.Code != 202 {
		t.Fatalf("повтор commit: %d", w.Code)
	}
	if j := e.wait(job); j.State != "written" || j.Warning != "" {
		t.Fatalf("после повтора: %+v", j)
	}
}

func TestReadRemoved(t *testing.T) {
	m := gen1Driver(iccID)
	m.RemoveAfter = 20
	e := newEnv(t, m)
	job := e.read("R1", "download")
	if j := e.wait(job); j.State != "failed" || j.Error != "card_removed" {
		t.Fatalf("задача: %+v", j)
	}
	if w := e.do("GET", "/v1/jobs/"+job+"/file", originA, nil); w.Code != 404 {
		t.Fatalf("файл: %d", w.Code)
	}
}

func TestOrphanReplaced(t *testing.T) {
	e := newEnv(t, gen1Driver(iccID))
	old := e.read("R1", "download")
	e.wait(old)
	e.f.holdOn(0xB0)
	job := e.read("R1", "check")
	if w := e.do("GET", "/v1/jobs/"+old, originA, nil); w.Code != 404 {
		t.Fatalf("старая задача: %d", w.Code)
	}
	if w := e.do("POST", "/v1/read", originA, map[string]string{"reader": "R1", "mode": "check"}); w.Code != 409 {
		t.Fatalf("второе чтение во время первого: %d", w.Code)
	}
	e.f.release()
	if j := e.wait(job); j.State != "done" {
		t.Fatalf("задача: %+v", j)
	}
}

func TestDeleteWhileReading(t *testing.T) {
	m := gen1Driver(iccID)
	e := newEnv(t, m)
	e.f.holdOn(0xB0)
	job := e.read("R1", "check")
	if w := e.do("DELETE", "/v1/jobs/"+job, originA, nil); w.Code != 204 {
		t.Fatalf("DELETE: %d", w.Code)
	}
	e.f.release()
	// чтение прервано: новое чтение принимается, как только кардридер освобождён
	for i := 0; ; i++ {
		w := e.do("POST", "/v1/read", originA, map[string]string{"reader": "R1", "mode": "check"})
		if w.Code == 202 {
			break
		}
		if w.Code != 409 || i > 5000 {
			t.Fatalf("новое чтение: %d", w.Code)
		}
		time.Sleep(time.Millisecond)
	}
	if w := e.do("GET", "/v1/jobs/"+job, originA, nil); w.Code != 404 {
		t.Fatalf("удалённая задача: %d", w.Code)
	}
}

func TestOtherOrigin(t *testing.T) {
	e := newEnv(t, gen1Driver(iccID))
	job := e.read("R1", "download")
	e.wait(job)
	for _, rt := range []struct{ method, path string }{
		{"GET", "/v1/jobs/" + job}, {"GET", "/v1/jobs/" + job + "/file"},
		{"POST", "/v1/jobs/" + job + "/commit"}, {"DELETE", "/v1/jobs/" + job},
	} {
		var body any
		if rt.method == "POST" {
			body = map[string]time.Time{"serverTime": e.clk.Now()}
		}
		if w := e.do(rt.method, rt.path, originB, body); w.Code != 404 {
			t.Errorf("%s %s от B: %d", rt.method, rt.path, w.Code)
		}
	}
	if e.f.count("", true) != 1 {
		t.Fatalf("Connect для записи: %d", e.f.count("", true)-1)
	}
	if w := e.do("GET", "/v1/jobs/"+job, originA, nil); w.Code != 200 || decode[jobJSON](t, w).State != "done" {
		t.Fatalf("задача A: %d %s", w.Code, w.Body)
	}
}

func TestTTL(t *testing.T) {
	e := newEnv(t, gen1Driver(iccID))
	job := e.read("R1", "check")
	e.wait(job)
	e.clk.add(10 * time.Minute)
	if w := e.do("GET", "/v1/jobs/"+job, originA, nil); w.Code != 200 {
		t.Fatalf("ровно 10 мин: %d", w.Code)
	}
	e.clk.add(time.Second)
	if w := e.do("GET", "/v1/jobs/"+job, originA, nil); w.Code != 404 {
		t.Fatalf("10 мин 1 с: %d", w.Code)
	}
}

func TestNotDriver(t *testing.T) {
	e := newEnv(t, workshop(), cardtest.FromDDD(nil))
	for _, r := range []string{"R1", "R2"} {
		w := e.do("POST", "/v1/read", originA, map[string]string{"reader": r, "mode": "check"})
		if w.Code != 422 || decode[map[string]string](t, w)["error"] != "not_driver_card" {
			t.Errorf("%s: %d %s", r, w.Code, w.Body)
		}
	}
	if e.f.count("", true) != 0 {
		t.Fatal("карта не водителя читалась")
	}
}

func TestBadRequest(t *testing.T) {
	e := newEnv(t, gen1Driver(iccID))
	for _, b := range []any{map[string]string{"reader": "R1", "mode": "write"}, map[string]string{"mode": "check"}, "x"} {
		if w := e.do("POST", "/v1/read", originA, b); w.Code != 400 {
			t.Errorf("%v: %d", b, w.Code)
		}
	}
	job := e.read("R1", "download")
	e.wait(job)
	if w := e.do("POST", "/v1/jobs/"+job+"/commit", originA, map[string]string{}); w.Code != 400 {
		t.Errorf("commit без serverTime: %d", w.Code)
	}
}

func TestShutdown(t *testing.T) {
	e := newEnv(t)
	if w := e.do("POST", "/v1/shutdown", "", nil); w.Code != 204 || e.shutdown.Load() != 1 {
		t.Fatalf("без Origin: %d, OnShutdown %d", w.Code, e.shutdown.Load())
	}
	if w := e.do("POST", "/v1/shutdown", originA, nil); w.Code != 403 || e.shutdown.Load() != 1 {
		t.Fatalf("с Origin: %d, OnShutdown %d", w.Code, e.shutdown.Load())
	}
	if w := e.do("POST", "/v1/shutdown", "", nil, withHost("evil.example:47340")); w.Code != 403 || e.shutdown.Load() != 1 {
		t.Fatalf("чужой Host: %d", w.Code)
	}
	if w := e.do("GET", "/v1/status", "", nil); w.Code != 403 {
		t.Fatalf("без Origin на другой маршрут: %d", w.Code)
	}
}

// Фоновая очистка: истёкшая задача и её байты убираются без HTTP-запроса; задача в writing не трогается.
func TestSweep(t *testing.T) {
	e := newEnv(t, gen1Driver(iccID))
	job := e.read("R1", "check")
	e.wait(job)
	e.s.mu.Lock()
	j := e.s.job
	e.s.mu.Unlock()
	e.clk.add(10 * time.Minute)
	e.s.sweep()
	e.s.mu.Lock()
	kept := e.s.job == j && j.file != nil
	e.s.mu.Unlock()
	if !kept {
		t.Fatal("ровно 10 мин: задачу убрали раньше срока")
	}

	// срок истёк: очистка по тику цикла, затем цикл останавливается по Done
	e.clk.add(time.Second)
	tick, done, exited := make(chan time.Time), make(chan struct{}), make(chan struct{})
	go func() { e.s.sweepLoop(tick, done); close(exited) }()
	tick <- e.clk.Now()
	close(done)
	<-exited
	e.s.mu.Lock()
	gone := e.s.job == nil && j.file == nil && j.icc == nil
	e.s.mu.Unlock()
	if !gone {
		t.Fatal("после срока задача или её байты остались в памяти")
	}

	// writing при истёкшем сроке не стирается
	w := newEnv(t, gen1Driver(iccID))
	job = w.read("R1", "download")
	w.wait(job)
	w.f.holdOn(0xD6)
	w.commit(job, w.clk.Now())
	w.waitState(job, "writing")
	w.clk.add(10*time.Minute + time.Second)
	w.s.sweep()
	w.s.mu.Lock()
	kept = w.s.job != nil && w.s.job.state == "writing" && w.s.job.file != nil
	w.s.mu.Unlock()
	if !kept {
		t.Fatal("задача в writing стёрта")
	}
	w.s.mu.Lock()
	idle := w.s.idle
	w.s.mu.Unlock()
	w.f.release()
	<-idle // запись закончилась (по HTTP задача уже 404: 10 мин от готовности истекли)
	w.s.mu.Lock()
	st := w.s.job.state
	w.s.mu.Unlock()
	if st != "written" {
		t.Fatalf("запись: %s", st)
	}

	// New запускает очистку и отдаёт рабочий обработчик; Done останавливает её
	stop := make(chan struct{})
	defer close(stop)
	r := httptest.NewRequest("GET", "/v1/status", nil)
	r.Host, r.Header["Origin"] = host, []string{originA}
	rec := httptest.NewRecorder()
	New(Config{Origins: []string{originA}, PCSC: newFake(), Now: e.clk.Now, Done: stop}).ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("New: %d", rec.Code)
	}
}
