package tunnel

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

func (m *Manager) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/pay/", m.handlePay)
	mux.HandleFunc("/complete/", m.handleComplete)
	mux.HandleFunc("/status/", m.handleStatus)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "service": "gails-3ds-challenge"})
	})
	return mux
}

func (m *Manager) get(id string) *record {
	m.recsMu.Lock()
	defer m.recsMu.Unlock()
	return m.recs[id]
}

func (m *Manager) handlePay(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/pay/")
	rec := m.get(id)
	if rec == nil {
		http.Error(w, "unknown payment", http.StatusNotFound)
		return
	}
	actionJSON, _ := json.Marshal(rec.Action)
	amount := ""
	if rec.Amount > 0 {
		amount = trimFloat(rec.Amount)
	}
	html := payPage
	html = strings.ReplaceAll(html, "__ACTION__", string(actionJSON))
	html = strings.ReplaceAll(html, "__ID__", id)
	html = strings.ReplaceAll(html, "__CLIENT_KEY__", m.clientKey)
	html = strings.ReplaceAll(html, "__AMOUNT__", amount)
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(html))
}

func (m *Manager) handleComplete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/complete/")
	rec := m.get(id)
	if rec == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown id"})
		return
	}
	var data map[string]any
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad body"})
		return
	}
	details, _ := data["details"].(map[string]any)
	if details == nil {
		details = data // adyen-web posts {details:{...}}; tolerate a flat body too
	}
	res, err := m.confirm(context.Background(), rec.Order, rec.Txn, rec.Store, details)
	rec.mu.Lock()
	rec.Done = true
	if err != nil {
		rec.Result = map[string]any{"error": err.Error()}
	} else {
		rec.Result = res
	}
	rec.mu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res})
}

func (m *Manager) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/status/")
	rec := m.get(id)
	if rec == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown id"})
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"pending": !rec.Done, "result": rec.Result})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func trimFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// payPage loads Adyen Web and renders the bank's 3DS challenge for the action,
// then POSTs the result to /complete/{id} which confirms the order.
const payPage = `<!doctype html><html><head><meta charset=utf-8>
<meta name=viewport content="width=device-width,initial-scale=1">
<title>Gail's — verify payment</title>
<link rel=stylesheet href="https://checkoutshopper-live.adyen.com/checkoutshopper/sdk/5.65.0/adyen.css">
<script src="https://checkoutshopper-live.adyen.com/checkoutshopper/sdk/5.65.0/adyen.js"></script>
<style>body{font-family:-apple-system,system-ui,sans-serif;max-width:480px;margin:24px auto;padding:0 16px}#status{margin-top:16px;padding:12px;border-radius:8px;background:#f4f4f5}</style>
</head><body>
<h3>Verify your payment</h3>
<p>Amount: £__AMOUNT__ — complete your bank's 3-D Secure check below.</p>
<div id=container></div>
<div id=status>Loading your bank's verification…</div>
<script>
const ACTION = __ACTION__;
const ID = "__ID__";
const S = document.getElementById('status');
(async () => {
  try {
    const checkout = await AdyenCheckout({
      environment: 'live',
      clientKey: "__CLIENT_KEY__",
      analytics: { enabled: false },
      onAdditionalDetails: (state) => {
        S.textContent = 'Verified — finalising your order…';
        fetch('/complete/' + ID, {method:'POST', headers:{'content-type':'application/json'}, body: JSON.stringify(state.data)})
          .then(r => r.json())
          .then(res => { S.textContent = res.ok ? '✅ Payment confirmed. You can close this tab.' : ('⚠️ Confirm failed: ' + JSON.stringify(res).slice(0,300)); })
          .catch(e => { S.textContent = 'Error finalising: ' + e; });
      },
      onError: (e) => { S.textContent = 'Error: ' + (e && e.message ? e.message : e); }
    });
    checkout.createFromAction(ACTION, { challengeWindowSize: '05' }).mount('#container');
    S.textContent = "Follow your bank's prompt above (you may need to approve in your banking app).";
  } catch (e) { S.textContent = 'Failed to start verification: ' + e; }
})();
</script>
</body></html>`
