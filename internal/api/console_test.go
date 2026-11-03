package api

import (
	"errors"
	"strings"
	"testing"
)

func TestConsoleAndPlayerJobsRedactKnownRCONSecret(t *testing.T) {
	h := newHarness(t, nil)
	h.runtime.mu.Lock()
	h.runtime.ready = true
	h.runtime.mu.Unlock()
	base := "/instances/" + h.instance.ID
	for _, request := range []struct {
		path string
		body map[string]string
	}{
		{base + "/console", map[string]string{"command": "list"}},
		{base + "/players", map[string]string{"action": "kick", "player": "Tester"}},
	} {
		for _, failed := range []bool{false, true} {
			h.runtime.mu.Lock()
			h.runtime.commandErr = nil
			if failed {
				h.runtime.commandErr = errors.New("controlled error " + h.instance.RCONSecret)
			}
			h.runtime.mu.Unlock()
			job := h.queue(t, request.path, request.body)
			if !failed {
				mustSucceed(t, job)
			} else if job.State != "failed" {
				t.Fatal("failed RCON command reported success")
			}
			if strings.Contains(string(job.Result), h.instance.RCONSecret) || strings.Contains(job.Error, h.instance.RCONSecret) {
				t.Fatal("RCON credential persisted in job result or error")
			}
		}
	}
}
