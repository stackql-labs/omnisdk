package omnisdk_test

import (
	"context"
	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"testing"
	"testing/synctest"
	"time"
)

func TestSleepExchange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		planned, err := omnisdk.NewSleep(time.Second)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := planned.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(started); elapsed != time.Second {
			t.Fatalf("slept %s, want 1s", elapsed)
		}
	})
}
func TestSleepExchangeCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		ctx, cancel := context.WithCancel(context.Background())
		planned, err := omnisdk.NewSleep(time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := planned.Open(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		cancel()
		if rows.Next() {
			t.Fatal("cancelled sleep emitted a row")
		}
		synctest.Wait()
		if elapsed := time.Since(started); elapsed != 0 {
			t.Fatalf("cancellation waited %s", elapsed)
		}
	})
}
func TestSleepExchangeRejectsNonpositiveDuration(t *testing.T) {
	for _, duration := range []time.Duration{0, -time.Second} {
		if _, err := omnisdk.NewSleep(duration); err == nil {
			t.Fatalf("accepted %s", duration)
		}
	}
}
