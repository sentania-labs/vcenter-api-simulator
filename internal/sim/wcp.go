package sim

import "net/http"

// A vCenter without workload management still exposes the WCP JSON-RPC
// listener. Operations probes it with GET, for which 9.1.1 returns this error.
func serveWCP(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return true
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusMethodNotAllowed)
	_, _ = w.Write([]byte("Invalid request method\n" + `{"error":{"code":-32600,"data":"Error deserializing jsonrpc request","message":"Invalid Request"},"jsonrpc":"2.0"}`))
	return true
}
