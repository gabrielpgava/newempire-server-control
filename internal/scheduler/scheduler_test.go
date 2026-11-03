package scheduler

import (
	"testing"
	"time"
)

func TestDSTAndMaintenanceWindows(t *testing.T) {
	s := Schedule{InstanceID: "0123456789abcdef0123456789abcdef", Kind: "backup", Hour: 2, Minute: 30, Weekdays: []int{0, 1, 2, 3, 4, 5, 6}, Timezone: "America/New_York"}
	after := time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)
	next, err := Next(s, after)
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation(s.Timezone)
	if next.In(loc).Day() != 9 || next.In(loc).Hour() != 2 {
		t.Fatalf("nonexistent DST time was not skipped: %s", next)
	}
	inside, err := InWindow(time.Date(2026, 1, 1, 23, 30, 0, 0, time.UTC), "UTC", "23:00", "02:00")
	if err != nil || !inside {
		t.Fatal("overnight window failed")
	}
	inside, err = InWindow(time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC), "UTC", "23:00", "02:00")
	if err != nil || inside {
		t.Fatal("window end must be exclusive")
	}
}
