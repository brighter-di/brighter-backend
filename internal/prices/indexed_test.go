package prices

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIndexedHistoryKeepsRawClosesAndFiltersWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/assets/SPY/prices" || r.URL.Query().Get("adjusted") != "false" || r.URL.Query().Get("end") != "" {
			t.Error("wrong indexed price request")
		}
		fmt.Fprint(w, `{"symbol":"SPY","adjusted":false,"prices":[{"date":"2025-01-06","close":102},{"date":"2025-01-03","close":101},{"date":"2025-01-02","close":100}]}`)
	}))
	defer srv.Close()
	from := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	to := time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC)
	client := New(Options{IndexedURL: srv.URL})
	bars, err := client.History(context.Background(), "SPY", ETF, from, to)
	if err != nil || len(bars) != 2 || bars[0].Close != 100 || bars[1].Close != 101 {
		t.Fatalf("bad indexed response: %+v %v", bars, err)
	}
}
func TestIndexedHistoryRejectsWrongAdjustmentAndInvalidRows(t *testing.T) {
	for _, body := range []string{`{"symbol":"SPY","adjusted":true,"prices":[]}`, `{"symbol":"QQQ","prices":[]}`, `{"symbol":"SPY","prices":[{"date":"2025-01-02","close":100},{"date":"2025-01-02","close":100}]}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		client := New(Options{IndexedURL: srv.URL})
		_, err := client.History(context.Background(), "SPY", ETF, time.Now().AddDate(-1, 0, 0), time.Now())
		srv.Close()
		if err == nil {
			t.Fatal("invalid indexer response accepted")
		}
	}
}
