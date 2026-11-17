package sim

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

type statsQuery struct {
	user     string
	expires  time.Time
	interval time.Duration
	running  bool
	active   int
	ttl      time.Duration
	done     chan struct{}
}

// The controller keeps its listener open when it retires a simulator, so
// attached requests need cancellation independent of their HTTP contexts.
func (s *restSessions) closeStats() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statsClosed = true
	for id, q := range s.stats {
		close(q.done)
		delete(s.stats, id)
	}
}

func vapiStruct(name string, fields map[string]any) any {
	return map[string]any{"STRUCTURE": map[string]any{name: fields}}
}
func vapiFields(raw any) map[string]any {
	obj, _ := raw.(map[string]any)
	structures, _ := obj["STRUCTURE"].(map[string]any)
	if len(structures) != 1 {
		return nil
	}
	for _, value := range structures {
		fields, _ := value.(map[string]any)
		return fields
	}
	return nil
}
func optionalValue(raw any) any {
	if v, ok := raw.(map[string]any); ok {
		return v["OPTIONAL"]
	}
	return raw
}

// The Operations vStats spec asks for GPU device and VM counters. This
// topology has no GPU devices, so each sample has no snapshots. A live
// query and stream still have the same lifecycle as a real vCenter.
func (s *restSessions) statsRPC(w http.ResponseWriter, r *http.Request, release func(), id json.RawMessage, service, operation, user string, input any) (any, string, bool) {
	fields := vapiFields(input)
	if service == "com.vmware.stats.provider.queries" && operation == "create" {
		spec := vapiFields(fields["spec"])
		instances, ok := spec["instances"].([]any)
		if optionalValue(spec["view"]) != "vstats" || !ok || len(instances) == 0 {
			return nil, "invalid_argument", false
		}
		for _, raw := range instances {
			instance := vapiFields(raw)
			counter, _ := optionalValue(instance["counter"]).(string)
			if !gpuCounter(counter) {
				return nil, "not_found", false
			}
		}
		qid := uuid.NewString()
		ttl := 600 * time.Second
		if seconds, ok := optionalValue(spec["time_to_live"]).(float64); ok && seconds > 0 && seconds <= 86400 {
			ttl = time.Duration(seconds) * time.Second
		}
		s.mu.Lock()
		if s.statsClosed {
			s.mu.Unlock()
			return nil, "service_unavailable", false
		}
		if s.stats == nil {
			s.stats = map[string]*statsQuery{}
		}
		for key, q := range s.stats {
			if q.active == 0 && time.Now().After(q.expires) {
				close(q.done)
				delete(s.stats, key)
			}
		}
		if len(s.stats) >= 1024 {
			s.mu.Unlock()
			return nil, "unable_to_allocate_resource", false
		}
		s.stats[qid] = &statsQuery{user: user, expires: time.Now().Add(ttl), interval: 20 * time.Second, ttl: ttl, done: make(chan struct{})}
		s.mu.Unlock()
		return vapiStruct("com.vmware.stats.provider.queries.create_result", map[string]any{"query": qid, "messages": []any{}}), "", false
	}
	qid, _ := fields["query"].(string)
	s.mu.Lock()
	if s.statsClosed {
		s.mu.Unlock()
		return nil, "service_unavailable", false
	}
	q := s.stats[qid]
	if q == nil || q.user != user || (q.active == 0 && time.Now().After(q.expires)) {
		s.mu.Unlock()
		return nil, "not_found", false
	}
	switch service + "/" + operation {
	case "com.vmware.stats.provider.queries/delete":
		close(q.done)
		delete(s.stats, qid)
		s.mu.Unlock()
		return nil, "", false
	case "com.vmware.stats.provider.stream/create":
		if q.running {
			s.mu.Unlock()
			return nil, "already_exists", false
		}
		spec := vapiFields(fields["spec"])
		if interval, ok := optionalValue(spec["interval"]).(float64); ok && interval >= 1 && interval <= 3600 {
			q.interval = time.Duration(interval) * time.Second
		}
		q.running = true
		s.mu.Unlock()
		return nil, "", false
	case "com.vmware.stats.provider.stream/delete":
		close(q.done)
		q.done = make(chan struct{})
		q.running = false
		s.mu.Unlock()
		return nil, "", false
	case "com.vmware.stats.provider.stream/attach":
		if !q.running {
			s.mu.Unlock()
			return nil, "not_allowed_in_current_state", false
		}
		interval, done := q.interval, q.done
		q.active++
		s.mu.Unlock()
		defer func() { s.mu.Lock(); q.active--; q.expires = time.Now().Add(q.ttl); s.mu.Unlock() }()
		release()
		s.writeStatsStream(w, r, id, qid, interval, done)
		return nil, "", true
	default:
		s.mu.Unlock()
		return nil, "operation_not_found", false
	}
}
func (s *restSessions) writeStatsStream(w http.ResponseWriter, r *http.Request, id json.RawMessage, qid string, interval time.Duration, done <-chan struct{}) {
	w.Header().Set("Content-Type", "application/vnd.vmware.vapi.stream.json")
	w.WriteHeader(http.StatusOK)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	flush := http.NewResponseController(w)
	for {
		output := vapiStruct("com.vmware.stats.provider.result", map[string]any{"query": qid, "timestamp": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), "snapshots": []any{}, "messages": []any{}})
		frame, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"output": output}})
		// vAPI stream frames use chunk framing inside the HTTP response body.
		if _, err := fmt.Fprintf(w, "%x\r\n%s\r\n", len(frame), frame); err != nil {
			return
		}
		if flush.Flush() != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-done:
			return
		case <-ticker.C:
		}
	}
}

// The GPU counters in Operations 9.1.1's default vStats query spec.
func gpuCounter(counter string) bool {
	switch counter {
	case "gpu.compute.clock.used.DEVICE", "gpu.compute.clock.limit.DEVICE", "gpu.compute.clock.demand.DEVICE", "gpu.mem.demand.DEVICE", "gpu.slowdown.temperature.DEVICE", "gpu.shutdown.temperature.DEVICE", "gpu.power.limit.DEVICE", "gpu.mem.reserved.VM.DEVICE", "gpu.mem.used.VM.DEVICE", "gpu.compute.utilization.VM.DEVICE", "gpu.mem.utilization.VM.DEVICE", "gpu.encode.utilization.VM.DEVICE", "gpu.decode.utilization.VM.DEVICE":
		return true
	}
	return false
}
