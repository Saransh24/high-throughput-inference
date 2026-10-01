package loadgen

import (
	"crypto/hmac"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"hti/internal/api"
	"hti/internal/service"
)

// Delivery is one callback attempt as seen by the receiver.
type Delivery struct {
	BatchID     string           `json:"batch_id"`
	DeliveryID  string           `json:"delivery_id"`
	Attempt     int              `json:"attempt"`
	At          time.Time        `json:"at"`
	Responded   int              `json:"responded"` // status code the receiver answered with
	SignatureOK bool             `json:"signature_ok"`
	Body        api.BatchSummary `json:"body"`
}

// Sink is a callback receiver for tests. It can reject the first N
// deliveries of every batch to exercise the service's retry logic.
type Sink struct {
	URL string

	mu         sync.Mutex
	failFirst  int
	secret     string
	seen       map[string]int
	deliveries []Delivery
	srv        *http.Server
}

func StartSink(addr string, failFirst int, secret string) (*Sink, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Sink{URL: "http://" + ln.Addr().String() + "/callback", failFirst: failFirst, secret: secret, seen: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /callback", s.handle)
	s.srv = &http.Server{Handler: mux}
	go s.srv.Serve(ln)
	return s, nil
}

func (s *Sink) Close() { s.srv.Close() }

func (s *Sink) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	d := Delivery{At: time.Now(), DeliveryID: r.Header.Get("X-HTI-Delivery-Id")}
	d.Attempt, _ = strconv.Atoi(r.Header.Get("X-HTI-Attempt"))
	json.Unmarshal(body, &d.Body)
	d.BatchID = d.Body.BatchID
	want := service.Sign(s.secret, r.Header.Get("X-HTI-Timestamp"), body)
	got := strings.TrimPrefix(r.Header.Get("X-HTI-Signature"), "sha256=")
	d.SignatureOK = hmac.Equal([]byte(want), []byte(got))

	s.mu.Lock()
	n := s.seen[d.BatchID]
	s.seen[d.BatchID] = n + 1
	d.Responded = http.StatusOK
	if n < s.failFirst {
		d.Responded = http.StatusServiceUnavailable
	}
	s.deliveries = append(s.deliveries, d)
	s.mu.Unlock()
	w.WriteHeader(d.Responded)
}

// For returns every delivery attempt received for one batch, in order.
func (s *Sink) For(batchID string) []Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Delivery
	for _, d := range s.deliveries {
		if d.BatchID == batchID {
			out = append(out, d)
		}
	}
	return out
}
