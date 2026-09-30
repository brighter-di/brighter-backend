package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/brighter-di/backend/internal/backtestengine"
)

func TestCustomBacktestInsufficientHistory(t *testing.T) {
	err := customBacktestRejection(&backtestengine.Error{Status: 422, Detail: `[{"code":"insufficient-ticker-data","meta":{"min_lookback":90,"series_length":46,"ticker":"EQUITIES::SKHY//USD","user_id":"private"}}]`})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "INSUFFICIENT_HISTORY" || failure.Status != 422 {
		t.Fatalf("want insufficient history, got %v", err)
	}
	for _, want := range []string{"SKHY", "46", "90", "Reduce its lookback"} {
		if !strings.Contains(failure.Message, want) {
			t.Fatalf("missing %s: %s", want, failure.Message)
		}
	}
	if strings.Contains(failure.Message, "private") || strings.Contains(failure.Message, "EQUITIES::") {
		t.Fatalf("provider metadata leaked: %s", failure.Message)
	}
}

func TestCustomBacktestRejectionFallback(t *testing.T) {
	for _, detail := range []string{"bad request", `[{"code":"other"}]`, `[{"code":"insufficient-ticker-data","meta":{"ticker":"UNKNOWN","min_lookback":90}}]`} {
		err := customBacktestRejection(&backtestengine.Error{Status: 422, Detail: detail})
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "STRATEGY_NOT_BACKTESTABLE" {
			t.Fatalf("want generic rejection, got %v", err)
		}
	}
}
