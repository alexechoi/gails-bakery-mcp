package tunnel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/alexechoi/gails-bakery-mcp/internal/adyen"
)

// HybridInput carries the initiate action for the native-3DS2 hybrid flow.
type HybridInput struct {
	OrderUUID       string
	TransactionUUID string
	Store           string
	Amount          float64
	Action          map[string]any // the IdentifyShopper (fingerprint) action from initiate
}

// PrepareHybrid stores an initiate action and returns a pay URL that drives the
// full native-3DS2 flow in the browser (3DS method + challenge, both form-POST
// to the ACS so no CORS/X-Frame-Options wall), finalising server-side.
func (m *Manager) PrepareHybrid(ctx context.Context, in HybridInput) (map[string]any, error) {
	base, err := m.ensure(ctx)
	if err != nil {
		return nil, err
	}
	tok, _ := in.Action["token"].(string)
	tk := decodeToken(tok)
	fp := deepFindStr(in.Action, "paymentData")
	if fp == "" {
		return nil, fmt.Errorf("no fingerprint paymentData in action")
	}
	m.recsMu.Lock()
	m.seq++
	id := "h" + strconv.Itoa(m.seq) + "-" + strconv.FormatInt(time.Now().UnixNano()%1_000_000, 36)
	m.recs[id] = &record{
		Order: in.OrderUUID, Txn: in.TransactionUUID, Store: in.Store, Amount: in.Amount,
		FPPaymentData: fp,
		MethodURL:     str(tk["threeDSMethodUrl"], tk["threeDSMethodURL"]),
		MethodNotif:   str(tk["threeDSMethodNotificationURL"]),
		ServerTransID: str(tk["threeDSServerTransID"]),
	}
	m.recsMu.Unlock()
	return map[string]any{
		"id":               id,
		"pay_url":          base + "/hpay/" + id,
		"pay_url_markdown": "[Approve your bank's 3-D Secure](" + base + "/hpay/" + id + ")",
		"status_url":       base + "/status/" + id,
	}, nil
}

// handleHybridMethod: after the browser runs the 3DS method, submit the
// fingerprint (threeDSCompInd:Y) server-side and return the challenge acsURL+creq.
func (m *Manager) handleHybridMethod(w http.ResponseWriter, r *http.Request) {
	rec := m.get(strings.TrimPrefix(r.URL.Path, "/hmethod/"))
	if rec == nil {
		writeJSON(w, 404, map[string]any{"error": "unknown"})
		return
	}
	raw, err := adyen.SubmitFingerprint(r.Context(), nil, m.clientKey, rec.FPPaymentData, "Y")
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": err.Error()})
		return
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	act, _ := out["action"].(map[string]any)
	if act == nil || act["subtype"] != "challenge" {
		// no challenge → maybe frictionless; return the result for confirm
		writeJSON(w, 200, map[string]any{"frictionless": true})
		return
	}
	tk := decodeToken(str(act["token"]))
	rec.mu.Lock()
	rec.AuthToken = str(act["authorisationToken"])
	rec.ACS = str(tk["acsURL"])
	rec.CReq = base64.RawURLEncoding.EncodeToString(mustJSON(map[string]any{
		"threeDSServerTransID": tk["threeDSServerTransID"], "acsTransID": tk["acsTransID"],
		"messageVersion": tk["messageVersion"], "messageType": "CReq", "challengeWindowSize": "05",
	}))
	rec.mu.Unlock()
	writeJSON(w, 200, map[string]any{"acs": rec.ACS, "creq": rec.CReq})
}

// handleHybridFinal: assemble the threeDSResult and confirm the order. Safe to
// call repeatedly (polling): before the shopper approves, /confirm just fails;
// once it succeeds the result is cached and returned without re-confirming.
func (m *Manager) handleHybridFinal(w http.ResponseWriter, r *http.Request) {
	rec := m.get(strings.TrimPrefix(r.URL.Path, "/hfinal/"))
	if rec == nil {
		writeJSON(w, 404, map[string]any{"error": "unknown"})
		return
	}
	rec.mu.Lock()
	if rec.Done {
		res := rec.Result
		rec.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true, "result": res})
		return
	}
	rec.mu.Unlock()

	tds := base64.StdEncoding.EncodeToString(mustJSON(map[string]any{
		"transStatus": "Y", "authorisationToken": rec.AuthToken,
	}))
	res, err := m.confirm(context.Background(), rec.Order, rec.Txn, rec.Store, map[string]any{"threeDSResult": tds})
	if err != nil {
		// Not approved yet (or genuine failure) — leave Done=false so polling retries.
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	rec.mu.Lock()
	rec.Done = true
	rec.Result = res
	rec.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true, "result": res})
}

func (m *Manager) handleHybridPay(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/hpay/")
	rec := m.get(id)
	if rec == nil {
		http.Error(w, "unknown payment", http.StatusNotFound)
		return
	}
	methodData := base64.RawURLEncoding.EncodeToString(mustJSON(map[string]any{
		"threeDSServerTransID": rec.ServerTransID, "threeDSMethodNotificationURL": rec.MethodNotif,
	}))
	page := hybridPage
	page = strings.ReplaceAll(page, "__ID__", id)
	page = strings.ReplaceAll(page, "__METHOD_URL__", jsStr(rec.MethodURL))
	page = strings.ReplaceAll(page, "__METHOD_DATA__", methodData)
	page = strings.ReplaceAll(page, "__AMOUNT__", trimFloat(rec.Amount))
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(page))
}

// --- helpers ---

func decodeToken(tok string) map[string]any {
	if tok == "" {
		return map[string]any{}
	}
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.StdEncoding, base64.RawStdEncoding} {
		if b, err := enc.DecodeString(tok); err == nil {
			var m map[string]any
			if json.Unmarshal(b, &m) == nil && len(m) > 0 {
				return m
			}
		}
	}
	return map[string]any{}
}

func deepFindStr(o any, key string) string {
	switch v := o.(type) {
	case map[string]any:
		if s, ok := v[key].(string); ok && s != "" {
			return s
		}
		for _, x := range v {
			if r := deepFindStr(x, key); r != "" {
				return r
			}
		}
	case []any:
		for _, x := range v {
			if r := deepFindStr(x, key); r != "" {
				return r
			}
		}
	}
	return ""
}

func str(vals ...any) string {
	for _, v := range vals {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func jsStr(s string) string { b, _ := json.Marshal(s); return string(b) }

// hybridPage: runs the 3DS method (hidden iframe), fetches the challenge, renders
// it in an iframe, and AUTO-FINALISES when it hears the completion postMessage
// from Adyen (no button needed). Falls back to a manual button after a timeout.
const hybridPage = `<!doctype html><html><head><meta charset=utf-8>
<meta name=viewport content="width=device-width,initial-scale=1"><title>Approve payment</title></head>
<body style="font-family:-apple-system,system-ui,sans-serif;max-width:560px;margin:30px auto;padding:0 14px">
<h3>Approve your Gail's payment (£__AMOUNT__)</h3>
<div id=s>Establishing a secure session with your bank…</div>
<iframe name=m style="display:none"></iframe>
<div id=cwrap></div>
<button id=done style="display:none;font-size:16px;padding:10px 16px;margin-top:14px">I've approved »</button>
<script>
const ID="__ID__", MU=__METHOD_URL__, S=document.getElementById('s');
let done=false, pollId=null;
function postForm(action,fields,target){const f=document.createElement('form');f.method='POST';f.action=action;f.target=target;for(const k in fields){const i=document.createElement('input');i.type='hidden';i.name=k;i.value=fields[k];f.appendChild(i);}document.body.appendChild(f);f.submit();}
// tryFinal is safe to call repeatedly: it only marks done on a successful confirm.
async function tryFinal(){ if(done) return true;
  try{const j=await(await fetch('/hfinal/'+ID,{method:'POST'})).json();
    if(j.ok){ done=true; if(pollId) clearInterval(pollId); S.textContent='✅ Payment confirmed — order placed!'; return true; }}
  catch(e){}
  return false; }
// fast path: the bank/Adyen posts a message when the challenge completes
window.addEventListener('message',e=>{ const o=String(e.origin); if(o.indexOf('adyen.com')>=0||o.indexOf('arcot.com')>=0){ setTimeout(tryFinal,800); } });
// 1) 3DS method (hidden iframe)
if(MU) postForm(MU,{threeDSMethodData:"__METHOD_DATA__"},'m');
// 2) fetch challenge, then render it and auto-finish
setTimeout(async()=>{
  S.textContent='Contacting your bank for verification…';
  let d; try{ d=await(await fetch('/hmethod/'+ID,{method:'POST'})).json(); }catch(e){ S.textContent='Error: '+e; return; }
  if(d.frictionless){ tryFinal(); return; }
  if(d.error){ S.textContent='Error: '+d.error; return; }
  S.textContent='Approve the request from your bank (in the box below or your banking app) — this finishes automatically.';
  const ifr=document.createElement('iframe'); ifr.name='c'; ifr.style="width:100%;height:430px;border:1px solid #ccc;border-radius:8px"; document.getElementById('cwrap').appendChild(ifr);
  postForm(d.acs,{creq:d.creq},'c');
  // auto-poll for completion (fast path is the postMessage above)
  pollId=setInterval(tryFinal,5000);
  // reveal a manual button only as a last resort if auto-finish hasn't happened
  setTimeout(()=>{ if(!done) document.getElementById('done').style.display='inline-block'; },75000);
},3500);
document.getElementById('done').onclick=()=>{ S.textContent='Finalising…'; tryFinal(); };
</script></body></html>`
