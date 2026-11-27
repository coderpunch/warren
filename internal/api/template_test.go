package api

import (
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/h3xcat/warren/internal/policy"
)

// Run with -race. Requests at the same time read the storage size of one
// policy. 8Gi is a size for which the parser keeps no text.
func TestTemplateRequestsAtTheSameTime(t *testing.T) {
	set, err := policy.Parse([]byte("policies:\n  - {id: a, name: A, scope: user, storage: {size: 8Gi}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{Policies: policy.NewStore(set), DefaultImage: "registry.example/default:1"}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/terminals/templates/a", nil))
			if rec.Code != 200 {
				t.Errorf("status = %d", rec.Code)
			}
		}()
	}
	close(start)
	wg.Wait()
}

// Open WebUI shows the CPU limit as the text that warren sends.
func TestTemplateCPULimitIsAShortDecimalNumber(t *testing.T) {
	for cpu, want := range map[string]string{
		"700m": "0.7", "0.3": "0.3", "600m": "0.6", "1100m": "1.1", "500m": "0.5", "2": "2", "1m": "0.001", "1500m": "1.5",
	} {
		set, err := policy.Parse([]byte("policies:\n  - {id: a, name: A, scope: user, resources: {limits: {cpu: " + cpu + "}}}\n"))
		if err != nil {
			t.Fatal(err)
		}
		h := &Handler{Policies: set}
		_, out := do(h, "GET", base+"a", "")
		data, _ := out["data"].(map[string]any)
		if data["cpu_limit"] != want {
			t.Errorf("cpu %s: cpu_limit = %v, want %s", cpu, data["cpu_limit"], want)
		}
	}
}
